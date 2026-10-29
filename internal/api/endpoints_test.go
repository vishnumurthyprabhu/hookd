package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

type endpointJSON struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Description string    `json:"description"`
	EventTypes  []string  `json:"event_types"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Secret      *string   `json:"secret"`
}

func decodeEndpoint(t *testing.T, rec *httptest.ResponseRecorder, status int) endpointJSON {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	var e endpointJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return e
}

// createEndpointVia creates an endpoint through the API and returns it.
func (ts *testServer) createEndpointVia(t *testing.T, key, body string) endpointJSON {
	t.Helper()
	return decodeEndpoint(t, ts.send(t, "POST", "/v1/endpoints", key, body, nil), http.StatusCreated)
}

type endpointListJSON struct {
	Data    []endpointJSON `json:"data"`
	HasMore bool           `json:"has_more"`
}

func (ts *testServer) listEndpointsVia(t *testing.T, key, query string) endpointListJSON {
	t.Helper()
	rec := ts.send(t, "GET", "/v1/endpoints"+query, key, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var l endpointListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return l
}

func TestCreateEndpoint(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)

	rec := ts.send(t, "POST", "/v1/endpoints", key,
		`{"url":"https://example.com/hook","description":"orders","event_types":["order.paid","order.paid","user.created"]}`, nil)
	e := decodeEndpoint(t, rec, http.StatusCreated)

	if !strings.HasPrefix(e.ID, "ep_") {
		t.Errorf("id = %q, want an ep_ ID", e.ID)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/endpoints/"+e.ID {
		t.Errorf("Location = %q", loc)
	}
	if e.URL != "https://example.com/hook" || e.Description != "orders" || !e.Enabled {
		t.Errorf("endpoint = %+v", e)
	}
	if want := []string{"order.paid", "user.created"}; !reflect.DeepEqual(e.EventTypes, want) {
		t.Errorf("event_types = %v, want %v (duplicates dropped)", e.EventTypes, want)
	}
	if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
		t.Errorf("created_at = %v, updated_at = %v", e.CreatedAt, e.UpdatedAt)
	}
	if e.Secret == nil || !strings.HasPrefix(*e.Secret, "whsec_") {
		t.Fatalf("secret = %v, want a whsec_ secret", e.Secret)
	}

	// The secret given at creation is the one the secret route returns.
	rec = ts.send(t, "GET", "/v1/endpoints/"+e.ID+"/secret", key, "", nil)
	var s struct {
		Secret string `json:"secret"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("secret status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Secret != *e.Secret {
		t.Errorf("secret route = %q, want %q", s.Secret, *e.Secret)
	}

	// The secret is not in the logs (N4).
	if strings.Contains(ts.logs.String(), *e.Secret) {
		t.Error("the secret is in the logs")
	}
}

func TestCreateEndpointDefaults(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)

	for _, body := range []string{
		`{"url":"http://localhost:9000/hook"}`,
		`{"url":"http://127.0.0.1:9000","event_types":[]}`,
		`{"url":"https://example.com","event_types":null}`,
	} {
		e := ts.createEndpointVia(t, key, body)
		if e.EventTypes == nil || len(e.EventTypes) != 0 {
			t.Errorf("%s: event_types = %#v, want [] (all types)", body, e.EventTypes)
		}
		if e.Description != "" || !e.Enabled {
			t.Errorf("%s: endpoint = %+v", body, e)
		}
	}
}

func TestCreateEndpointRejects(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)

	tests := []struct {
		name, body string
		status     int
		code       string
	}{
		{"empty body", ``, 400, CodeInvalidJSON},
		{"not JSON", `url=https://example.com`, 400, CodeInvalidJSON},
		{"truncated JSON", `{"url":"https://example.com"`, 400, CodeInvalidJSON},
		{"trailing data", `{"url":"https://example.com"} {}`, 400, CodeInvalidJSON},
		{"body is an array", `[]`, 400, CodeInvalidRequest},
		{"unknown field", `{"url":"https://example.com","secret":"whsec_x"}`, 400, CodeInvalidRequest},
		{"url not a string", `{"url":5}`, 400, CodeInvalidRequest},
		{"event_types not a list", `{"url":"https://example.com","event_types":"order.paid"}`, 400, CodeInvalidRequest},
		{"description too long", `{"url":"https://example.com","description":"` + strings.Repeat("d", 1025) + `"}`, 400, CodeInvalidRequest},
		{"missing url", `{}`, 400, CodeInvalidURL},
		{"relative url", `{"url":"/hook"}`, 400, CodeInvalidURL},
		{"no scheme", `{"url":"example.com/hook"}`, 400, CodeInvalidURL},
		{"ftp url", `{"url":"ftp://example.com/hook"}`, 400, CodeInvalidURL},
		{"javascript url", `{"url":"javascript:alert(1)"}`, 400, CodeInvalidURL},
		{"no host", `{"url":"https:///hook"}`, 400, CodeInvalidURL},
		{"opaque url", `{"url":"https:example.com"}`, 400, CodeInvalidURL},
		{"port only", `{"url":"http://:8080/hook"}`, 400, CodeInvalidURL},
		{"unparsable url", `{"url":"https://exa mple.com/%zz"}`, 400, CodeInvalidURL},
		{"url too long", `{"url":"https://example.com/` + strings.Repeat("a", 2048) + `"}`, 400, CodeInvalidURL},
		{"empty event type", `{"url":"https://example.com","event_types":[""]}`, 400, CodeInvalidEventType},
		{"event type with a space", `{"url":"https://example.com","event_types":["order paid"]}`, 400, CodeInvalidEventType},
		{"event type with a slash", `{"url":"https://example.com","event_types":["ok","order/paid"]}`, 400, CodeInvalidEventType},
		{"event type too long", `{"url":"https://example.com","event_types":["` + strings.Repeat("a", 129) + `"]}`, 400, CodeInvalidEventType},
		{"too many event types", `{"url":"https://example.com","event_types":[` + manyTypes(101) + `]}`, 400, CodeInvalidEventType},
		{"body too large", `{"url":"https://example.com","description":"` + strings.Repeat("d", 65<<10) + `"}`, 413, CodePayloadTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decodeError(t, ts.send(t, "POST", "/v1/endpoints", key, tt.body, nil), tt.status, tt.code)
		})
	}

	if l := ts.listEndpointsVia(t, key, ""); len(l.Data) != 0 {
		t.Errorf("rejected requests created %d endpoints", len(l.Data))
	}

	// The limits themselves are accepted.
	ts.createEndpointVia(t, key, `{"url":"https://example.com","description":"`+strings.Repeat("d", 1024)+
		`","event_types":["`+strings.Repeat("a", 128)+`","A-Z_0.9"]}`)
	ts.createEndpointVia(t, key, `{"url":"https://example.com","event_types":[`+manyTypes(100)+`]}`)
}

// manyTypes returns n distinct quoted event types, separated by commas.
func manyTypes(n int) string {
	types := make([]string, n)
	for i := range types {
		types[i] = fmt.Sprintf(`"t%d"`, i)
	}
	return strings.Join(types, ",")
}

func TestGetEndpoint(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)
	created := ts.createEndpointVia(t, key, `{"url":"https://example.com/hook","event_types":["order.paid"]}`)

	got := decodeEndpoint(t, ts.send(t, "GET", "/v1/endpoints/"+created.ID, key, "", nil), http.StatusOK)
	if got.Secret != nil {
		t.Errorf("secret = %q, want it left out", *got.Secret)
	}
	created.Secret = nil
	if !reflect.DeepEqual(got, created) {
		t.Errorf("get = %+v, want %+v", got, created)
	}

	decodeError(t, ts.send(t, "GET", "/v1/endpoints/ep_missing", key, "", nil), http.StatusNotFound, CodeNotFound)
	decodeError(t, ts.send(t, "GET", "/v1/endpoints/ep_missing/secret", key, "", nil), http.StatusNotFound, CodeNotFound)
}

func TestListEndpointsPagination(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)

	var want []string
	for i := range 5 {
		e := ts.createEndpointVia(t, key, fmt.Sprintf(`{"url":"https://example.com/%d"}`, i))
		want = append(want, e.ID)
	}

	all := ts.listEndpointsVia(t, key, "")
	if all.HasMore || len(all.Data) != 5 {
		t.Fatalf("list = %d endpoints, has_more %v; want 5, false", len(all.Data), all.HasMore)
	}
	for i, e := range all.Data {
		if e.ID != want[i] {
			t.Errorf("data[%d] = %s, want %s (oldest first)", i, e.ID, want[i])
		}
		if e.Secret != nil {
			t.Errorf("data[%d] has a secret", i)
		}
	}

	var got []string
	after := ""
	for page := 0; ; page++ {
		l := ts.listEndpointsVia(t, key, "?limit=2&after="+after)
		if page > 3 {
			t.Fatal("pagination does not end")
		}
		for _, e := range l.Data {
			got = append(got, e.ID)
		}
		if wantMore := len(got) < 5; l.HasMore != wantMore {
			t.Errorf("page %d: has_more = %v, want %v", page, l.HasMore, wantMore)
		}
		if !l.HasMore {
			break
		}
		after = l.Data[len(l.Data)-1].ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged IDs = %v, want %v", got, want)
	}

	// A page that ends exactly at the last endpoint has no more.
	if l := ts.listEndpointsVia(t, key, "?limit=5"); l.HasMore || len(l.Data) != 5 {
		t.Errorf("limit=5: %d endpoints, has_more %v", len(l.Data), l.HasMore)
	}
	// After the last endpoint, the list is empty, not null.
	rec := ts.send(t, "GET", "/v1/endpoints?after="+want[4], key, "", nil)
	if body := strings.TrimSpace(rec.Body.String()); body != `{"data":[],"has_more":false}` {
		t.Errorf("empty page = %s", body)
	}

	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=101", "?limit=two", "?limit=1.5"} {
		decodeError(t, ts.send(t, "GET", "/v1/endpoints"+q, key, "", nil), http.StatusBadRequest, CodeInvalidRequest)
	}
	if l := ts.listEndpointsVia(t, key, "?limit=100"); len(l.Data) != 5 {
		t.Errorf("limit=100: %d endpoints", len(l.Data))
	}
}

func TestUpdateEndpoint(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)
	e := ts.createEndpointVia(t, key, `{"url":"https://example.com/hook","description":"old","event_types":["order.paid"]}`)
	path := "/v1/endpoints/" + e.ID

	// An empty update changes nothing but updated_at.
	got := decodeEndpoint(t, ts.send(t, "PATCH", path, key, `{}`, nil), http.StatusOK)
	if got.URL != e.URL || got.Description != "old" || !reflect.DeepEqual(got.EventTypes, []string{"order.paid"}) || !got.Enabled {
		t.Errorf("after {} = %+v", got)
	}
	if got.Secret != nil {
		t.Error("PATCH returned the secret")
	}

	got = decodeEndpoint(t, ts.send(t, "PATCH", path, key,
		`{"url":"http://localhost:9000/new","description":"new","event_types":["user.created","user.created"]}`, nil), http.StatusOK)
	if got.URL != "http://localhost:9000/new" || got.Description != "new" ||
		!reflect.DeepEqual(got.EventTypes, []string{"user.created"}) || !got.Enabled {
		t.Errorf("after update = %+v", got)
	}
	if got.CreatedAt != e.CreatedAt || got.UpdatedAt.Before(e.UpdatedAt) {
		t.Errorf("created_at %v → %v, updated_at %v → %v", e.CreatedAt, got.CreatedAt, e.UpdatedAt, got.UpdatedAt)
	}

	// Only the fields given change; null leaves a field as it is.
	got = decodeEndpoint(t, ts.send(t, "PATCH", path, key, `{"description":"","url":null}`, nil), http.StatusOK)
	if got.URL != "http://localhost:9000/new" || got.Description != "" || !reflect.DeepEqual(got.EventTypes, []string{"user.created"}) {
		t.Errorf("after description-only update = %+v", got)
	}

	// An empty list subscribes to all types.
	got = decodeEndpoint(t, ts.send(t, "PATCH", path, key, `{"event_types":[]}`, nil), http.StatusOK)
	if got.EventTypes == nil || len(got.EventTypes) != 0 {
		t.Errorf("event_types = %#v, want []", got.EventTypes)
	}

	// The update is stored.
	stored := decodeEndpoint(t, ts.send(t, "GET", path, key, "", nil), http.StatusOK)
	got.Secret = nil
	if !reflect.DeepEqual(stored, got) {
		t.Errorf("stored = %+v, want %+v", stored, got)
	}

	// The secret does not change.
	rec := ts.send(t, "GET", path+"/secret", key, "", nil)
	if !strings.Contains(rec.Body.String(), *e.Secret) {
		t.Errorf("secret after update = %s, want %s", rec.Body.String(), *e.Secret)
	}

	decodeError(t, ts.send(t, "PATCH", "/v1/endpoints/ep_missing", key, `{"enabled":false}`, nil), http.StatusNotFound, CodeNotFound)
}

func TestUpdateEndpointRejects(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)
	e := ts.createEndpointVia(t, key, `{"url":"https://example.com/hook","event_types":["order.paid"]}`)
	path := "/v1/endpoints/" + e.ID

	tests := []struct {
		name, body string
		status     int
		code       string
	}{
		{"empty body", ``, 400, CodeInvalidJSON},
		{"not JSON", `enabled=false`, 400, CodeInvalidJSON},
		{"unknown field", `{"secret":"whsec_x"}`, 400, CodeInvalidRequest},
		{"enabled not a bool", `{"enabled":"no"}`, 400, CodeInvalidRequest},
		{"empty url", `{"url":""}`, 400, CodeInvalidURL},
		{"ftp url", `{"url":"ftp://example.com"}`, 400, CodeInvalidURL},
		{"relative url", `{"url":"/hook"}`, 400, CodeInvalidURL},
		{"bad event type", `{"event_types":["order paid"]}`, 400, CodeInvalidEventType},
		{"description too long", `{"description":"` + strings.Repeat("d", 1025) + `"}`, 400, CodeInvalidRequest},
		// One bad field rejects the whole update.
		{"good and bad fields", `{"enabled":false,"url":"nope"}`, 400, CodeInvalidURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decodeError(t, ts.send(t, "PATCH", path, key, tt.body, nil), tt.status, tt.code)
		})
	}

	got := decodeEndpoint(t, ts.send(t, "GET", path, key, "", nil), http.StatusOK)
	if got.URL != e.URL || !got.Enabled || !reflect.DeepEqual(got.EventTypes, e.EventTypes) {
		t.Errorf("after rejected updates = %+v, want unchanged", got)
	}
}

func TestEnableDisableEndpoint(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)
	e := ts.createEndpointVia(t, key, `{"url":"https://example.com/hook"}`)
	path := "/v1/endpoints/" + e.ID

	got := decodeEndpoint(t, ts.send(t, "PATCH", path, key, `{"enabled":false}`, nil), http.StatusOK)
	if got.Enabled {
		t.Error("enabled = true after disabling")
	}
	if got := decodeEndpoint(t, ts.send(t, "GET", path, key, "", nil), http.StatusOK); got.Enabled {
		t.Error("stored enabled = true after disabling")
	}

	// A disabled endpoint gets no deliveries (R7).
	ev := decodeEvent(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":{}}`, nil), http.StatusAccepted)
	if len(ev.Deliveries) != 0 {
		t.Errorf("deliveries to a disabled endpoint: %+v", ev.Deliveries)
	}

	// It is still listed.
	if l := ts.listEndpointsVia(t, key, ""); len(l.Data) != 1 || l.Data[0].Enabled {
		t.Errorf("list = %+v, want the disabled endpoint", l.Data)
	}

	got = decodeEndpoint(t, ts.send(t, "PATCH", path, key, `{"enabled":true}`, nil), http.StatusOK)
	if !got.Enabled {
		t.Error("enabled = false after enabling")
	}
	ev = decodeEvent(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":{}}`, nil), http.StatusAccepted)
	if len(ev.Deliveries) != 1 || ev.Deliveries[0].EndpointID != e.ID {
		t.Errorf("deliveries after enabling = %+v, want one to %s", ev.Deliveries, e.ID)
	}
}

func TestDeleteEndpoint(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)
	keep := ts.createEndpointVia(t, key, `{"url":"https://example.com/keep"}`)
	e := ts.createEndpointVia(t, key, `{"url":"https://example.com/hook"}`)
	path := "/v1/endpoints/" + e.ID

	rec := ts.send(t, "DELETE", path, key, "", nil)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("delete = %d %q, want 204 with no body", rec.Code, rec.Body.String())
	}

	// Every route now answers 404 for it.
	decodeError(t, ts.send(t, "GET", path, key, "", nil), http.StatusNotFound, CodeNotFound)
	decodeError(t, ts.send(t, "GET", path+"/secret", key, "", nil), http.StatusNotFound, CodeNotFound)
	decodeError(t, ts.send(t, "PATCH", path, key, `{"enabled":true}`, nil), http.StatusNotFound, CodeNotFound)
	decodeError(t, ts.send(t, "DELETE", path, key, "", nil), http.StatusNotFound, CodeNotFound)

	if l := ts.listEndpointsVia(t, key, ""); len(l.Data) != 1 || l.Data[0].ID != keep.ID {
		t.Errorf("list after delete = %+v, want only %s", l.Data, keep.ID)
	}

	// A deleted endpoint gets no deliveries (R7).
	ev := decodeEvent(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":{}}`, nil), http.StatusAccepted)
	if len(ev.Deliveries) != 1 || ev.Deliveries[0].EndpointID != keep.ID {
		t.Errorf("deliveries = %+v, want one to %s", ev.Deliveries, keep.ID)
	}
}

func TestEndpointsIsolatedBetweenAccounts(t *testing.T) {
	ts := newTestServer(t)
	_, key1 := ts.newKey(t)
	_, key2 := ts.newKey(t)

	e1 := ts.createEndpointVia(t, key1, `{"url":"https://example.com/one","description":"one"}`)
	e2 := ts.createEndpointVia(t, key2, `{"url":"https://example.com/two"}`)

	// Each account lists only its own endpoints.
	if l := ts.listEndpointsVia(t, key1, ""); len(l.Data) != 1 || l.Data[0].ID != e1.ID {
		t.Errorf("account 1 lists %+v", l.Data)
	}
	if l := ts.listEndpointsVia(t, key2, ""); len(l.Data) != 1 || l.Data[0].ID != e2.ID {
		t.Errorf("account 2 lists %+v", l.Data)
	}

	// Account 2 gets 404, never 403, for account 1's endpoint on every route.
	path := "/v1/endpoints/" + e1.ID
	for _, r := range []struct{ method, path, body string }{
		{"GET", path, ""},
		{"GET", path + "/secret", ""},
		{"PATCH", path, `{"enabled":false,"description":"taken"}`},
		{"DELETE", path, ""},
	} {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			rec := ts.send(t, r.method, r.path, key2, r.body, nil)
			decodeError(t, rec, http.StatusNotFound, CodeNotFound)
			if strings.Contains(rec.Body.String(), *e1.Secret) {
				t.Error("the response holds the other account's secret")
			}
		})
	}

	// Account 1's endpoint is unchanged.
	got := decodeEndpoint(t, ts.send(t, "GET", path, key1, "", nil), http.StatusOK)
	if !got.Enabled || got.Description != "one" {
		t.Errorf("account 1's endpoint = %+v, want unchanged", got)
	}
}

func TestEndpointRoutesRequireAuth(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)
	e := ts.createEndpointVia(t, key, `{"url":"https://example.com/hook"}`)

	for _, r := range []struct{ method, path string }{
		{"POST", "/v1/endpoints"},
		{"GET", "/v1/endpoints"},
		{"GET", "/v1/endpoints/" + e.ID},
		{"PATCH", "/v1/endpoints/" + e.ID},
		{"DELETE", "/v1/endpoints/" + e.ID},
		{"GET", "/v1/endpoints/" + e.ID + "/secret"},
	} {
		decodeError(t, ts.do(t, r.method, r.path, ""), http.StatusUnauthorized, CodeUnauthorized)
	}

	// Methods with no route get a JSON 405.
	decodeError(t, ts.send(t, "PUT", "/v1/endpoints/"+e.ID, key, `{}`, nil), http.StatusMethodNotAllowed, CodeMethodNotAllowed)
	decodeError(t, ts.send(t, "POST", "/v1/endpoints/"+e.ID+"/secret", key, ``, nil), http.StatusMethodNotAllowed, CodeMethodNotAllowed)
}
