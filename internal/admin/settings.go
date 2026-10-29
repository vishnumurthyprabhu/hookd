package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"hookd/internal/config"
	"hookd/internal/settings"
)

// healthTimeout bounds the database calls of the health section, so the
// settings page still loads when the database hangs.
const healthTimeout = 3 * time.Second

// settingsData is the settings and health page (R22–R27).
type settingsData struct {
	Settings []settingRow
	// LoadError is the manager's last reload error, as lines of text.
	LoadError []string
	Health    health
}

// settingRow is one setting, with the form's input and error when a save or
// reset of that setting was refused.
type settingRow struct {
	settings.Setting
	Input string
	Error []string
}

// health is the health section (R27).
type health struct {
	Reachable bool
	PingError string
	DB        config.DB
	Postgres  bool
	// Pending lists the migrations not yet applied. PendingKnown is false
	// when they could not be read.
	Pending      []string
	PendingKnown bool
	// Counts is nil when the deliveries could not be counted.
	Counts map[string]int
	// WaitingRetries is the number of pending deliveries waiting for a
	// retry, or -1 when they could not be counted.
	WaitingRetries int
}

// showSettings shows every setting with its value and source, and the
// health section.
func (h *Handler) showSettings(w http.ResponseWriter, r *http.Request) {
	h.renderSettings(w, r, http.StatusOK, "", "", nil)
}

// renderSettings shows the settings page. If name is not empty, that
// setting's form shows input and the lines of msg.
func (h *Handler) renderSettings(w http.ResponseWriter, r *http.Request, status int, name, input string, msg []string) {
	var data settingsData
	for _, s := range h.settings.List() {
		row := settingRow{Setting: s, Input: s.Value}
		if s.Name == name {
			row.Input, row.Error = input, msg
		}
		data.Settings = append(data.Settings, row)
	}
	if err := h.settings.Err(); err != nil {
		data.LoadError = errorLines(err)
	}
	data.Health = h.health(r.Context())
	h.render(w, r, status, "settings", "Settings and health", data)
}

// health checks the database. A failed call is shown on the page rather
// than failing the page, since the page is where the admin looks for it.
func (h *Handler) health(ctx context.Context) health {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	db := h.settings.Current().DB
	hl := health{DB: db, Postgres: db.Driver == config.DriverPostgres}
	if err := h.store.Ping(ctx); err != nil {
		h.log.ErrorContext(ctx, "admin health ping", "err", err)
		hl.PingError = "The database cannot be reached. Check that it is running and that the HOOKD_DB_* settings are right."
		return hl
	}
	hl.Reachable = true
	if pending, err := h.store.PendingMigrations(ctx); err != nil {
		h.log.ErrorContext(ctx, "admin health pending migrations", "err", err)
	} else {
		hl.Pending, hl.PendingKnown = pending, true
	}
	if counts, err := h.store.CountDeliveriesByStatus(ctx); err != nil {
		h.log.ErrorContext(ctx, "admin health count deliveries", "err", err)
	} else {
		hl.Counts = counts
	}
	if n, err := h.store.CountWaitingRetries(ctx); err != nil {
		h.log.ErrorContext(ctx, "admin health count waiting retries", "err", err)
		hl.WaitingRetries = -1
	} else {
		hl.WaitingRetries = n
	}
	return hl
}

// saveSetting saves the posted value of an editable setting through the
// manager, so it applies at once (R23). An invalid value is refused with
// the parser's message and nothing is saved (R25).
func (h *Handler) saveSetting(w http.ResponseWriter, r *http.Request) {
	name, ok := h.editableSetting(w, r)
	if !ok {
		return
	}
	input := r.PostFormValue("value")
	h.changeSetting(w, r, name, input, h.settings.Set(r.Context(), name, input))
}

// resetSetting deletes the UI value of an editable setting, so the
// environment or default value applies again (R23).
func (h *Handler) resetSetting(w http.ResponseWriter, r *http.Request) {
	name, ok := h.editableSetting(w, r)
	if !ok {
		return
	}
	input := ""
	for _, s := range h.settings.List() {
		if s.Name == name {
			input = s.Value
		}
	}
	h.changeSetting(w, r, name, input, h.settings.Reset(r.Context(), name))
}

// changeSetting answers a save or reset that returned err: back to the page
// on success, the page with the message on an invalid configuration.
func (h *Handler) changeSetting(w http.ResponseWriter, r *http.Request, name, input string, err error) {
	var invalid *settings.InvalidError
	switch {
	case err == nil:
		http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
	case errors.As(err, &invalid):
		h.renderSettings(w, r, http.StatusUnprocessableEntity, name, input, errorLines(invalid))
	default:
		h.storeError(w, r, err)
	}
}

// editableSetting returns the {name} path value if it is a setting that can
// be changed here. An unknown name is not found, and a setting that is set
// with environment variables is refused (R26).
func (h *Handler) editableSetting(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if settings.Editable(name) {
		return name, true
	}
	for _, s := range h.settings.List() {
		if s.Name == name {
			h.renderError(w, r, http.StatusForbidden, "Setting not changed",
				name+" is set with environment variables and cannot be changed here. Set it in Hookd's environment and restart Hookd.")
			return "", false
		}
	}
	h.notFound(w, r)
	return "", false
}

// errorLines splits the parser's message into lines without its heading
// and indentation.
func errorLines(err error) []string {
	msg := strings.TrimPrefix(err.Error(), "invalid configuration:\n")
	var lines []string
	for l := range strings.SplitSeq(msg, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}
