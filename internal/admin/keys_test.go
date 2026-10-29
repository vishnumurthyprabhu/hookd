package admin_test

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"hookd/internal/store"
)

// newAccount creates an account with one API key and returns both.
func (u *testUI) newAccount(name string) (store.Account, store.APIKey) {
	u.t.Helper()
	ctx := context.Background()
	acct, err := u.st.CreateAccount(ctx, name)
	if err != nil {
		u.t.Fatal(err)
	}
	k, _, err := u.st.CreateAPIKey(ctx, acct.ID)
	if err != nil {
		u.t.Fatal(err)
	}
	return acct, k
}

// apiStatus is the status of GET /v1/endpoints with the given API key.
func (u *testUI) apiStatus(key string) int {
	u.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/endpoints", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	return u.serve(req).Code
}

func TestAccountPage(t *testing.T) {
	u := newTestUI(t)
	acct, k := u.newAccount("acme")

	list := wantPage(t, u.get("/admin/"), http.StatusOK)
	if !strings.Contains(list, `href="/admin/accounts/`+acct.ID+`"`) {
		t.Fatalf("account list does not link to the account page: %q", list)
	}

	body := wantPage(t, u.get("/admin/accounts/"+acct.ID), http.StatusOK)
	for _, want := range []string{
		"acme", acct.ID, k.Prefix,
		`href="/admin/accounts/` + acct.ID + `/endpoints"`,
		`href="/admin/accounts/` + acct.ID + `/events"`,
		`href="/admin/accounts/` + acct.ID + `/keys/` + k.ID + `/revoke"`,
		`action="/admin/accounts/` + acct.ID + `/keys"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("account page lacks %q: %q", want, body)
		}
	}
}

func TestCreateKeyShownOnce(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")

	body := wantPage(t, u.post("/admin/accounts/"+acct.ID+"/keys", nil), http.StatusOK)
	m := keyRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no API key on the page: %q", body)
	}
	key := html.UnescapeString(m[1])
	if !strings.Contains(body, `data-copy="api-key"`) {
		t.Fatal("no copy button for the key")
	}
	if got := u.apiStatus(key); got != http.StatusOK {
		t.Fatalf("GET /v1/endpoints with the new key: status %d", got)
	}

	keys, err := u.st.ListAPIKeys(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %+v, want 2", keys)
	}
	newKey := keys[1]
	if !strings.HasPrefix(key, newKey.Prefix) || key == newKey.Prefix {
		t.Fatalf("prefix %q is not a proper prefix of the key", newKey.Prefix)
	}

	// Afterwards only the prefix is shown.
	for _, path := range []string{"/admin/accounts/" + acct.ID, "/admin/", "/admin/accounts/" + acct.ID + "/keys/" + newKey.ID + "/revoke"} {
		page := wantPage(t, u.get(path), http.StatusOK)
		if strings.Contains(page, key) {
			t.Fatalf("%s shows the full API key", path)
		}
		if path != "/admin/" && !strings.Contains(page, newKey.Prefix) {
			t.Fatalf("%s lacks the key prefix", path)
		}
	}
	if strings.Contains(u.logs.String(), key) {
		t.Fatal("the API key was logged")
	}
}

func TestRevokeKey(t *testing.T) {
	u := newTestUI(t)
	body := wantPage(t, u.post("/admin/accounts", url.Values{"name": {"acme"}}), http.StatusOK)
	key := html.UnescapeString(keyRE.FindStringSubmatch(body)[1])
	accts, err := u.st.ListAccounts(context.Background(), 10, "")
	if err != nil {
		t.Fatal(err)
	}
	acct := accts[0]
	keys, err := u.st.ListAPIKeys(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/accounts/" + acct.ID + "/keys/" + keys[0].ID + "/revoke"

	// The confirm page does not revoke.
	confirm := wantPage(t, u.get(path), http.StatusOK)
	if !strings.Contains(confirm, `<form method="post" action="`+path+`">`) {
		t.Fatalf("confirm page has no revoke form: %q", confirm)
	}
	if got := u.apiStatus(key); got != http.StatusOK {
		t.Fatalf("after GET of the confirm page: API status %d, want 200", got)
	}

	rec := u.post(path, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/accounts/"+acct.ID {
		t.Fatalf("revoke: status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	if got := u.apiStatus(key); got != http.StatusUnauthorized {
		t.Fatalf("revoked key: API status %d, want 401", got)
	}
	page := wantPage(t, u.get("/admin/accounts/"+acct.ID), http.StatusOK)
	if strings.Contains(page, keys[0].Prefix) {
		t.Fatal("account page still lists the revoked key")
	}

	// Revoking it again finds nothing.
	wantPage(t, u.post(path, nil), http.StatusNotFound)
	wantPage(t, u.get(path), http.StatusNotFound)
}

func TestRevokeCrossSiteRefused(t *testing.T) {
	u := newTestUI(t)
	acct, k := u.newAccount("acme")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/"+acct.ID+"/keys/"+k.ID+"/revoke", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	wantPage(t, u.serve(req), http.StatusForbidden)
	keys, err := u.st.ListAPIKeys(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("keys = %+v, want the key kept", keys)
	}
}

func TestAccountAndKeyNotFound(t *testing.T) {
	u := newTestUI(t)
	acme, acmeKey := u.newAccount("acme")
	other, otherKey := u.newAccount("other")

	for _, path := range []string{
		"/admin/accounts/acct_nope",
		"/admin/accounts/acct_nope/keys/" + acmeKey.ID + "/revoke",
		"/admin/accounts/" + acme.ID + "/keys/key_nope/revoke",
		// Another account's key is not found under this account.
		"/admin/accounts/" + acme.ID + "/keys/" + otherKey.ID + "/revoke",
	} {
		body := wantPage(t, u.get(path), http.StatusNotFound)
		if strings.Contains(body, otherKey.Prefix) {
			t.Fatalf("GET %s shows the other account's key", path)
		}
		wantPage(t, u.post(path, nil), http.StatusNotFound)
	}
	wantPage(t, u.post("/admin/accounts/acct_nope/keys", nil), http.StatusNotFound)

	// Nothing was revoked or created.
	for _, a := range []store.Account{acme, other} {
		keys, err := u.st.ListAPIKeys(context.Background(), a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(keys) != 1 {
			t.Fatalf("account %s: keys = %+v, want its one key", a.Name, keys)
		}
	}
}
