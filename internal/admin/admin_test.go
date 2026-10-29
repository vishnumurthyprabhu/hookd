package admin_test

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"hookd/internal/admin"
	"hookd/internal/api"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

// notice is part of the no-sign-in notice every page must show (R4).
const notice = "No sign-in."

type testUI struct {
	t       *testing.T
	st      store.Store
	handler http.Handler
	logs    *bytes.Buffer
}

// newTestUI returns the API server with the admin UI mounted, on a fresh
// store.
func newTestUI(t *testing.T) *testUI {
	t.Helper()
	st := storetest.New(t)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := api.New(st, log, api.WithAdmin(admin.New(st, log)))
	return &testUI{t: t, st: st, handler: srv.Handler(), logs: logs}
}

func (u *testUI) get(path string) *httptest.ResponseRecorder {
	u.t.Helper()
	return u.serve(httptest.NewRequest(http.MethodGet, path, nil))
}

// post sends a same-origin form, as a browser would from the UI's own page.
func (u *testUI) post(path string, form url.Values) *httptest.ResponseRecorder {
	u.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return u.serve(req)
}

func (u *testUI) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	u.handler.ServeHTTP(rec, req)
	return rec
}

// wantPage checks that rec is an HTML page with the given status and the
// no-sign-in notice, and returns its body.
func wantPage(t *testing.T, rec *httptest.ResponseRecorder, status int) string {
	t.Helper()
	body := rec.Body.String()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(body, notice) {
		t.Fatalf("page has no no-sign-in notice: %q", body)
	}
	return body
}

func TestRootRedirects(t *testing.T) {
	u := newTestUI(t)
	for _, path := range []string{"/", "/admin"} {
		rec := u.get(path)
		if rec.Code < 300 || rec.Code > 399 {
			t.Fatalf("GET %s: status %d, want a redirect", path, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "/admin/" {
			t.Fatalf("GET %s: Location = %q, want /admin/", path, loc)
		}
	}
}

func TestNoticeOnEveryPage(t *testing.T) {
	u := newTestUI(t)
	wantPage(t, u.get("/admin/"), http.StatusOK)
	wantPage(t, u.get("/admin/no-such-page"), http.StatusNotFound)
	wantPage(t, u.post("/admin/accounts", url.Values{"name": {""}}), http.StatusUnprocessableEntity)
	wantPage(t, u.post("/admin/accounts", url.Values{"name": {"acme"}}), http.StatusOK)
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts", strings.NewReader("name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	wantPage(t, u.serve(req), http.StatusForbidden)
}

func TestStaticFiles(t *testing.T) {
	u := newTestUI(t)
	for _, path := range []string{"/admin/static/style.css", "/admin/static/copy.js"} {
		if rec := u.get(path); rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("GET %s: status %d, %d bytes", path, rec.Code, rec.Body.Len())
		}
	}
}

var keyRE = regexp.MustCompile(`<code id="api-key">([^<]+)</code>`)

func TestCreateAccount(t *testing.T) {
	u := newTestUI(t)
	body := wantPage(t, u.post("/admin/accounts", url.Values{"name": {"  acme  "}}), http.StatusOK)
	m := keyRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no API key on the page: %q", body)
	}
	key := html.UnescapeString(m[1])
	if !strings.Contains(body, `data-copy="api-key"`) {
		t.Fatal("no copy button for the key")
	}

	accts, err := u.st.ListAccounts(context.Background(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 1 || accts[0].Name != "acme" || accts[0].APIKeys != 1 {
		t.Fatalf("accounts = %+v, want one named acme with one key", accts)
	}

	// The key works with the API.
	req := httptest.NewRequest(http.MethodGet, "/v1/endpoints", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	if rec := u.serve(req); rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/endpoints with the new key: status %d, body %q", rec.Code, rec.Body.String())
	}

	// The list shows the account but never the key again.
	list := wantPage(t, u.get("/admin/"), http.StatusOK)
	if !strings.Contains(list, accts[0].ID) || !strings.Contains(list, "acme") {
		t.Fatalf("account list lacks the account: %q", list)
	}
	if strings.Contains(list, key) {
		t.Fatal("account list shows the API key")
	}
	if strings.Contains(u.logs.String(), key) {
		t.Fatal("the API key was logged")
	}
}

func TestCreateAccountEmptyName(t *testing.T) {
	u := newTestUI(t)
	body := wantPage(t, u.post("/admin/accounts", url.Values{"name": {"   "}}), http.StatusUnprocessableEntity)
	if !strings.Contains(body, "Enter a name for the account.") {
		t.Fatalf("no error message: %q", body)
	}
	if !strings.Contains(body, `<form method="post" action="/admin/accounts">`) || !strings.Contains(body, `value="   "`) {
		t.Fatalf("form not shown again with the input: %q", body)
	}
	accts, err := u.st.ListAccounts(context.Background(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 0 {
		t.Fatalf("accounts = %+v, want none", accts)
	}
}

func TestCrossSitePostRefused(t *testing.T) {
	u := newTestUI(t)
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts", strings.NewReader("name=evil"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := u.serve(req); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	accts, err := u.st.ListAccounts(context.Background(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 0 {
		t.Fatalf("accounts = %+v, want none", accts)
	}
}

func TestAccountListPaging(t *testing.T) {
	u := newTestUI(t)
	ctx := context.Background()
	var ids []string
	for i := range 51 {
		a, err := u.st.CreateAccount(ctx, fmt.Sprintf("acct-%02d", i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	if _, _, err := u.st.CreateEndpoint(ctx, ids[0], store.NewEndpoint{URL: "https://example.com/hook"}); err != nil {
		t.Fatal(err)
	}

	first := wantPage(t, u.get("/admin/"), http.StatusOK)
	if !strings.Contains(first, ids[49]) || strings.Contains(first, ids[50]) {
		t.Fatal("first page does not hold exactly the first 50 accounts")
	}
	if !strings.Contains(first, "<td>1</td>") {
		t.Fatalf("first account's endpoint count not shown: %q", first)
	}
	next := "/admin/?after=" + ids[49]
	if !strings.Contains(first, `href="`+next+`"`) {
		t.Fatalf("no next page link to %s", next)
	}
	second := wantPage(t, u.get(next), http.StatusOK)
	if !strings.Contains(second, ids[50]) || strings.Contains(second, ids[49]) {
		t.Fatal("second page does not hold exactly the last account")
	}
	if strings.Contains(second, "Next page") {
		t.Fatal("last page has a next page link")
	}
}

func TestWithoutAdminIsJSON404(t *testing.T) {
	st := storetest.New(t)
	log := slog.New(slog.DiscardHandler)
	h := api.New(st, log).Handler()
	for _, path := range []string{"/", "/admin", "/admin/", "/admin/static/style.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("GET %s: status %d, Content-Type %q, want the JSON 404", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz: status %d", rec.Code)
	}
}

func TestOtherPathsKeepJSON404(t *testing.T) {
	u := newTestUI(t)
	rec := u.get("/nope")
	if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("GET /nope: status %d, Content-Type %q, want the JSON 404", rec.Code, rec.Header().Get("Content-Type"))
	}
}
