package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"hookd/internal/store"
)

const (
	// healthTimeout bounds the database check of GET /healthz.
	healthTimeout = 2 * time.Second
	// shutdownTimeout is how long in-flight requests get to finish once
	// shutdown starts.
	shutdownTimeout = 10 * time.Second
)

// defaultMaxPayloadBytes is the event payload limit when WithMaxPayloadBytes
// is not given (R9).
const defaultMaxPayloadBytes = 256 << 10

// Server is Hookd's HTTP API.
type Server struct {
	store           store.Store
	log             *slog.Logger
	mux             *http.ServeMux
	maxPayloadBytes func() int64
	wake            chan<- struct{}
	admin           http.Handler
}

// Option changes how New sets up a Server.
type Option func(*Server)

// WithMaxPayloadBytes sets the largest event payload POST /v1/events
// accepts (HOOKD_MAX_PAYLOAD_BYTES). n must be positive.
func WithMaxPayloadBytes(n int64) Option {
	return WithMaxPayloadBytesFunc(func() int64 { return n })
}

// WithMaxPayloadBytesFunc makes POST /v1/events read the largest payload it
// accepts from max for each request, so a limit changed while Hookd runs
// applies to the next request. max must return a positive value.
func WithMaxPayloadBytesFunc(max func() int64) Option {
	return func(s *Server) { s.maxPayloadBytes = max }
}

// WithWake gives the server a channel to signal after each new event is
// stored, so delivery workers can start at once. The send never blocks: if
// the channel is full, the signal is dropped, since a pending one already
// wakes the workers.
func WithWake(wake chan<- struct{}) Option {
	return func(s *Server) { s.wake = wake }
}

// WithAdmin serves the admin UI with h: "/" and every path under "/admin"
// go to h, inside the same request logging and panic recovery as the API.
// Without it those paths answer the JSON 404 like any unknown path.
func WithAdmin(h http.Handler) Option {
	return func(s *Server) { s.admin = h }
}

// New returns a Server that keeps its state in st and logs to log.
func New(st store.Store, log *slog.Logger, opts ...Option) *Server {
	s := &Server{store: st, log: log, mux: http.NewServeMux()}
	WithMaxPayloadBytes(defaultMaxPayloadBytes)(s)
	for _, opt := range opts {
		opt(s)
	}
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("POST /v1/endpoints", s.createEndpoint)
	s.mux.HandleFunc("GET /v1/endpoints", s.listEndpoints)
	s.mux.HandleFunc("GET /v1/endpoints/{id}", s.getEndpoint)
	s.mux.HandleFunc("PATCH /v1/endpoints/{id}", s.updateEndpoint)
	s.mux.HandleFunc("DELETE /v1/endpoints/{id}", s.deleteEndpoint)
	s.mux.HandleFunc("GET /v1/endpoints/{id}/secret", s.getEndpointSecret)
	s.mux.HandleFunc("GET /v1/endpoints/{id}/attempts", s.listEndpointAttempts)
	s.mux.HandleFunc("POST /v1/events", s.createEvent)
	s.mux.HandleFunc("GET /v1/events/{id}", s.getEvent)
	s.mux.HandleFunc("GET /v1/events/{id}/attempts", s.listEventAttempts)
	s.mux.HandleFunc("GET /v1/attempts/{id}", s.getAttempt)
	return s
}

// Handler returns the API's root handler. Every route under /v1 requires a
// valid API key; /healthz does not.
func (s *Server) Handler() http.Handler {
	return s.logRequests(s.recoverPanics(http.HandlerFunc(s.route)))
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") {
		s.authenticate(http.HandlerFunc(s.dispatch)).ServeHTTP(w, r)
		return
	}
	if s.admin != nil && isAdminPath(r.URL.Path) {
		s.admin.ServeHTTP(w, r)
		return
	}
	s.dispatch(w, r)
}

// isAdminPath reports whether path belongs to the admin UI.
func isAdminPath(path string) bool {
	return path == "/" || path == "/admin" || strings.HasPrefix(path, "/admin/")
}

// dispatch serves r with the mux, answering in JSON when no route matches.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	if h, pattern := s.mux.Handler(r); pattern == "" {
		// The mux would answer in plain text. Find out whether it would
		// say 404 or 405, and say it in JSON instead.
		rec := &headerRecorder{header: http.Header{}}
		h.ServeHTTP(rec, r)
		if rec.status == http.StatusMethodNotAllowed {
			w.Header().Set("Allow", rec.header.Get("Allow"))
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		writeError(w, http.StatusNotFound, CodeNotFound, "not found")
		return
	}
	s.mux.ServeHTTP(w, r)
}

// healthz answers 200 when the database can be reached and 503 otherwise.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.log.WarnContext(r.Context(), "health check failed", "err", err)
		writeError(w, http.StatusServiceUnavailable, CodeUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Serve answers requests on ln until ctx is done, then shuts down
// gracefully: it stops accepting connections and waits for in-flight
// requests to finish. It returns nil after a clean shutdown.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if serveErr := <-errc; !errors.Is(serveErr, http.ErrServerClosed) && err == nil {
		err = serveErr
	}
	return err
}

// headerRecorder captures the status and headers a handler writes, and
// drops the body.
type headerRecorder struct {
	header http.Header
	status int
}

func (h *headerRecorder) Header() http.Header         { return h.header }
func (h *headerRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (h *headerRecorder) WriteHeader(status int)      { h.status = status }

// maxDrainBytes is the most of an oversized request body that is read and
// thrown away after the 413 is decided.
const maxDrainBytes = 4 << 20

// readBody reads the request body if it is at most limit bytes long. If it
// is longer, readBody returns tooLarge and first reads and discards the rest
// of the body, up to maxDrainBytes. Closing a connection whose request body
// is still unread makes the kernel send RST, which can drop the 413 before
// a proxy such as nginx reads it; a drained connection is closed cleanly or
// kept alive. A body too long to drain closes the connection after the reply.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) (body []byte, tooLarge bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) <= limit {
		return body, false, nil
	}
	if n, err := io.Copy(io.Discard, io.LimitReader(r.Body, maxDrainBytes)); err != nil || n == maxDrainBytes {
		w.Header().Set("Connection", "close")
	}
	return nil, true, nil
}
