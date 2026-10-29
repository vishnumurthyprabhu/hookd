package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hookd/internal/store"
)

// send makes a request with a body and extra headers.
func (ts *testServer) send(t *testing.T, method, path, key, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// newEndpoint creates an endpoint in the store and returns its ID.
func (ts *testServer) newEndpoint(t *testing.T, accountID string, types ...string) string {
	t.Helper()
	ep, _, err := ts.st.CreateEndpoint(context.Background(), accountID, store.NewEndpoint{
		URL:        "https://example.com/hook",
		EventTypes: types,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ep.ID
}

type eventJSON struct {
	ID             string          `json:"id"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
	CreatedAt      time.Time       `json:"created_at"`
	Deliveries     []struct {
		EndpointID     string `json:"endpoint_id"`
		Status         string `json:"status"`
		ResponseStatus *int   `json:"response_status"`
		Error          string `json:"error"`
	} `json:"deliveries"`
}

func decodeEvent(t *testing.T, rec *httptest.ResponseRecorder, status int) eventJSON {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	var e eventJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return e
}

// wakeServer returns a test server whose wake channel has room for many
// signals, so every signal can be counted.
func wakeServer(t *testing.T, opts ...Option) (*testServer, chan struct{}) {
	t.Helper()
	ts := newTestServer(t)
	wake := make(chan struct{}, 100)
	ts.srv = New(ts.st, ts.srv.log, append([]Option{WithWake(wake)}, opts...)...)
	return ts, wake
}

func TestCreateEvent(t *testing.T) {
	ts, wake := wakeServer(t)
	acctID, key := ts.newKey(t)
	paid := ts.newEndpoint(t, acctID, "order.paid")
	all := ts.newEndpoint(t, acctID)
	ts.newEndpoint(t, acctID, "user.created")

	payload := `{"order_id": "o_1", "amount": 1200, "note": "café"}`
	rec := ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":`+payload+`}`, nil)
	e := decodeEvent(t, rec, http.StatusAccepted)

	if !strings.HasPrefix(e.ID, "msg_") {
		t.Errorf("id = %q, want a msg_ ID", e.ID)
	}
	if e.EventType != "order.paid" {
		t.Errorf("event_type = %q", e.EventType)
	}
	if e.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/events/"+e.ID {
		t.Errorf("Location = %q", loc)
	}
	var got, want any
	json.Unmarshal(e.Payload, &got)
	json.Unmarshal([]byte(payload), &want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("payload = %s, want %s", e.Payload, payload)
	}
	if len(e.Deliveries) != 2 {
		t.Fatalf("deliveries = %+v, want 2", e.Deliveries)
	}
	targets := map[string]bool{}
	for _, d := range e.Deliveries {
		targets[d.EndpointID] = true
		if d.Status != "pending" || d.ResponseStatus != nil || d.Error != "" {
			t.Errorf("delivery = %+v, want pending with no status", d)
		}
	}
	if !targets[paid] || !targets[all] {
		t.Errorf("deliveries go to %v, want %s and %s", targets, paid, all)
	}

	// The payload is stored exactly as sent.
	stored, err := ts.st.GetEvent(context.Background(), acctID, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Payload) != payload {
		t.Errorf("stored payload = %q, want %q", stored.Payload, payload)
	}

	if n := len(wake); n != 1 {
		t.Errorf("wake signals = %d, want 1", n)
	}
}

func TestCreateEventRejects(t *testing.T) {
	ts, wake := wakeServer(t)
	_, key := ts.newKey(t)

	tests := []struct {
		name, body string
		status     int
		code       string
	}{
		{"empty body", ``, 400, CodeInvalidJSON},
		{"not JSON", `event_type=order.paid`, 400, CodeInvalidJSON},
		{"truncated JSON", `{"event_type":"order.paid","payload":{"a":`, 400, CodeInvalidJSON},
		{"invalid payload JSON", `{"event_type":"order.paid","payload":{a:1}}`, 400, CodeInvalidJSON},
		{"trailing data", `{"event_type":"order.paid","payload":{}} {}`, 400, CodeInvalidJSON},
		{"body is an array", `[]`, 400, CodeInvalidRequest},
		{"unknown field", `{"event_type":"order.paid","payload":{},"extra":1}`, 400, CodeInvalidRequest},
		{"event_type not a string", `{"event_type":5,"payload":{}}`, 400, CodeInvalidRequest},
		{"missing event_type", `{"payload":{}}`, 400, CodeInvalidEventType},
		{"event_type with a space", `{"event_type":"order paid","payload":{}}`, 400, CodeInvalidEventType},
		{"event_type with a slash", `{"event_type":"order/paid","payload":{}}`, 400, CodeInvalidEventType},
		{"event_type too long", `{"event_type":"` + strings.Repeat("a", 129) + `","payload":{}}`, 400, CodeInvalidEventType},
		{"missing payload", `{"event_type":"order.paid"}`, 400, CodeInvalidPayload},
		{"null payload", `{"event_type":"order.paid","payload":null}`, 400, CodeInvalidPayload},
		{"string payload", `{"event_type":"order.paid","payload":"hi"}`, 400, CodeInvalidPayload},
		{"array payload", `{"event_type":"order.paid","payload":[1]}`, 400, CodeInvalidPayload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decodeError(t, ts.send(t, "POST", "/v1/events", key, tt.body, nil), tt.status, tt.code)
		})
	}

	// The longest valid event type is accepted.
	decodeEvent(t, ts.send(t, "POST", "/v1/events", key,
		`{"event_type":"`+strings.Repeat("a", 128)+`","payload":{}}`, nil), http.StatusAccepted)

	decodeError(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"a","payload":{}}`,
		map[string]string{"Idempotency-Key": strings.Repeat("k", 256)}),
		http.StatusBadRequest, CodeInvalidIdempotencyKey)

	if n := len(wake); n != 1 {
		t.Errorf("wake signals = %d, want 1 (only the valid event)", n)
	}
}

func TestCreateEventPayloadSize(t *testing.T) {
	const limit = 1000
	ts, wake := wakeServer(t, WithMaxPayloadBytes(limit))
	_, key := ts.newKey(t)

	// payloadOf returns a JSON object payload exactly n bytes long.
	payloadOf := func(n int) string {
		return `{"x":"` + strings.Repeat("a", n-len(`{"x":""}`)) + `"}`
	}
	body := func(payload string) string { return `{"event_type":"big","payload":` + payload + `}` }

	decodeEvent(t, ts.send(t, "POST", "/v1/events", key, body(payloadOf(limit)), nil), http.StatusAccepted)
	decodeError(t, ts.send(t, "POST", "/v1/events", key, body(payloadOf(limit+1)), nil),
		http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
	// A body far over the limit is cut off while it is read.
	decodeError(t, ts.send(t, "POST", "/v1/events", key, body(payloadOf(100*limit)), nil),
		http.StatusRequestEntityTooLarge, CodePayloadTooLarge)

	if n := len(wake); n != 1 {
		t.Errorf("wake signals = %d, want 1", n)
	}

	// Without the option the limit is 256 KB (R9).
	ts2 := newTestServer(t)
	_, key2 := ts2.newKey(t)
	decodeEvent(t, ts2.send(t, "POST", "/v1/events", key2, body(payloadOf(256<<10)), nil), http.StatusAccepted)
	decodeError(t, ts2.send(t, "POST", "/v1/events", key2, body(payloadOf(256<<10+1)), nil),
		http.StatusRequestEntityTooLarge, CodePayloadTooLarge)
}

func TestCreateEventIdempotency(t *testing.T) {
	ts, wake := wakeServer(t)
	acctID, key := ts.newKey(t)
	ts.newEndpoint(t, acctID)
	_, otherKey := ts.newKey(t)
	idem := map[string]string{"Idempotency-Key": "order-1-paid"}

	first := decodeEvent(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":{"n":1}}`, idem), http.StatusAccepted)
	if first.IdempotencyKey != "order-1-paid" {
		t.Errorf("idempotency_key = %q", first.IdempotencyKey)
	}

	// A repeat, even with another body, returns the original event.
	rec := ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":{"n":2}}`, idem)
	again := decodeEvent(t, rec, http.StatusOK)
	if again.ID != first.ID || string(again.Payload) != `{"n":1}` {
		t.Errorf("repeat = %+v, want the first event %s", again, first.ID)
	}
	if len(again.Deliveries) != 1 {
		t.Errorf("repeat deliveries = %+v, want the original 1", again.Deliveries)
	}
	if rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Error("no Idempotent-Replayed header on the repeat")
	}
	if len(wake) != 1 {
		t.Errorf("wake signals = %d, want 1 (none for the repeat)", len(wake))
	}

	// The same key from another account, or no key, makes a new event.
	other := decodeEvent(t, ts.send(t, "POST", "/v1/events", otherKey, `{"event_type":"order.paid","payload":{}}`, idem), http.StatusAccepted)
	plain1 := decodeEvent(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":{}}`, nil), http.StatusAccepted)
	plain2 := decodeEvent(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":{}}`, nil), http.StatusAccepted)
	if seen := map[string]bool{first.ID: true, other.ID: true, plain1.ID: true, plain2.ID: true}; len(seen) != 4 {
		t.Errorf("event IDs %s %s %s %s are not all different", first.ID, other.ID, plain1.ID, plain2.ID)
	}
	if len(wake) != 4 {
		t.Errorf("wake signals = %d, want 4", len(wake))
	}
}

func TestCreateEventWakeNeverBlocks(t *testing.T) {
	ts := newTestServer(t)
	wake := make(chan struct{}) // nobody receives
	ts.srv = New(ts.st, ts.srv.log, WithWake(wake))
	_, key := ts.newKey(t)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- ts.send(t, "POST", "/v1/events", key, `{"event_type":"a","payload":{}}`, nil) }()
	select {
	case rec := <-done:
		decodeEvent(t, rec, http.StatusAccepted)
	case <-time.After(5 * time.Second):
		t.Fatal("POST /v1/events blocked on the wake channel")
	}
}

func TestGetEvent(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	ep1 := ts.newEndpoint(t, acctID, "order.paid")
	ep2 := ts.newEndpoint(t, acctID, "order.paid")
	_, otherKey := ts.newKey(t)

	created := decodeEvent(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"order.paid","payload":{"a":[1,2]}}`, nil), http.StatusAccepted)

	got := decodeEvent(t, ts.do(t, "GET", "/v1/events/"+created.ID, "Bearer "+key), http.StatusOK)
	if got.ID != created.ID || got.EventType != "order.paid" || string(got.Payload) != `{"a":[1,2]}` {
		t.Errorf("event = %+v", got)
	}
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, created.CreatedAt)
	}
	if len(got.Deliveries) != 2 {
		t.Fatalf("deliveries = %+v, want 2", got.Deliveries)
	}
	for _, d := range got.Deliveries {
		if d.Status != "pending" || d.ResponseStatus != nil {
			t.Errorf("delivery = %+v, want pending", d)
		}
	}

	// A delivery being sent still shows as pending; finished ones show their
	// outcome and status.
	ctx := context.Background()
	claim := func() store.ClaimedDelivery {
		t.Helper()
		d, ok, err := ts.st.ClaimDelivery(ctx, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim: %v %v", ok, err)
		}
		return d
	}
	d1, d2 := claim(), claim()
	byEndpoint := func() map[string]string {
		t.Helper()
		e := decodeEvent(t, ts.do(t, "GET", "/v1/events/"+created.ID, "Bearer "+key), http.StatusOK)
		m := map[string]string{}
		for _, d := range e.Deliveries {
			s := d.Status
			if d.ResponseStatus != nil {
				s += fmt.Sprintf(" %d", *d.ResponseStatus)
			}
			if d.Error != "" {
				s += " " + d.Error
			}
			m[d.EndpointID] = s
		}
		return m
	}
	if m := byEndpoint(); m[ep1] != "pending" || m[ep2] != "pending" {
		t.Errorf("in progress deliveries = %v, want pending", m)
	}
	if err := ts.st.RecordDeliveryResult(ctx, d1.ID, store.DeliveryResult{Status: store.DeliverySucceeded, ResponseStatus: 204}); err != nil {
		t.Fatal(err)
	}
	if err := ts.st.RecordDeliveryResult(ctx, d2.ID, store.DeliveryResult{Status: store.DeliveryFailed, ResponseStatus: 500, Error: "HTTP 500"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{d1.EndpointID: "succeeded 204", d2.EndpointID: "failed 500 HTTP 500"}
	if m := byEndpoint(); fmt.Sprint(m) != fmt.Sprint(want) {
		t.Errorf("deliveries = %v, want %v", m, want)
	}

	// An event with no target endpoint has an empty list.
	none := decodeEvent(t, ts.send(t, "POST", "/v1/events", key, `{"event_type":"user.created","payload":{}}`, nil), http.StatusAccepted)
	rec := ts.do(t, "GET", "/v1/events/"+none.ID, "Bearer "+key)
	if !strings.Contains(rec.Body.String(), `"deliveries":[]`) {
		t.Errorf("body = %s, want an empty deliveries list", rec.Body.String())
	}

	// Other accounts and unknown IDs get 404.
	decodeError(t, ts.do(t, "GET", "/v1/events/"+created.ID, "Bearer "+otherKey), http.StatusNotFound, CodeNotFound)
	decodeError(t, ts.do(t, "GET", "/v1/events/msg_nope", "Bearer "+key), http.StatusNotFound, CodeNotFound)
	decodeError(t, ts.do(t, "GET", "/v1/events/"+created.ID, ""), http.StatusUnauthorized, CodeUnauthorized)
}
