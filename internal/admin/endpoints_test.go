package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"hookd/internal/store"
)

// newEndpoint creates an endpoint through the store and returns it with its
// secret.
func (u *testUI) newEndpoint(acctID string, in store.NewEndpoint) (store.Endpoint, string) {
	u.t.Helper()
	ep, secret, err := u.st.CreateEndpoint(context.Background(), acctID, in)
	if err != nil {
		u.t.Fatal(err)
	}
	return ep, secret
}

// endpoints lists the account's endpoints in the store.
func (u *testUI) endpoints(acctID string) []store.Endpoint {
	u.t.Helper()
	eps, err := u.st.ListEndpoints(context.Background(), acctID, 1000, "")
	if err != nil {
		u.t.Fatal(err)
	}
	return eps
}

// wantRedirect checks that rec redirects to location after a form.
func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, location string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != location {
		t.Fatalf("status %d, Location %q, want 303 to %s (body %q)", rec.Code, rec.Header().Get("Location"), location, rec.Body.String())
	}
}

func TestCreateEndpoint(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	base := "/admin/accounts/" + acct.ID + "/endpoints"

	list := wantPage(t, u.get(base), http.StatusOK)
	if !strings.Contains(list, `href="`+base+`/new"`) {
		t.Fatalf("list has no link to the create form: %q", list)
	}
	form := wantPage(t, u.get(base+"/new"), http.StatusOK)
	if !strings.Contains(form, `action="`+base+`/new"`) {
		t.Fatalf("no create form: %q", form)
	}

	wantRedirect(t, u.post(base+"/new", url.Values{
		"url":         {"  https://example.com/hook  "},
		"description": {"Orders"},
		"event_types": {"order.paid, user.created\r\norder.paid\n\n refund.issued "},
	}), base)

	eps := u.endpoints(acct.ID)
	if len(eps) != 1 {
		t.Fatalf("endpoints = %+v, want 1", eps)
	}
	ep := eps[0]
	if ep.URL != "https://example.com/hook" || ep.Description != "Orders" || !ep.Enabled {
		t.Fatalf("endpoint = %+v", ep)
	}
	if want := []string{"order.paid", "user.created", "refund.issued"}; !reflect.DeepEqual(ep.EventTypes, want) {
		t.Fatalf("event types = %q, want %q", ep.EventTypes, want)
	}

	// Empty event types mean all types.
	wantRedirect(t, u.post(base+"/new", url.Values{"url": {"http://localhost:9000/"}, "event_types": {" , \n"}}), base)
	eps = u.endpoints(acct.ID)
	if len(eps) != 2 || len(eps[1].EventTypes) != 0 {
		t.Fatalf("endpoints = %+v, want a second one for all event types", eps)
	}

	list = wantPage(t, u.get(base), http.StatusOK)
	for _, want := range []string{"https://example.com/hook", "Orders", "<code>order.paid</code>, <code>user.created</code>", "all", "enabled", eps[0].ID, eps[1].ID} {
		if !strings.Contains(list, want) {
			t.Fatalf("list lacks %q: %q", want, list)
		}
	}
}

// invalidEndpointForms are endpoint inputs the API refuses, with the form
// fields that carry them in the UI.
var invalidEndpointForms = []struct {
	name       string
	api        string
	form       url.Values
	wantInPage string
}{
	{"missing url", `{}`, url.Values{"url": {""}}, "URL is required."},
	{"relative url", `{"url":"/hook"}`, url.Values{"url": {"/hook"}}, "absolute http or https URL"},
	{"no scheme", `{"url":"example.com/hook"}`, url.Values{"url": {"example.com/hook"}}, "absolute http or https URL"},
	{"ftp url", `{"url":"ftp://example.com/hook"}`, url.Values{"url": {"ftp://example.com/hook"}}, "absolute http or https URL"},
	{"javascript url", `{"url":"javascript:alert(1)"}`, url.Values{"url": {"javascript:alert(1)"}}, "absolute http or https URL"},
	{"no host", `{"url":"https:///hook"}`, url.Values{"url": {"https:///hook"}}, "absolute http or https URL"},
	{"opaque url", `{"url":"https:example.com"}`, url.Values{"url": {"https:example.com"}}, "absolute http or https URL"},
	{"port only", `{"url":"http://:8080/hook"}`, url.Values{"url": {"http://:8080/hook"}}, "absolute http or https URL"},
	{"url too long", `{"url":"https://example.com/` + strings.Repeat("a", 2048) + `"}`,
		url.Values{"url": {"https://example.com/" + strings.Repeat("a", 2048)}}, "at most 2048 bytes"},
	{"description too long", `{"url":"https://example.com","description":"` + strings.Repeat("d", 1025) + `"}`,
		url.Values{"url": {"https://example.com"}, "description": {strings.Repeat("d", 1025)}}, "at most 1024 bytes"},
	{"event type with a space", `{"url":"https://example.com","event_types":["order paid"]}`,
		url.Values{"url": {"https://example.com"}, "event_types": {"order paid"}}, "1 to 128 letters"},
	{"event type with a slash", `{"url":"https://example.com","event_types":["ok","order/paid"]}`,
		url.Values{"url": {"https://example.com"}, "event_types": {"ok, order/paid"}}, "1 to 128 letters"},
	{"event type too long", `{"url":"https://example.com","event_types":["` + strings.Repeat("a", 129) + `"]}`,
		url.Values{"url": {"https://example.com"}, "event_types": {strings.Repeat("a", 129)}}, "1 to 128 letters"},
	{"too many event types", `{"url":"https://example.com","event_types":[` + quotedTypes(101) + `]}`,
		url.Values{"url": {"https://example.com"}, "event_types": {strings.Join(typeNames(101), ",")}}, "at most 100 types"},
}

func typeNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("t%d", i)
	}
	return names
}

func quotedTypes(n int) string {
	b, _ := json.Marshal(typeNames(n))
	return strings.Trim(string(b), "[]")
}

// TestCreateEndpointInvalid shows that each input the API refuses is refused
// by the create form too, with a message and the input kept (R5, R11, N4).
func TestCreateEndpointInvalid(t *testing.T) {
	u := newTestUI(t)
	acct, err := u.st.CreateAccount(context.Background(), "acme")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := u.st.CreateAPIKey(context.Background(), acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/admin/accounts/" + acct.ID + "/endpoints/new"

	for _, tt := range invalidEndpointForms {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/endpoints", strings.NewReader(tt.api))
			req.Header.Set("Authorization", "Bearer "+key)
			if rec := u.serve(req); rec.Code != http.StatusBadRequest {
				t.Fatalf("API status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}

			body := wantPage(t, u.post(path, tt.form), http.StatusUnprocessableEntity)
			if !strings.Contains(body, `class="error"`) || !strings.Contains(html.UnescapeString(body), tt.wantInPage) {
				t.Fatalf("no message %q on the page: %q", tt.wantInPage, body)
			}
			for field, vals := range tt.form {
				if vals[0] != "" && !strings.Contains(html.UnescapeString(body), vals[0]) {
					t.Fatalf("input of %s not kept", field)
				}
			}
		})
	}
	if eps := u.endpoints(acct.ID); len(eps) != 0 {
		t.Fatalf("refused forms created endpoints: %+v", eps)
	}
}

func TestEditEndpoint(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a", Description: "A", EventTypes: []string{"order.paid", "user.created"}})
	path := "/admin/accounts/" + acct.ID + "/endpoints/" + ep.ID

	body := wantPage(t, u.get(path), http.StatusOK)
	for _, want := range []string{
		`action="` + path + `"`, `value="https://example.com/a"`, `value="A"`, "order.paid\nuser.created</textarea>",
		`href="` + path + `/secret"`, `href="` + path + `/delete"`, `action="` + path + `/disable"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("edit page lacks %q: %q", want, body)
		}
	}
	list := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints"), http.StatusOK)
	if !strings.Contains(list, `href="`+path+`"`) {
		t.Fatal("list does not link to the edit page")
	}

	wantRedirect(t, u.post(path, url.Values{"url": {"https://example.com/b"}, "description": {""}, "event_types": {""}}), path)
	got, err := u.st.GetEndpoint(context.Background(), acct.ID, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://example.com/b" || got.Description != "" || len(got.EventTypes) != 0 || !got.Enabled {
		t.Fatalf("endpoint = %+v", got)
	}
}

func TestEditEndpointInvalid(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a", EventTypes: []string{"order.paid"}})
	path := "/admin/accounts/" + acct.ID + "/endpoints/" + ep.ID

	for _, tt := range invalidEndpointForms {
		t.Run(tt.name, func(t *testing.T) {
			body := wantPage(t, u.post(path, tt.form), http.StatusUnprocessableEntity)
			if !strings.Contains(body, `class="error"`) || !strings.Contains(html.UnescapeString(body), tt.wantInPage) {
				t.Fatalf("no message %q on the page: %q", tt.wantInPage, body)
			}
			if !strings.Contains(body, `action="`+path+`"`) {
				t.Fatal("edit form not shown again")
			}
		})
	}
	// Several errors are shown together, and the input is kept.
	body := wantPage(t, u.post(path, url.Values{"url": {"nope"}, "description": {"keep me"}, "event_types": {"bad type"}}), http.StatusUnprocessableEntity)
	if strings.Count(body, `class="error"`) != 2 || !strings.Contains(body, `value="nope"`) || !strings.Contains(body, `value="keep me"`) || !strings.Contains(body, ">bad type</textarea>") {
		t.Fatalf("errors or input not shown: %q", body)
	}

	got, err := u.st.GetEndpoint(context.Background(), acct.ID, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, ep) {
		t.Fatalf("refused edits changed the endpoint: %+v, was %+v", got, ep)
	}
}

func TestEnableDisableEndpoint(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	path := "/admin/accounts/" + acct.ID + "/endpoints/" + ep.ID

	enabled := func() bool {
		t.Helper()
		got, err := u.st.GetEndpoint(context.Background(), acct.ID, ep.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Enabled
	}

	wantRedirect(t, u.post(path+"/disable", nil), path)
	if enabled() {
		t.Fatal("endpoint still enabled")
	}
	body := wantPage(t, u.get(path), http.StatusOK)
	if !strings.Contains(body, "<strong>disabled</strong>") || !strings.Contains(body, `action="`+path+`/enable"`) {
		t.Fatalf("page does not show the endpoint disabled: %q", body)
	}
	if list := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints"), http.StatusOK); !strings.Contains(list, "<td>disabled</td>") {
		t.Fatal("list does not show the endpoint disabled")
	}

	wantRedirect(t, u.post(path+"/enable", nil), path)
	if !enabled() {
		t.Fatal("endpoint still disabled")
	}

	// GET changes nothing.
	wantPage(t, u.get(path+"/disable"), http.StatusNotFound)
	if !enabled() {
		t.Fatal("GET disabled the endpoint")
	}
}

func TestDeleteEndpoint(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	path := "/admin/accounts/" + acct.ID + "/endpoints/" + ep.ID

	// The confirm page does not delete.
	body := wantPage(t, u.get(path+"/delete"), http.StatusOK)
	if !strings.Contains(body, `<form method="post" action="`+path+`/delete">`) || !strings.Contains(body, "https://example.com/a") {
		t.Fatalf("no confirm form: %q", body)
	}
	if _, err := u.st.GetEndpoint(context.Background(), acct.ID, ep.ID); err != nil {
		t.Fatalf("the confirm page deleted the endpoint: %v", err)
	}

	wantRedirect(t, u.post(path+"/delete", nil), "/admin/accounts/"+acct.ID+"/endpoints")
	if _, err := u.st.GetEndpoint(context.Background(), acct.ID, ep.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetEndpoint after delete: %v, want ErrNotFound", err)
	}
	if list := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints"), http.StatusOK); strings.Contains(list, ep.ID) {
		t.Fatal("list still shows the deleted endpoint")
	}
	for _, p := range []string{path, path + "/delete", path + "/secret"} {
		wantPage(t, u.get(p), http.StatusNotFound)
	}
	wantPage(t, u.post(path+"/delete", nil), http.StatusNotFound)
}

func TestEndpointSecretPage(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, secret := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a", EventTypes: []string{"order.paid"}})
	path := "/admin/accounts/" + acct.ID + "/endpoints/" + ep.ID

	// html/template writes "+" as "&#43;", which base64 secrets may contain.
	body := html.UnescapeString(wantPage(t, u.get(path+"/secret"), http.StatusOK))
	if !strings.Contains(body, `<code id="endpoint-secret">`+secret+`</code>`) || !strings.Contains(body, `data-copy="endpoint-secret"`) {
		t.Fatalf("secret page lacks the secret or its copy button: %q", body)
	}
	if list := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints"), http.StatusOK); !strings.Contains(list, `href="`+path+`/secret"`) {
		t.Fatal("list does not link to the secret page")
	}

	// No other page shows it, including pages shown after a form.
	pages := map[string]string{
		"list":       wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints"), http.StatusOK),
		"edit":       wantPage(t, u.get(path), http.StatusOK),
		"bad edit":   wantPage(t, u.post(path, url.Values{"url": {"nope"}}), http.StatusUnprocessableEntity),
		"delete":     wantPage(t, u.get(path+"/delete"), http.StatusOK),
		"account":    wantPage(t, u.get("/admin/accounts/"+acct.ID), http.StatusOK),
		"accounts":   wantPage(t, u.get("/admin/"), http.StatusOK),
		"create":     wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints/new"), http.StatusOK),
		"bad create": wantPage(t, u.post("/admin/accounts/"+acct.ID+"/endpoints/new", url.Values{"url": {"nope"}}), http.StatusUnprocessableEntity),
	}
	for name, page := range pages {
		if strings.Contains(html.UnescapeString(page), secret) || strings.Contains(page, "whsec_") {
			t.Fatalf("the %s page shows the secret", name)
		}
	}
	if strings.Contains(u.logs.String(), secret) {
		t.Fatal("the secret was logged")
	}
}

func TestEndpointListMarksTestEndpoints(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	plain, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	te, err := u.st.CreateTestEndpoint(context.Background(), acct.ID, store.NewTestEndpoint{BaseURL: "http://127.0.0.1:8080"})
	if err != nil {
		t.Fatal(err)
	}

	list := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints"), http.StatusOK)
	row := func(id string) string {
		t.Helper()
		i := strings.Index(list, `<tr id="endpoint-`+id+`">`)
		if i < 0 {
			t.Fatalf("no row for %s: %q", id, list)
		}
		return list[i : i+strings.Index(list[i:], "</tr>")]
	}
	if !strings.Contains(row(te.Endpoint.ID), "test endpoint") {
		t.Fatal("test endpoint not marked")
	}
	if strings.Contains(row(plain.ID), "test endpoint") {
		t.Fatal("ordinary endpoint marked as a test endpoint")
	}
	if page := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints/"+te.Endpoint.ID), http.StatusOK); !strings.Contains(page, "test endpoint") {
		t.Fatal("test endpoint's page does not say it is one")
	}
}

func TestEndpointListPaging(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	var ids []string
	for i := range 51 {
		ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: fmt.Sprintf("https://example.com/%02d", i)})
		ids = append(ids, ep.ID)
	}
	base := "/admin/accounts/" + acct.ID + "/endpoints"

	first := wantPage(t, u.get(base), http.StatusOK)
	if !strings.Contains(first, ids[49]) || strings.Contains(first, ids[50]) {
		t.Fatal("first page does not hold exactly the first 50 endpoints")
	}
	next := base + "?after=" + ids[49]
	if !strings.Contains(first, `href="`+next+`"`) {
		t.Fatalf("no next page link to %s", next)
	}
	second := wantPage(t, u.get(next), http.StatusOK)
	if !strings.Contains(second, ids[50]) || strings.Contains(second, ids[49]) {
		t.Fatal("second page does not hold exactly the last endpoint")
	}
	if strings.Contains(second, "Next page") {
		t.Fatal("last page has a next page link")
	}
}

// TestOtherAccountsEndpointNotFound shows that an endpoint is found only
// under its own account, and that nothing is changed or shown through
// another account.
func TestOtherAccountsEndpointNotFound(t *testing.T) {
	u := newTestUI(t)
	acme, _ := u.newAccount("acme")
	other, _ := u.newAccount("other")
	ep, secret := u.newEndpoint(other.ID, store.NewEndpoint{URL: "https://other.example.com/hook"})

	for _, base := range []string{
		"/admin/accounts/" + acme.ID + "/endpoints/" + ep.ID,
		"/admin/accounts/acct_nope/endpoints/" + ep.ID,
		"/admin/accounts/" + acme.ID + "/endpoints/ep_nope",
	} {
		for _, p := range []string{base, base + "/delete", base + "/secret"} {
			body := html.UnescapeString(wantPage(t, u.get(p), http.StatusNotFound))
			if strings.Contains(body, secret) || strings.Contains(body, "other.example.com") {
				t.Fatalf("GET %s shows the other account's endpoint", p)
			}
		}
		wantPage(t, u.post(base, url.Values{"url": {"https://evil.example.com"}}), http.StatusNotFound)
		for _, action := range []string{"/enable", "/disable", "/delete"} {
			wantPage(t, u.post(base+action, nil), http.StatusNotFound)
		}
	}
	for _, p := range []string{"/admin/accounts/acct_nope/endpoints", "/admin/accounts/acct_nope/endpoints/new"} {
		wantPage(t, u.get(p), http.StatusNotFound)
	}
	wantPage(t, u.post("/admin/accounts/acct_nope/endpoints/new", url.Values{"url": {"https://example.com"}}), http.StatusNotFound)

	if list := wantPage(t, u.get("/admin/accounts/"+acme.ID+"/endpoints"), http.StatusOK); strings.Contains(list, ep.ID) {
		t.Fatal("acme's list shows the other account's endpoint")
	}
	got, err := u.st.GetEndpoint(context.Background(), other.ID, ep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, ep) {
		t.Fatalf("the endpoint changed: %+v, was %+v", got, ep)
	}
	if eps := u.endpoints(acme.ID); len(eps) != 0 {
		t.Fatalf("acme has endpoints: %+v", eps)
	}
}

func TestEndpointCrossSitePostRefused(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	for _, p := range []string{"/endpoints/new", "/endpoints/" + ep.ID + "/delete", "/endpoints/" + ep.ID + "/disable"} {
		req := httptest.NewRequest(http.MethodPost, "/admin/accounts/"+acct.ID+p, strings.NewReader("url=https://evil.example.com"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		if rec := u.serve(req); rec.Code != http.StatusForbidden {
			t.Fatalf("POST %s: status = %d, want 403", p, rec.Code)
		}
	}
	eps := u.endpoints(acct.ID)
	if len(eps) != 1 || !eps[0].Enabled {
		t.Fatalf("endpoints = %+v, want the one enabled endpoint", eps)
	}
}
