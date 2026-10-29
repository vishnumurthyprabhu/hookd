package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
)

// receivedRequest is one webhook request seen by a test receiver.
type receivedRequest struct {
	header http.Header
	body   []byte
}

// newReceiver starts an httptest server that answers every request with status
// and passes a copy of it to the returned channel.
func newReceiver(t *testing.T, status int) (*httptest.Server, <-chan receivedRequest) {
	t.Helper()
	got := make(chan receivedRequest, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- receivedRequest{header: r.Header.Clone(), body: body}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// apiClient calls the Hookd API with one account's key.
type apiClient struct {
	t    *testing.T
	base string
	key  string
}

// do sends a request with body encoded as JSON (if not nil) and decodes the
// JSON response into out (if not nil). It returns the response status.
func (c apiClient) do(method, path string, body, out any) int {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp.StatusCode
}

type testEndpoint struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	EventTypes []string `json:"event_types"`
	Enabled    bool     `json:"enabled"`
	Secret     string   `json:"secret"`
}

type testEvent struct {
	ID         string `json:"id"`
	Deliveries []struct {
		EndpointID     string `json:"endpoint_id"`
		Status         string `json:"status"`
		ResponseStatus *int   `json:"response_status"`
	} `json:"deliveries"`
}

func (c apiClient) createEndpoint(url, eventType string) testEndpoint {
	c.t.Helper()
	var e testEndpoint
	body := map[string]any{"url": url, "event_types": []string{eventType}}
	if status := c.do("POST", "/v1/endpoints", body, &e); status != http.StatusCreated {
		c.t.Fatalf("POST /v1/endpoints: status %d, want 201", status)
	}
	if e.ID == "" || e.Secret == "" {
		c.t.Fatalf("POST /v1/endpoints returned %+v, want an id and a secret", e)
	}
	return e
}

func (c apiClient) sendEvent(eventType, payload string) testEvent {
	c.t.Helper()
	var e testEvent
	body := map[string]any{"event_type": eventType, "payload": json.RawMessage(payload)}
	if status := c.do("POST", "/v1/events", body, &e); status != http.StatusAccepted {
		c.t.Fatalf("POST /v1/events: status %d, want 202", status)
	}
	return e
}

// waitDelivered polls GET /v1/events/{id} until no delivery is pending.
func (c apiClient) waitDelivered(id string) testEvent {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var e testEvent
		if status := c.do("GET", "/v1/events/"+id, nil, &e); status != http.StatusOK {
			c.t.Fatalf("GET /v1/events/%s: status %d, want 200", id, status)
		}
		pending := false
		for _, d := range e.Deliveries {
			pending = pending || d.Status == "pending"
		}
		if !pending {
			return e
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("event %s still has pending deliveries: %+v", id, e.Deliveries)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// createAccount runs "hookd account create" and returns the printed API key.
func createAccount(t *testing.T, name string) string {
	t.Helper()
	out := runOK(t, "account", "create", "--name", name)
	m := regexp.MustCompile(`(?m)^api_key: +(\S+)$`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("account create printed %q", out)
	}
	return m[1]
}

// startServe runs "hookd serve" in the background on a free port and returns
// the API's base URL once /healthz answers. The server stops at test cleanup.
func startServe(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	t.Setenv("HOOKD_LISTEN_ADDR", addr)

	ctx, cancel := context.WithCancel(context.Background())
	setSignalContext(t, func() (context.Context, context.CancelFunc) { return ctx, cancel })
	done := make(chan int, 1)
	var stderr bytes.Buffer
	go func() { done <- run([]string{"serve"}, io.Discard, &stderr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("serve exit code %d, stderr %q", code, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop after its context was cancelled")
		}
	})

	base := "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become healthy: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAcceptance follows the acceptance steps of requirements.md against the
// whole service, run in-process on a fresh SQLite database.
func TestAcceptance(t *testing.T) {
	useTempDB(t)
	t.Setenv("HOOKD_POLL_INTERVAL", "50ms")
	t.Setenv("HOOKD_LOG_LEVEL", "error")

	// 1. The operator creates an account and gets an API key.
	runOK(t, "migrate")
	alice := apiClient{t: t, key: createAccount(t, "alice")}
	bob := apiClient{t: t, key: createAccount(t, "bob")}
	base := startServe(t)
	alice.base, bob.base = base, base

	// 2. Two endpoints on test receivers: one for order.paid, one for user.created.
	orders, orderReqs := newReceiver(t, http.StatusOK)
	users, userReqs := newReceiver(t, http.StatusOK)
	orderEP := alice.createEndpoint(orders.URL+"/hooks", "order.paid")
	userEP := alice.createEndpoint(users.URL+"/hooks", "user.created")

	// 3. Sending order.paid reaches only the first endpoint.
	payload := `{"order_id":"ord_123","amount":4200}`
	sent := alice.sendEvent("order.paid", payload)
	ev := alice.waitDelivered(sent.ID)

	var got receivedRequest
	select {
	case got = <-orderReqs:
	case <-time.After(5 * time.Second):
		t.Fatal("order.paid receiver was not called")
	}
	select {
	case r := <-userReqs:
		t.Fatalf("user.created receiver was called with %q", r.body)
	default:
	}
	select {
	case r := <-orderReqs:
		t.Fatalf("order.paid receiver was called a second time with %q", r.body)
	default:
	}

	// 4. The request verifies with the official library and the endpoint's secret.
	wh, err := standardwebhooks.NewWebhook(orderEP.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := wh.Verify(got.body, got.header); err != nil {
		t.Errorf("official Standard Webhooks library rejected the delivery: %v", err)
	}
	if string(got.body) != payload {
		t.Errorf("delivered body = %q, want %q", got.body, payload)
	}
	if id := got.header.Get("Webhook-Id"); id != sent.ID {
		t.Errorf("Webhook-Id = %q, want event id %q", id, sent.ID)
	}
	otherWH, err := standardwebhooks.NewWebhook(userEP.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := otherWH.Verify(got.body, got.header); err == nil {
		t.Error("delivery also verified with another endpoint's secret")
	}

	// 5. Fetching the event shows delivery succeeded for the first endpoint.
	if len(ev.Deliveries) != 1 {
		t.Fatalf("event deliveries = %+v, want exactly one", ev.Deliveries)
	}
	if d := ev.Deliveries[0]; d.EndpointID != orderEP.ID || d.Status != "succeeded" ||
		d.ResponseStatus == nil || *d.ResponseStatus != http.StatusOK {
		t.Errorf("delivery = {%s %s %v}, want {%s succeeded 200}",
			d.EndpointID, d.Status, d.ResponseStatus, orderEP.ID)
	}

	// 6. An endpoint whose receiver returns 500 shows as failed, with status 500.
	broken, brokenReqs := newReceiver(t, http.StatusInternalServerError)
	brokenEP := alice.createEndpoint(broken.URL, "invoice.created")
	failedSent := alice.sendEvent("invoice.created", `{"invoice_id":"inv_1"}`)
	failed := alice.waitDelivered(failedSent.ID)
	select {
	case <-brokenReqs:
	default:
		t.Error("500 receiver was not called")
	}
	if len(failed.Deliveries) != 1 {
		t.Fatalf("failed event deliveries = %+v, want exactly one", failed.Deliveries)
	}
	if d := failed.Deliveries[0]; d.EndpointID != brokenEP.ID || d.Status != "failed" ||
		d.ResponseStatus == nil || *d.ResponseStatus != http.StatusInternalServerError {
		t.Errorf("delivery = {%s %s %v}, want {%s failed 500}",
			d.EndpointID, d.Status, d.ResponseStatus, brokenEP.ID)
	}

	// 7. Another account's key cannot see or change the first account's
	// endpoints or events.
	notFound := []struct {
		method, path string
		body         any
	}{
		{"GET", "/v1/endpoints/" + orderEP.ID, nil},
		{"GET", "/v1/endpoints/" + orderEP.ID + "/secret", nil},
		{"PATCH", "/v1/endpoints/" + orderEP.ID, map[string]any{"enabled": false, "url": "http://example.com/stolen"}},
		{"DELETE", "/v1/endpoints/" + orderEP.ID, nil},
		{"GET", "/v1/events/" + sent.ID, nil},
		{"GET", "/v1/events/" + failedSent.ID, nil},
	}
	for _, c := range notFound {
		if status := bob.do(c.method, c.path, c.body, nil); status != http.StatusNotFound {
			t.Errorf("second account: %s %s: status %d, want 404", c.method, c.path, status)
		}
	}
	var bobList struct {
		Data []testEndpoint `json:"data"`
	}
	if status := bob.do("GET", "/v1/endpoints", nil, &bobList); status != http.StatusOK || len(bobList.Data) != 0 {
		t.Errorf("second account: GET /v1/endpoints: status %d, data %+v, want 200 and none", status, bobList.Data)
	}
	var after testEndpoint
	if status := alice.do("GET", "/v1/endpoints/"+orderEP.ID, nil, &after); status != http.StatusOK {
		t.Fatalf("first account: GET its endpoint after second account's requests: status %d", status)
	}
	if after.URL != orderEP.URL || !after.Enabled || strings.Join(after.EventTypes, ",") != "order.paid" {
		t.Errorf("first account's endpoint changed to %+v, want it as created %+v", after, orderEP)
	}
}
