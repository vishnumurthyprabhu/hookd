package admin_test

import (
	"bytes"
	"context"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"hookd/internal/admin"
	"hookd/internal/api"
	"hookd/internal/settings"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

const testPassword = "pw-must-not-show"

// newSettingsUI returns the UI with the settings page, on a manager whose
// environment is env.
func newSettingsUI(t *testing.T, env map[string]string) (*testUI, *settings.Manager) {
	t.Helper()
	st := storetest.New(t)
	mgr, err := settings.New(st, settings.WithLookupEnv(func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := api.New(st, log, api.WithAdmin(admin.New(st, log, admin.WithSettings(mgr))))
	return &testUI{t: t, st: st, handler: srv.Handler(), logs: logs}, mgr
}

// settingRow returns the table row of the setting name.
func settingRow(t *testing.T, body, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<tr id="setting-` + name + `">.*?</tr>`).FindString(body)
	if m == "" {
		t.Fatalf("no row for %s: %q", name, body)
	}
	return m
}

func savedSettings(t *testing.T, st store.Store) map[string]string {
	t.Helper()
	rows, err := st.ListSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Name] = r.Value
	}
	return out
}

func TestSettingsPageListsEverySetting(t *testing.T) {
	u, _ := newSettingsUI(t, map[string]string{"HOOKD_WORKERS": "3", "HOOKD_DB_PASSWORD": testPassword})
	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)

	row := settingRow(t, body, "HOOKD_WORKERS")
	if !strings.Contains(row, `value="3"`) || !strings.Contains(row, "<td>environment</td>") ||
		!strings.Contains(row, `action="/admin/settings/HOOKD_WORKERS"`) {
		t.Fatalf("HOOKD_WORKERS row: %q", row)
	}
	row = settingRow(t, body, "HOOKD_POLL_INTERVAL")
	if !strings.Contains(row, `value="1s"`) || !strings.Contains(row, "<td>default</td>") {
		t.Fatalf("HOOKD_POLL_INTERVAL row: %q", row)
	}
	for _, name := range []string{"HOOKD_LISTEN_ADDR", "HOOKD_DB_DRIVER", "HOOKD_DB_PATH", "HOOKD_AUTO_MIGRATE", "HOOKD_LOG_FORMAT", "HOOKD_ADMIN_UI", "HOOKD_ADMIN_RECEIVER_URL"} {
		row := settingRow(t, body, name)
		if !strings.Contains(row, "set with environment variables") || strings.Contains(row, "<form") {
			t.Fatalf("%s is not read-only: %q", name, row)
		}
	}
	for _, name := range []string{"HOOKD_DELIVERY_TIMEOUT", "HOOKD_DELIVERY_LEASE", "HOOKD_MAX_PAYLOAD_BYTES", "HOOKD_IDEMPOTENCY_WINDOW", "HOOKD_LOG_LEVEL"} {
		if row := settingRow(t, body, name); !strings.Contains(row, "<form") {
			t.Fatalf("%s has no edit form: %q", name, row)
		}
	}

	row = settingRow(t, body, "HOOKD_DB_PASSWORD")
	if !strings.Contains(row, "<code>set</code>") {
		t.Fatalf("password not shown as set: %q", row)
	}
	if strings.Contains(body, testPassword) {
		t.Fatal("the database password is on the page")
	}
}

func TestSettingsPasswordNotSet(t *testing.T) {
	u, _ := newSettingsUI(t, nil)
	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	if row := settingRow(t, body, "HOOKD_DB_PASSWORD"); !strings.Contains(row, "<code>not set</code>") {
		t.Fatalf("password not shown as not set: %q", row)
	}
}

func TestSaveSetting(t *testing.T) {
	u, mgr := newSettingsUI(t, map[string]string{"HOOKD_DELIVERY_TIMEOUT": "10s"})
	rec := u.post("/admin/settings/HOOKD_DELIVERY_TIMEOUT", url.Values{"value": {" 20s "}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/settings" {
		t.Fatalf("status %d, Location %q, want a redirect to the settings page", rec.Code, rec.Header().Get("Location"))
	}
	// In effect at once, without a reload.
	if got := mgr.Current().DeliveryTimeout; got != 20*time.Second {
		t.Fatalf("delivery timeout = %v, want 20s", got)
	}
	if got := savedSettings(t, u.st)["HOOKD_DELIVERY_TIMEOUT"]; got != "20s" {
		t.Fatalf("saved value = %q, want 20s", got)
	}
	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	row := settingRow(t, body, "HOOKD_DELIVERY_TIMEOUT")
	if !strings.Contains(row, `value="20s"`) || !strings.Contains(row, "<td>UI</td>") ||
		!strings.Contains(row, `action="/admin/settings/HOOKD_DELIVERY_TIMEOUT/reset"`) {
		t.Fatalf("row after save: %q", row)
	}
}

func TestSaveSettingInvalid(t *testing.T) {
	u, mgr := newSettingsUI(t, nil)
	body := wantPage(t, u.post("/admin/settings/HOOKD_DELIVERY_TIMEOUT", url.Values{"value": {"2m"}}), http.StatusUnprocessableEntity)
	row := settingRow(t, body, "HOOKD_DELIVERY_TIMEOUT")
	if !strings.Contains(html.UnescapeString(row), "must be longer than HOOKD_DELIVERY_TIMEOUT") || !strings.Contains(row, `value="2m"`) {
		t.Fatalf("row does not show the message and the input: %q", row)
	}
	if got := mgr.Current().DeliveryTimeout; got != 15*time.Second {
		t.Fatalf("delivery timeout = %v, want the default 15s", got)
	}
	if saved := savedSettings(t, u.st); len(saved) != 0 {
		t.Fatalf("saved = %v, want nothing", saved)
	}

	body = wantPage(t, u.post("/admin/settings/HOOKD_WORKERS", url.Values{"value": {"many"}}), http.StatusUnprocessableEntity)
	if row := settingRow(t, body, "HOOKD_WORKERS"); !strings.Contains(row, "a whole number greater than 0") {
		t.Fatalf("row does not show the message: %q", row)
	}
}

func TestReadOnlySettingCannotBePosted(t *testing.T) {
	u, mgr := newSettingsUI(t, nil)
	for _, path := range []string{"/admin/settings/HOOKD_LISTEN_ADDR", "/admin/settings/HOOKD_DB_PASSWORD/reset", "/admin/settings/HOOKD_ADMIN_UI"} {
		body := wantPage(t, u.post(path, url.Values{"value": {"x"}}), http.StatusForbidden)
		if !strings.Contains(body, "set with environment variables") {
			t.Fatalf("POST %s: no reason given: %q", path, body)
		}
	}
	wantPage(t, u.post("/admin/settings/HOOKD_NO_SUCH", url.Values{"value": {"1"}}), http.StatusNotFound)
	if saved := savedSettings(t, u.st); len(saved) != 0 {
		t.Fatalf("saved = %v, want nothing", saved)
	}
	if got := mgr.Current().ListenAddr; got != ":8080" {
		t.Fatalf("listen address = %q, want unchanged", got)
	}
}

func TestResetSetting(t *testing.T) {
	u, mgr := newSettingsUI(t, map[string]string{"HOOKD_WORKERS": "3"})
	if rec := u.post("/admin/settings/HOOKD_WORKERS", url.Values{"value": {"5"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: status %d", rec.Code)
	}
	if got := mgr.Current().Workers; got != 5 {
		t.Fatalf("workers = %d, want 5", got)
	}
	if rec := u.post("/admin/settings/HOOKD_WORKERS/reset", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("reset: status %d", rec.Code)
	}
	if got := mgr.Current().Workers; got != 3 {
		t.Fatalf("workers = %d, want the environment's 3", got)
	}
	if saved := savedSettings(t, u.st); len(saved) != 0 {
		t.Fatalf("saved = %v, want nothing", saved)
	}
	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	row := settingRow(t, body, "HOOKD_WORKERS")
	if !strings.Contains(row, `value="3"`) || !strings.Contains(row, "<td>environment</td>") || strings.Contains(row, "/reset") {
		t.Fatalf("row after reset: %q", row)
	}
}

func TestSettingsLoadError(t *testing.T) {
	u, mgr := newSettingsUI(t, nil)
	// Another instance saved a lease that this one's environment makes too
	// short.
	if err := u.st.SetSetting(context.Background(), "HOOKD_DELIVERY_LEASE", "1s"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Load(context.Background()); err == nil {
		t.Fatal("Load accepted an invalid lease")
	}
	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	if !strings.Contains(body, "not in effect") || !strings.Contains(html.UnescapeString(body), "must be longer than HOOKD_DELIVERY_TIMEOUT") {
		t.Fatalf("page does not show the reload error: %q", body)
	}
}

func TestHealthSection(t *testing.T) {
	u, _ := newSettingsUI(t, map[string]string{"HOOKD_DB_PATH": "/data/hookd-test.db"})
	ctx := context.Background()
	acct, err := u.st.CreateAccount(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := u.st.CreateEndpoint(ctx, acct.ID, store.NewEndpoint{URL: "https://example.com/a"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := u.st.CreateEndpoint(ctx, acct.ID, store.NewEndpoint{URL: "https://example.com/b"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := u.st.CreateEvent(ctx, acct.ID, store.NewEvent{EventType: "order.paid", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := u.st.ClaimDelivery(ctx, time.Minute); err != nil || !ok {
		t.Fatalf("claim: ok %v, err %v", ok, err)
	}

	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	for _, want := range []string{
		`<td id="health-database">reachable</td>`,
		`<code>sqlite</code>`,
		`<code>/data/hookd-test.db</code>`,
		`<td id="health-migrations">up to date</td>`,
		`<td id="health-pending">1</td>`,
		`<td id="health-in-progress">1</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("health section lacks %q: %q", want, body)
		}
	}
}

func TestHealthSectionPostgres(t *testing.T) {
	u, _ := newSettingsUI(t, map[string]string{
		"HOOKD_DB_DRIVER": "postgres", "HOOKD_DB_HOST": "db.internal", "HOOKD_DB_PORT": "6543",
		"HOOKD_DB_NAME": "hookd_prod", "HOOKD_DB_PASSWORD": testPassword,
	})
	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	if !strings.Contains(body, "<code>db.internal:6543</code>") || !strings.Contains(body, "<code>hookd_prod</code>") {
		t.Fatalf("health section lacks the PostgreSQL host, port and name: %q", body)
	}
	if strings.Contains(body, testPassword) {
		t.Fatal("the database password is on the page")
	}
}

func TestHealthSectionDatabaseDown(t *testing.T) {
	u, _ := newSettingsUI(t, nil)
	if err := u.st.Close(); err != nil {
		t.Fatal(err)
	}
	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	if !strings.Contains(body, "not reachable") {
		t.Fatalf("health section does not say the database is down: %q", body)
	}
}

func TestSettingsCrossSitePostRefused(t *testing.T) {
	u, mgr := newSettingsUI(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/admin/settings/HOOKD_WORKERS", strings.NewReader("value=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := u.serve(req); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := mgr.Current().Workers; got != 8 {
		t.Fatalf("workers = %d, want unchanged", got)
	}
}

func TestSettingsWithoutManagerNotFound(t *testing.T) {
	u := newTestUI(t)
	wantPage(t, u.get("/admin/settings"), http.StatusNotFound)
}

func TestHealthWaitingRetries(t *testing.T) {
	u, _ := newSettingsUI(t, nil)
	acct, _ := u.newAccount("acme")
	u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	u.newEvent(acct.ID, "order.paid")
	u.newEvent(acct.ID, "order.paid")
	u.newEvent(acct.ID, "order.paid")
	// One delivery fails and waits for a retry, one succeeds, one has not
	// been tried yet.
	u.attempt(store.AttemptSucceeded, false)
	u.attempt(store.AttemptFailed, true)

	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	for _, want := range []string{
		`<td id="health-waiting-retry">1</td>`,
		`<td id="health-pending">2</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("health section lacks %q: %q", want, body)
		}
	}
}

func TestRetryScheduleSetting(t *testing.T) {
	u, mgr := newSettingsUI(t, map[string]string{"HOOKD_RETENTION": "48h"})
	body := wantPage(t, u.get("/admin/settings"), http.StatusOK)
	row := settingRow(t, body, "HOOKD_RETENTION")
	if !strings.Contains(row, "<code>48h</code>") || !strings.Contains(row, "set with environment variables") || strings.Contains(row, "<form") {
		t.Fatalf("HOOKD_RETENTION is not shown read-only: %q", row)
	}
	row = settingRow(t, body, "HOOKD_RETRY_SCHEDULE")
	if !strings.Contains(row, `action="/admin/settings/HOOKD_RETRY_SCHEDULE"`) || !strings.Contains(row, `value="5s,5m,30m,2h,5h,10h,10h"`) {
		t.Fatalf("HOOKD_RETRY_SCHEDULE has no edit form with the default: %q", row)
	}
	wantPage(t, u.post("/admin/settings/HOOKD_RETENTION", url.Values{"value": {"1000h"}}), http.StatusForbidden)

	wantRedirect(t, u.post("/admin/settings/HOOKD_RETRY_SCHEDULE", url.Values{"value": {"5s,10s,20s"}}), "/admin/settings")
	if got := mgr.Current().RetrySchedule; len(got) != 3 || got[0] != 5*time.Second || got[2] != 20*time.Second {
		t.Fatalf("retry schedule = %v, want [5s 10s 20s]", got)
	}
	body = wantPage(t, u.get("/admin/settings"), http.StatusOK)
	row = settingRow(t, body, "HOOKD_RETRY_SCHEDULE")
	if !strings.Contains(row, `value="5s,10s,20s"`) || !strings.Contains(row, "<td>UI</td>") || !strings.Contains(row, "/reset") {
		t.Fatalf("row after save: %q", row)
	}

	// 20h + 20h + 4h is 48.4h with its jitter, more than the 48h retention.
	body = wantPage(t, u.post("/admin/settings/HOOKD_RETRY_SCHEDULE", url.Values{"value": {"20h,20h,4h"}}), http.StatusUnprocessableEntity)
	row = settingRow(t, body, "HOOKD_RETRY_SCHEDULE")
	if text := html.UnescapeString(row); !strings.Contains(text, "HOOKD_RETENTION") || !strings.Contains(text, "HOOKD_RETRY_SCHEDULE plus 10%") ||
		!strings.Contains(row, `value="20h,20h,4h"`) {
		t.Fatalf("row does not show the reason and the input: %q", row)
	}
	if got := savedSettings(t, u.st)["HOOKD_RETRY_SCHEDULE"]; got != "5s,10s,20s" {
		t.Fatalf("saved schedule = %q, want the previous 5s,10s,20s", got)
	}

	wantRedirect(t, u.post("/admin/settings/HOOKD_RETRY_SCHEDULE/reset", nil), "/admin/settings")
	if got := mgr.Current().RetrySchedule; len(got) != 7 {
		t.Fatalf("retry schedule after reset = %v, want the default", got)
	}
	if saved := savedSettings(t, u.st); len(saved) != 0 {
		t.Fatalf("saved = %v, want nothing", saved)
	}
	row = settingRow(t, wantPage(t, u.get("/admin/settings"), http.StatusOK), "HOOKD_RETRY_SCHEDULE")
	if !strings.Contains(row, `value="5s,5m,30m,2h,5h,10h,10h"`) || !strings.Contains(row, "<td>default</td>") {
		t.Fatalf("row after reset: %q", row)
	}
}
