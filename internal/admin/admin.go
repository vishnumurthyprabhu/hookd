// Package admin serves Hookd's admin UI under /admin: server-rendered pages
// and plain HTML forms, embedded in the binary. The UI has no sign-in, so
// every page carries a notice saying so (R4), and every route is wrapped in
// http.CrossOriginProtection so forms cannot be sent from another site (N5).
package admin

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"hookd/internal/settings"
	"hookd/internal/store"
)

// pageSize is the number of rows a list page shows.
const pageSize = 50

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Handler is the admin UI. Mount it for "/" and "/admin/…" (api.WithAdmin).
type Handler struct {
	store    store.Store
	settings *settings.Manager
	log      *slog.Logger
	pages    map[string]*template.Template
	root     http.Handler
}

// Option configures a Handler.
type Option func(*Handler)

// WithSettings serves the settings and health page, which shows and changes
// the settings of m. Without it the page is not found.
func WithSettings(m *settings.Manager) Option {
	return func(h *Handler) { h.settings = m }
}

// New returns the admin UI on st, logging to log.
func New(st store.Store, log *slog.Logger, opts ...Option) *Handler {
	h := &Handler{store: st, log: log, pages: parsePages()}
	for _, o := range opts {
		o(h)
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	mux.Handle("GET /admin/static/", http.StripPrefix("/admin/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /admin/{$}", h.listAccounts)
	mux.HandleFunc("POST /admin/accounts", h.createAccount)
	mux.HandleFunc("GET /admin/accounts/{acct}", h.showAccount)
	mux.HandleFunc("POST /admin/accounts/{acct}/keys", h.createKey)
	mux.HandleFunc("GET /admin/accounts/{acct}/keys/{key}/revoke", h.confirmRevokeKey)
	mux.HandleFunc("POST /admin/accounts/{acct}/keys/{key}/revoke", h.revokeKey)
	if h.settings != nil {
		mux.HandleFunc("GET /admin/settings", h.showSettings)
		mux.HandleFunc("POST /admin/settings/{name}", h.saveSetting)
		mux.HandleFunc("POST /admin/settings/{name}/reset", h.resetSetting)
	}
	mux.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) { h.notFound(w, r) })

	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.renderError(w, r, http.StatusForbidden, "Request refused",
			"This form was sent from another website, so Hookd did not act on it. Open the admin UI directly and send the form from there.")
	}))
	h.root = cop.Handler(securityHeaders(mux))
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.root.ServeHTTP(w, r)
}

// securityHeaders keeps pages from being framed by other sites and limits
// them to the UI's own scripts and styles.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

var funcs = template.FuncMap{
	"time": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") },
}

// parsePages parses each page template together with the layout.
func parsePages() map[string]*template.Template {
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	pages := map[string]*template.Template{}
	for _, name := range names {
		if name == "templates/layout.html" {
			continue
		}
		t := template.Must(template.New("layout.html").Funcs(funcs).ParseFS(templateFS, "templates/layout.html", name))
		pages[strings.TrimSuffix(strings.TrimPrefix(name, "templates/"), ".html")] = t
	}
	return pages
}

// page is what the layout needs from every page.
type page struct {
	Title string
	Data  any
}

// render writes the named page with the given status. The page is rendered
// into a buffer first, so a template error becomes a plain 500.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	var buf bytes.Buffer
	if err := h.pages[name].Execute(&buf, page{Title: title, Data: data}); err != nil {
		h.log.ErrorContext(r.Context(), "render admin page", "page", name, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

type errorData struct {
	Message string
}

// renderError shows the shared error page.
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	h.render(w, r, status, "error", title, errorData{Message: message})
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, http.StatusNotFound, "Not found", "There is nothing at this address. It may have been deleted, or the link may be wrong.")
}

// storeError logs err and shows the error page for a failed database call.
func (h *Handler) storeError(w http.ResponseWriter, r *http.Request, err error) {
	h.log.ErrorContext(r.Context(), "admin store call", "path", r.URL.Path, "err", err)
	h.renderError(w, r, http.StatusServiceUnavailable, "Database unavailable",
		"Hookd could not read from or write to its database. Check that the database is running and reachable, then try again.")
}
