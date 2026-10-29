package api

import (
	"context"
	"errors"
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

// Server is Hookd's HTTP API.
type Server struct {
	store store.Store
	log   *slog.Logger
	mux   *http.ServeMux
}

// New returns a Server that keeps its state in st and logs to log.
func New(st store.Store, log *slog.Logger) *Server {
	s := &Server{store: st, log: log, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /healthz", s.healthz)
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
	s.dispatch(w, r)
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
