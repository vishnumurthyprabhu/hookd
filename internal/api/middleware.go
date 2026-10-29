package api

import (
	"context"
	"net/http"
	"time"
)

// requestInfo collects what the request log line needs from inner handlers.
type requestInfo struct {
	accountID string
}

type requestInfoKey struct{}

func requestInfoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}

// logRequests logs one line per request. It logs the method, the path
// without its query, the status, the duration and the account, and never
// headers, so API keys stay out of the logs (N4).
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{}
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info)))

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.statusOrOK(),
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if info.accountID != "" {
			attrs = append(attrs, "account_id", info.accountID)
		}
		s.log.InfoContext(r.Context(), "request", attrs...)
	})
}

// recoverPanics turns a panicking handler into a 500 response.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			s.log.ErrorContext(r.Context(), "handler panic", "method", r.Method, "path", r.URL.Path, "panic", v)
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
		}()
		next.ServeHTTP(w, r)
	})
}

// statusWriter records the status code written through it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) statusOrOK() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
