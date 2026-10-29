package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"hookd/internal/store"
)

type attemptJSON struct {
	ID             string    `json:"id"`
	EventID        string    `json:"event_id"`
	EndpointID     string    `json:"endpoint_id"`
	Number         int       `json:"number"`
	StartedAt      time.Time `json:"started_at"`
	Outcome        string    `json:"outcome"`
	ResponseStatus *int      `json:"response_status"`
	LatencyMS      int64     `json:"latency_ms"`
	Error          string    `json:"error"`
}

type attemptListJSON struct {
	Data    []attemptJSON `json:"data"`
	HasMore bool          `json:"has_more"`
}

func decodeAttemptList(t *testing.T, rec *httptest.ResponseRecorder) attemptListJSON {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var l attemptListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return l
}

func attemptIDs(l attemptListJSON) []string {
	out := make([]string, 0, len(l.Data))
	for _, a := range l.Data {
		out = append(out, a.ID)
	}
	return out
}

// newEvent stores an event for the account and returns its ID.
func (ts *testServer) newEvent(t *testing.T, accountID, eventType string) string {
	t.Helper()
	e, _, err := ts.st.CreateEvent(context.Background(), accountID, store.NewEvent{EventType: eventType, Payload: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return e.ID
}

// attempt claims the next due delivery and records one attempt of it with
// the given outcome and HTTP status (0 for none). If retry is set, the
// delivery is due again at once, behind those already due; otherwise the
// outcome is final.
func (ts *testServer) attempt(t *testing.T, outcome string, status int, retry bool) store.Attempt {
	t.Helper()
	ctx := context.Background()
	d, ok, err := ts.st.ClaimDelivery(ctx, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	r := store.AttemptResult{
		Number:         d.Attempts + 1,
		Outcome:        outcome,
		ResponseStatus: status,
		Latency:        42 * time.Millisecond,
		StartedAt:      time.Now(),
	}
	if outcome == store.AttemptFailed {
		r.Error = "non-2xx response"
		if status == 0 {
			r.Error = "connection refused"
		}
	}
	if retry {
		next := time.Now()
		r.NextAttemptAt = &next
	}
	a, err := ts.st.RecordAttempt(ctx, d.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGetEventDeliveryAttemptFields(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	ts.newEndpoint(t, acctID)
	eventID := ts.newEvent(t, acctID, "order.paid")

	get := func() map[string]any {
		t.Helper()
		rec := ts.do(t, "GET", "/v1/events/"+eventID, "Bearer "+key)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
		}
		var e struct {
			Deliveries []map[string]any `json:"deliveries"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if len(e.Deliveries) != 1 {
			t.Fatalf("deliveries = %v, want 1", e.Deliveries)
		}
		return e.Deliveries[0]
	}

	// A new delivery: no attempts, both times present and null.
	d := get()
	if d["attempts"] != float64(0) || d["last_attempt_at"] != nil || d["next_attempt_at"] != nil {
		t.Errorf("new delivery = %v, want 0 attempts and null times", d)
	}
	for _, k := range []string{"last_attempt_at", "next_attempt_at"} {
		if _, ok := d[k]; !ok {
			t.Errorf("new delivery has no %q field: %v", k, d)
		}
	}

	// A failed attempt with a retry waiting: still pending, with both times.
	ctx := context.Background()
	c, ok, err := ts.st.ClaimDelivery(ctx, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	started := time.Date(2026, 10, 29, 10, 0, 5, 123e6, time.UTC)
	next := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	if _, err := ts.st.RecordAttempt(ctx, c.ID, store.AttemptResult{
		Number: 1, Outcome: store.AttemptFailed, ResponseStatus: 500, Error: "non-2xx response: 500",
		StartedAt: started, NextAttemptAt: &next,
	}); err != nil {
		t.Fatal(err)
	}
	d = get()
	if d["status"] != "pending" || d["attempts"] != float64(1) {
		t.Errorf("waiting delivery = %v, want pending with 1 attempt", d)
	}
	if got, _ := time.Parse(time.RFC3339Nano, d["last_attempt_at"].(string)); !got.Equal(started) {
		t.Errorf("last_attempt_at = %v, want %v", d["last_attempt_at"], started)
	}
	if got, _ := time.Parse(time.RFC3339Nano, d["next_attempt_at"].(string)); !got.Equal(next) {
		t.Errorf("next_attempt_at = %v, want %v", d["next_attempt_at"], next)
	}
	if s, _ := d["next_attempt_at"].(string); !strings.HasSuffix(s, "Z") {
		t.Errorf("next_attempt_at = %q, want UTC", s)
	}
}

func TestGetEventDeliveryAfterFinalAttempt(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	ts.newEndpoint(t, acctID)
	eventID := ts.newEvent(t, acctID, "order.paid")
	ts.attempt(t, store.AttemptFailed, 500, true)
	ts.attempt(t, store.AttemptSucceeded, 200, false)

	rec := ts.do(t, "GET", "/v1/events/"+eventID, "Bearer "+key)
	var e struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || len(e.Deliveries) != 1 {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	d := e.Deliveries[0]
	if d["status"] != "succeeded" || d["attempts"] != float64(2) || d["last_attempt_at"] == nil || d["next_attempt_at"] != nil {
		t.Errorf("delivery = %v, want succeeded after 2 attempts with no next attempt", d)
	}
}

func TestListEventAttempts(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	ep1 := ts.newEndpoint(t, acctID)
	ep2 := ts.newEndpoint(t, acctID)
	eventID := ts.newEvent(t, acctID, "order.paid")

	// Two rounds of failures for both endpoints, then a success and a final
	// failure: six attempts, numbered 1 to 3 per endpoint.
	var all []store.Attempt
	for range 2 {
		all = append(all, ts.attempt(t, store.AttemptFailed, 500, true), ts.attempt(t, store.AttemptFailed, 0, true))
	}
	all = append(all, ts.attempt(t, store.AttemptSucceeded, 204, false), ts.attempt(t, store.AttemptFailed, 503, false))
	var wantAll, wantEp1 []string
	for _, a := range all {
		wantAll = append(wantAll, a.ID)
		if a.EndpointID == ep1 {
			wantEp1 = append(wantEp1, a.ID)
		}
	}

	path := "/v1/events/" + eventID + "/attempts"
	l := decodeAttemptList(t, ts.do(t, "GET", path, "Bearer "+key))
	if got := attemptIDs(l); !slices.Equal(got, wantAll) || l.HasMore {
		t.Errorf("attempts = %v has_more %v, want %v oldest first and no more", got, l.HasMore, wantAll)
	}

	// The JSON shape of one attempt.
	first := l.Data[0]
	if first.EventID != eventID || first.Number != 1 || first.Outcome != "failed" ||
		first.ResponseStatus == nil || *first.ResponseStatus != 500 || first.LatencyMS != 42 ||
		first.Error == "" || first.StartedAt.IsZero() {
		t.Errorf("first attempt = %+v", first)
	}
	if l.Data[1].ResponseStatus != nil {
		t.Errorf("attempt with no response: response_status = %v, want null", *l.Data[1].ResponseStatus)
	}
	rec := ts.do(t, "GET", path, "Bearer "+key)
	if !strings.Contains(rec.Body.String(), `"response_status":null`) {
		t.Errorf("body %s has no null response_status", rec.Body.String())
	}

	// Filter by endpoint.
	l = decodeAttemptList(t, ts.do(t, "GET", path+"?endpoint_id="+ep1, "Bearer "+key))
	if got := attemptIDs(l); !slices.Equal(got, wantEp1) {
		t.Errorf("attempts to ep1 = %v, want %v", got, wantEp1)
	}
	for _, a := range l.Data {
		if a.EndpointID != ep1 {
			t.Errorf("attempt %s is to %s, want %s", a.ID, a.EndpointID, ep1)
		}
	}
	l = decodeAttemptList(t, ts.do(t, "GET", path+"?endpoint_id="+ep2+"&limit=1", "Bearer "+key))
	if len(l.Data) != 1 || l.Data[0].EndpointID != ep2 || !l.HasMore {
		t.Errorf("first attempt to ep2 = %+v, want one with more", l)
	}

	// Paging with limit and after.
	var paged []string
	after := ""
	for pages := 0; ; pages++ {
		if pages > 3 {
			t.Fatal("too many pages")
		}
		q := path + "?limit=4"
		if after != "" {
			q += "&after=" + after
		}
		l := decodeAttemptList(t, ts.do(t, "GET", q, "Bearer "+key))
		paged = append(paged, attemptIDs(l)...)
		if !l.HasMore {
			if pages != 1 || len(l.Data) != 2 {
				t.Errorf("last page = %d attempts on page %d, want 2 on page 1", len(l.Data), pages)
			}
			break
		}
		after = l.Data[len(l.Data)-1].ID
	}
	if !slices.Equal(paged, wantAll) {
		t.Errorf("paged attempts = %v, want %v", paged, wantAll)
	}

	// An event with no attempts has an empty list.
	other := ts.newEvent(t, acctID, "order.paid")
	rec = ts.do(t, "GET", "/v1/events/"+other+"/attempts", "Bearer "+key)
	if l := decodeAttemptList(t, rec); len(l.Data) != 0 || l.HasMore || !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("attempts of a new event = %s, want an empty list", rec.Body.String())
	}
}

func TestListEndpointAttempts(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	ep := ts.newEndpoint(t, acctID, "order.paid")
	otherEp := ts.newEndpoint(t, acctID, "user.created")

	// Three events to ep: failed then succeeded, failed then failed, and
	// succeeded.
	var all []store.Attempt
	ts.newEvent(t, acctID, "order.paid")
	all = append(all, ts.attempt(t, store.AttemptFailed, 500, true), ts.attempt(t, store.AttemptSucceeded, 200, false))
	ts.newEvent(t, acctID, "order.paid")
	all = append(all, ts.attempt(t, store.AttemptFailed, 500, true), ts.attempt(t, store.AttemptFailed, 502, false))
	ts.newEvent(t, acctID, "order.paid")
	all = append(all, ts.attempt(t, store.AttemptSucceeded, 204, false))
	// An attempt to the other endpoint is not listed.
	ts.newEvent(t, acctID, "user.created")
	if a := ts.attempt(t, store.AttemptSucceeded, 200, false); a.EndpointID != otherEp {
		t.Fatalf("attempt went to %s, want %s", a.EndpointID, otherEp)
	}

	newestFirst := func(outcome string) []string {
		var out []string
		for _, a := range slices.Backward(all) {
			if outcome == "" || a.Outcome == outcome {
				out = append(out, a.ID)
			}
		}
		return out
	}

	path := "/v1/endpoints/" + ep + "/attempts"
	l := decodeAttemptList(t, ts.do(t, "GET", path, "Bearer "+key))
	if got, want := attemptIDs(l), newestFirst(""); !slices.Equal(got, want) || l.HasMore {
		t.Errorf("attempts = %v has_more %v, want %v newest first", got, l.HasMore, want)
	}
	for _, a := range l.Data {
		if a.Outcome == "succeeded" && a.Error != "" {
			t.Errorf("succeeded attempt has error %q", a.Error)
		}
	}
	rec := ts.do(t, "GET", path+"?outcome=succeeded&limit=1", "Bearer "+key)
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("succeeded attempt %s has an error field", rec.Body.String())
	}

	for _, outcome := range []string{"succeeded", "failed"} {
		l := decodeAttemptList(t, ts.do(t, "GET", path+"?outcome="+outcome, "Bearer "+key))
		if got, want := attemptIDs(l), newestFirst(outcome); !slices.Equal(got, want) {
			t.Errorf("%s attempts = %v, want %v", outcome, got, want)
		}
	}

	// Paging with limit and before, with the filter.
	failed := newestFirst("failed")
	l = decodeAttemptList(t, ts.do(t, "GET", path+"?outcome=failed&limit=2", "Bearer "+key))
	if got := attemptIDs(l); !slices.Equal(got, failed[:2]) || !l.HasMore {
		t.Errorf("first page = %v has_more %v, want %v with more", got, l.HasMore, failed[:2])
	}
	l = decodeAttemptList(t, ts.do(t, "GET", path+"?outcome=failed&limit=2&before="+l.Data[1].ID, "Bearer "+key))
	if got := attemptIDs(l); !slices.Equal(got, failed[2:]) || l.HasMore {
		t.Errorf("second page = %v has_more %v, want %v and no more", got, l.HasMore, failed[2:])
	}

	// A deleted endpoint's attempts are still listed.
	if err := ts.st.DeleteEndpoint(context.Background(), acctID, ep); err != nil {
		t.Fatal(err)
	}
	decodeError(t, ts.do(t, "GET", "/v1/endpoints/"+ep, "Bearer "+key), http.StatusNotFound, CodeNotFound)
	l = decodeAttemptList(t, ts.do(t, "GET", path, "Bearer "+key))
	if got, want := attemptIDs(l), newestFirst(""); !slices.Equal(got, want) {
		t.Errorf("deleted endpoint's attempts = %v, want %v", got, want)
	}
}

func TestGetAttempt(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	ep := ts.newEndpoint(t, acctID)
	eventID := ts.newEvent(t, acctID, "order.paid")
	failed := ts.attempt(t, store.AttemptFailed, 0, true)
	ok := ts.attempt(t, store.AttemptSucceeded, 201, false)

	rec := ts.do(t, "GET", "/v1/attempts/"+failed.ID, "Bearer "+key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"endpoint_id", "error", "event_id", "id", "latency_ms", "number", "outcome", "response_status", "started_at"}
	if !slices.Equal(keys, want) {
		t.Errorf("fields = %v, want %v", keys, want)
	}
	var a attemptJSON
	json.Unmarshal(rec.Body.Bytes(), &a)
	if a.ID != failed.ID || a.EventID != eventID || a.EndpointID != ep || a.Number != 1 ||
		a.Outcome != "failed" || a.ResponseStatus != nil || a.Error != "connection refused" || a.LatencyMS != 42 {
		t.Errorf("attempt = %+v", a)
	}
	if !a.StartedAt.Equal(failed.StartedAt) {
		t.Errorf("started_at = %v, want %v", a.StartedAt, failed.StartedAt)
	}

	rec = ts.do(t, "GET", "/v1/attempts/"+ok.ID, "Bearer "+key)
	json.Unmarshal(rec.Body.Bytes(), &a)
	if a.Number != 2 || a.Outcome != "succeeded" || a.ResponseStatus == nil || *a.ResponseStatus != 201 {
		t.Errorf("attempt = %+v", a)
	}
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("succeeded attempt %s has an error field", rec.Body.String())
	}

	decodeError(t, ts.do(t, "GET", "/v1/attempts/att_nope", "Bearer "+key), http.StatusNotFound, CodeNotFound)
}

func TestAttemptRoutesRejectBadQueries(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	ep := ts.newEndpoint(t, acctID)
	eventID := ts.newEvent(t, acctID, "order.paid")
	ts.attempt(t, store.AttemptFailed, 500, false)

	for _, path := range []string{
		"/v1/endpoints/" + ep + "/attempts?outcome=pending",
		"/v1/endpoints/" + ep + "/attempts?outcome=SUCCEEDED",
		"/v1/endpoints/" + ep + "/attempts?limit=0",
		"/v1/endpoints/" + ep + "/attempts?limit=101",
		"/v1/endpoints/" + ep + "/attempts?limit=ten",
		"/v1/events/" + eventID + "/attempts?limit=0",
		"/v1/events/" + eventID + "/attempts?limit=-1",
		"/v1/events/" + eventID + "/attempts?limit=101",
		"/v1/events/" + eventID + "/attempts?limit=1.5",
	} {
		decodeError(t, ts.do(t, "GET", path, "Bearer "+key), http.StatusBadRequest, CodeInvalidRequest)
	}

	// The largest limit is accepted.
	decodeAttemptList(t, ts.do(t, "GET", "/v1/events/"+eventID+"/attempts?limit=100", "Bearer "+key))
	decodeAttemptList(t, ts.do(t, "GET", "/v1/endpoints/"+ep+"/attempts?limit=100", "Bearer "+key))
}

func TestAttemptsIsolatedBetweenAccounts(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	_, otherKey := ts.newKey(t)
	ep := ts.newEndpoint(t, acctID)
	eventID := ts.newEvent(t, acctID, "order.paid")
	a := ts.attempt(t, store.AttemptFailed, 500, false)

	for _, path := range []string{
		"/v1/events/" + eventID + "/attempts",
		"/v1/endpoints/" + ep + "/attempts",
		"/v1/attempts/" + a.ID,
		"/v1/events/msg_nope/attempts",
		"/v1/endpoints/ep_nope/attempts",
	} {
		decodeError(t, ts.do(t, "GET", path, "Bearer "+otherKey), http.StatusNotFound, CodeNotFound)
	}
	// The owner still sees them.
	if l := decodeAttemptList(t, ts.do(t, "GET", "/v1/events/"+eventID+"/attempts", "Bearer "+key)); len(l.Data) != 1 {
		t.Errorf("owner's attempts = %+v, want 1", l)
	}
}

func TestAttemptRoutesRequireAuth(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	ep := ts.newEndpoint(t, acctID)
	eventID := ts.newEvent(t, acctID, "order.paid")
	a := ts.attempt(t, store.AttemptFailed, 500, false)

	for _, path := range []string{
		"/v1/events/" + eventID + "/attempts",
		"/v1/endpoints/" + ep + "/attempts",
		"/v1/attempts/" + a.ID,
	} {
		decodeError(t, ts.do(t, "GET", path, ""), http.StatusUnauthorized, CodeUnauthorized)
		decodeError(t, ts.do(t, "GET", path, "Bearer hk_wrong"), http.StatusUnauthorized, CodeUnauthorized)
		decodeError(t, ts.send(t, "POST", path, key, `{}`, nil), http.StatusMethodNotAllowed, CodeMethodNotAllowed)
	}
}
