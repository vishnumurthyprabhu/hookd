package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hookd/internal/config"
	"hookd/internal/retention"
	"hookd/internal/store"
)

// adminPost posts form to the admin UI at base+path, follows the redirect
// and returns the path of the page it ends on. It does not stop the test, so
// it can be called from a receiver's handler.
func adminPost(base, path string, form url.Values) (string, error) {
	resp, err := http.PostForm(base+path, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("POST %s: status %d, body %q", path, resp.StatusCode, body)
	}
	return resp.Request.URL.Path, nil
}

// createTestEndpoint creates, in the admin UI, a test endpoint for eventType
// whose built-in receiver answers status, and returns its ID.
func createTestEndpoint(t *testing.T, base, accountID, eventType string, status int) string {
	t.Helper()
	page, err := adminPost(base, "/admin/accounts/"+accountID+"/test-endpoints/new", url.Values{
		"description": {"test endpoint for " + eventType},
		"event_types": {eventType},
		"status":      {fmt.Sprint(status)},
	})
	if err != nil {
		t.Fatal(err)
	}
	prefix := "/admin/accounts/" + accountID + "/endpoints/"
	if !strings.HasPrefix(page, prefix) {
		t.Fatalf("creating a test endpoint led to %q, want an endpoint page", page)
	}
	return strings.TrimPrefix(page, prefix)
}

// setTestStatus sets the status a test endpoint's receiver answers with, in
// the admin UI.
func setTestStatus(base, accountID, endpointID string, status int) error {
	_, err := adminPost(base, "/admin/accounts/"+accountID+"/endpoints/"+endpointID+"/test-status",
		url.Values{"status": {fmt.Sprint(status)}})
	return err
}

// setRetrySchedule saves HOOKD_RETRY_SCHEDULE in the admin UI.
func setRetrySchedule(t *testing.T, base, value string) {
	t.Helper()
	if _, err := adminPost(base, "/admin/settings/HOOKD_RETRY_SCHEDULE", url.Values{"value": {value}}); err != nil {
		t.Fatal(err)
	}
}

// newTap starts a server that passes each request on to target and answers
// with target's answer. Before it answers request n (1, 2, …), it calls
// before(n), so a test can change things between an attempt and the
// worker's record of it.
func newTap(t *testing.T, target string, before func(n int)) *httptest.Server {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, err := http.NewRequest(r.Method, target, bytes.NewReader(body))
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("tap: %v", err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		before(int(count.Add(1)))
		w.WriteHeader(resp.StatusCode)
		w.Write(answer)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type testAttempt struct {
	ID             string    `json:"id"`
	EventID        string    `json:"event_id"`
	EndpointID     string    `json:"endpoint_id"`
	Number         int       `json:"number"`
	StartedAt      time.Time `json:"started_at"`
	Outcome        string    `json:"outcome"`
	ResponseStatus *int      `json:"response_status"`
	LatencyMS      *int64    `json:"latency_ms"`
	Error          string    `json:"error"`
}

type testAttemptList struct {
	Data    []testAttempt `json:"data"`
	HasMore bool          `json:"has_more"`
}

func (c apiClient) attempts(path string) []testAttempt {
	c.t.Helper()
	var l testAttemptList
	if status := c.do("GET", path, nil, &l); status != http.StatusOK {
		c.t.Fatalf("GET %s: status %d, want 200", path, status)
	}
	return l.Data
}

// waitEvent polls GET /v1/events/{id} until ok is true of it.
func (c apiClient) waitEvent(id, what string, ok func(testEvent) bool) testEvent {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var e testEvent
		if status := c.do("GET", "/v1/events/"+id, nil, &e); status != http.StatusOK {
			c.t.Fatalf("GET /v1/events/%s: status %d, want 200", id, status)
		}
		if ok(e) {
			return e
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("event %s: %s did not happen: %+v", id, what, e.Deliveries)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// onlyDelivery returns the event's one delivery.
func onlyDelivery(t *testing.T, e testEvent) (status string, attempts int, errText string, next *time.Time) {
	t.Helper()
	if len(e.Deliveries) != 1 {
		t.Fatalf("event %s deliveries = %+v, want exactly one", e.ID, e.Deliveries)
	}
	d := e.Deliveries[0]
	return d.Status, d.Attempts, d.Error, d.NextAttemptAt
}

// receivedRequests returns the requests a test endpoint's built-in receiver
// got, oldest first.
func receivedRequests(t *testing.T, st store.Store, accountID, endpointID string) []store.TestRequest {
	t.Helper()
	reqs, err := st.ListTestRequests(context.Background(), accountID, endpointID, store.MaxTestRequests)
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(reqs)-1; i < j; i, j = i+1, j-1 {
		reqs[i], reqs[j] = reqs[j], reqs[i]
	}
	return reqs
}

// checkReceived checks that the receiver got n requests for eventID, each with
// it as webhook-id and a signature the official library verified.
func checkReceived(t *testing.T, reqs []store.TestRequest, n int, eventID string) {
	t.Helper()
	if len(reqs) != n {
		t.Fatalf("receiver got %d requests, want %d", len(reqs), n)
	}
	for i, r := range reqs {
		if r.WebhookID != eventID {
			t.Errorf("request %d: webhook-id %q, want %q", i+1, r.WebhookID, eventID)
		}
		if !r.Verified {
			t.Errorf("request %d: signature not verified: %s", i+1, r.VerifyError)
		}
	}
}

// TestRetriesAcceptance follows the acceptance steps of Hookd 2/6's
// requirements.md (docs/changes/003-hookd-2-6-retries-and-delivery-history)
// against the whole service, run in-process on a fresh SQLite database, with
// the admin UI's built-in test receiver and a retry schedule of milliseconds.
func TestRetriesAcceptance(t *testing.T) {
	dbPath := useTempDB(t)
	t.Setenv("HOOKD_POLL_INTERVAL", "10ms")
	t.Setenv("HOOKD_LOG_LEVEL", "error")
	t.Setenv("HOOKD_ADMIN_UI", "true")

	runOK(t, "migrate")
	aliceID, aliceKey := createAccountWithID(t, "alice")
	alice := apiClient{t: t, key: aliceKey}
	bob := apiClient{t: t, key: createAccount(t, "bob")}

	// The test reads the receivers' requests, and adds an old event, through
	// a store of its own on the same database.
	dbCfg := config.Default().DB
	dbCfg.Path = dbPath
	st, err := store.Open(context.Background(), dbCfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	srv := startServeAt(t, addr)
	base := srv.base
	alice.base, bob.base = base, base

	// 1. The admin sets the retry schedule in the admin UI.
	setRetrySchedule(t, base, "5ms,10ms,20ms")

	// 2. A test endpoint answering 500, reached through a tap that switches
	// it to 200 (step 4) once its second request has been answered with 500
	// and before the worker records that attempt, so the third attempt is
	// the first to find it at 200.
	orderEP := createTestEndpoint(t, base, aliceID, "order.paid", http.StatusInternalServerError)
	var ep testEndpoint
	if status := alice.do("GET", "/v1/endpoints/"+orderEP, nil, &ep); status != http.StatusOK {
		t.Fatalf("GET /v1/endpoints/%s: status %d", orderEP, status)
	}
	tap := newTap(t, ep.URL, func(n int) {
		if n == 2 {
			if err := setTestStatus(base, aliceID, orderEP, http.StatusOK); err != nil {
				t.Error(err)
			}
		}
	})
	if status := alice.do("PATCH", "/v1/endpoints/"+orderEP, map[string]any{"url": tap.URL + "/hooks"}, nil); status != http.StatusOK {
		t.Fatalf("PATCH /v1/endpoints/%s: status %d", orderEP, status)
	}
	sent := alice.sendEvent("order.paid", `{"order_id":"ord_123"}`)

	// 3–4. Two failed attempts, then success on the third; nothing more.
	ev := alice.waitDelivered(sent.ID)
	if status, n, _, next := onlyDelivery(t, ev); status != "succeeded" || n != 3 || next != nil {
		t.Errorf("delivery = %s after %d attempts, next %v; want succeeded after 3, no next", status, n, next)
	}
	if d := ev.Deliveries[0]; d.ResponseStatus == nil || *d.ResponseStatus != http.StatusOK || d.LastAttemptAt == nil {
		t.Errorf("delivery response_status %v, last_attempt_at %v; want 200 and a time", d.ResponseStatus, d.LastAttemptAt)
	}
	time.Sleep(100 * time.Millisecond)
	checkReceived(t, receivedRequests(t, st, aliceID, orderEP), 3, sent.ID)

	// 5. The event's attempts, oldest first, and the endpoint's, newest
	// first and filtered to the failed ones.
	evAttempts := alice.attempts("/v1/events/" + sent.ID + "/attempts")
	want := []struct {
		outcome string
		status  int
	}{{"failed", 500}, {"failed", 500}, {"succeeded", 200}}
	if len(evAttempts) != len(want) {
		t.Fatalf("event attempts = %+v, want %d", evAttempts, len(want))
	}
	for i, a := range evAttempts {
		w := want[i]
		if a.Number != i+1 || a.Outcome != w.outcome || a.ResponseStatus == nil || *a.ResponseStatus != w.status ||
			a.EventID != sent.ID || a.EndpointID != orderEP || a.LatencyMS == nil || *a.LatencyMS < 0 {
			t.Errorf("event attempt %d = %+v, want number %d, %s, %d", i, a, i+1, w.outcome, w.status)
		}
		if (a.Error != "") != (w.outcome == "failed") {
			t.Errorf("event attempt %d error = %q, want one only if it failed", i, a.Error)
		}
	}
	epAttempts := alice.attempts("/v1/endpoints/" + orderEP + "/attempts")
	if len(epAttempts) != 3 {
		t.Fatalf("endpoint attempts = %+v, want 3", epAttempts)
	}
	for i, a := range epAttempts {
		if a.ID != evAttempts[2-i].ID {
			t.Errorf("endpoint attempt %d = %s, want %s (newest first)", i, a.ID, evAttempts[2-i].ID)
		}
	}
	failedOnly := alice.attempts("/v1/endpoints/" + orderEP + "/attempts?outcome=failed")
	if len(failedOnly) != 2 || failedOnly[0].ID != evAttempts[1].ID || failedOnly[1].ID != evAttempts[0].ID {
		t.Errorf("failed endpoint attempts = %+v, want attempts 2 and 1", failedOnly)
	}
	var one testAttempt
	if status := alice.do("GET", "/v1/attempts/"+evAttempts[0].ID, nil, &one); status != http.StatusOK ||
		one.ID != evAttempts[0].ID || one.Number != 1 || one.Outcome != "failed" || one.Error != evAttempts[0].Error {
		t.Errorf("GET /v1/attempts/%s: status %d, %+v; want 200, %+v", evAttempts[0].ID, status, one, evAttempts[0])
	}

	// 6. An endpoint that keeps answering 500 ends failed after 4 attempts.
	brokenEP := createTestEndpoint(t, base, aliceID, "invoice.created", http.StatusInternalServerError)
	brokenSent := alice.sendEvent("invoice.created", `{"invoice_id":"inv_1"}`)
	broken := alice.waitDelivered(brokenSent.ID)
	if status, n, _, next := onlyDelivery(t, broken); status != "failed" || n != 4 || next != nil {
		t.Errorf("delivery = %s after %d attempts, next %v; want failed after 4, no next", status, n, next)
	}
	time.Sleep(100 * time.Millisecond)
	checkReceived(t, receivedRequests(t, st, aliceID, brokenEP), 4, brokenSent.ID)
	if a := alice.attempts("/v1/events/" + brokenSent.ID + "/attempts"); len(a) != 4 {
		t.Errorf("event attempts = %+v, want 4", a)
	}

	// 7. Disabling an endpoint while a retry waits ends the delivery as
	// failed with endpoint_disabled, without another request. A long wait
	// keeps the retry waiting while the test disables the endpoint.
	setRetrySchedule(t, base, "1h")
	disEP := createTestEndpoint(t, base, aliceID, "user.created", http.StatusInternalServerError)
	disSent := alice.sendEvent("user.created", `{"user_id":"usr_1"}`)
	waiting := alice.waitEvent(disSent.ID, "a retry waiting", func(e testEvent) bool {
		return len(e.Deliveries) == 1 && e.Deliveries[0].NextAttemptAt != nil
	})
	if status, n, _, next := onlyDelivery(t, waiting); status != "pending" || n != 1 || time.Until(*next) < 50*time.Minute {
		t.Errorf("waiting delivery = %s after %d attempts, next %v; want pending after 1, next in about an hour", status, n, next)
	}
	if status := alice.do("PATCH", "/v1/endpoints/"+disEP, map[string]any{"enabled": false}, nil); status != http.StatusOK {
		t.Fatalf("PATCH /v1/endpoints/%s: status %d", disEP, status)
	}
	disabled := alice.waitDelivered(disSent.ID)
	if status, n, errText, next := onlyDelivery(t, disabled); status != "failed" || n != 2 || errText != "endpoint_disabled" || next != nil {
		t.Errorf("delivery = %s after %d attempts, error %q, next %v; want failed after 2, endpoint_disabled, no next",
			status, n, errText, next)
	}
	checkReceived(t, receivedRequests(t, st, aliceID, disEP), 1, disSent.ID)
	setRetrySchedule(t, base, "5ms,10ms,20ms")

	// 8. Hookd stops while a retry waits and starts again after it is due:
	// the retry is sent soon after start. The tap tells the server to stop
	// before the first attempt is recorded, so the retry is scheduled by an
	// instance that is stopping and becomes due while none runs.
	restartEP := createTestEndpoint(t, base, aliceID, "order.shipped", http.StatusInternalServerError)
	if status := alice.do("GET", "/v1/endpoints/"+restartEP, nil, &ep); status != http.StatusOK {
		t.Fatalf("GET /v1/endpoints/%s: status %d", restartEP, status)
	}
	first, tapped := srv, make(chan struct{})
	stopTap := newTap(t, ep.URL, func(n int) {
		if n == 1 {
			first.cancel()
			close(tapped)
		}
	})
	if status := alice.do("PATCH", "/v1/endpoints/"+restartEP, map[string]any{"url": stopTap.URL}, nil); status != http.StatusOK {
		t.Fatalf("PATCH /v1/endpoints/%s: status %d", restartEP, status)
	}
	restartSent := alice.sendEvent("order.shipped", `{"order_id":"ord_123"}`)
	select {
	case <-tapped:
	case <-time.After(5 * time.Second):
		t.Fatal("the first attempt did not reach the receiver")
	}
	first.stop()
	stopped, err := st.GetEvent(context.Background(), aliceID, restartSent.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := stopped.Deliveries[0]
	if d.Status != store.DeliveryPending || d.Attempts != 1 || d.NextAttemptAt == nil {
		t.Fatalf("delivery while stopped = %+v, want pending after 1 attempt with a next attempt", d)
	}
	time.Sleep(time.Until(*d.NextAttemptAt) + 50*time.Millisecond)
	if err := st.SetTestReceiverStatus(context.Background(), aliceID, restartEP, http.StatusOK); err != nil {
		t.Fatal(err)
	}
	checkReceived(t, receivedRequests(t, st, aliceID, restartEP), 1, restartSent.ID)

	srv = startServeAt(t, addr)
	restarted := alice.waitDelivered(restartSent.ID)
	if status, n, _, _ := onlyDelivery(t, restarted); status != "succeeded" || n != 2 {
		t.Errorf("delivery after restart = %s after %d attempts, want succeeded after 2", status, n)
	}
	checkReceived(t, receivedRequests(t, st, aliceID, restartEP), 2, restartSent.ID)

	// 9. Retention deletes an event older than HOOKD_RETENTION with its
	// deliveries and attempts, and keeps newer ones. The old event is stored
	// with a clock 31 days back and delivered by the running server.
	oldStore, err := store.Open(context.Background(), dbCfg, store.WithClock(func() time.Time {
		return time.Now().Add(-31 * 24 * time.Hour)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { oldStore.Close() })
	okEP := createTestEndpoint(t, base, aliceID, "account.closed", http.StatusOK)
	old, _, err := oldStore.CreateEvent(context.Background(), aliceID, store.NewEvent{
		EventType: "account.closed", Payload: []byte(`{"account":"old"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	alice.waitDelivered(old.ID)
	oldAttempts := alice.attempts("/v1/events/" + old.ID + "/attempts")
	if len(oldAttempts) != 1 {
		t.Fatalf("old event attempts = %+v, want 1", oldAttempts)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if n := retention.New(st, log, func() time.Duration { return config.Default().Retention }).RunOnce(context.Background()); n != 1 {
		t.Errorf("retention deleted %d events, want 1", n)
	}
	for _, path := range []string{
		"/v1/events/" + old.ID,
		"/v1/events/" + old.ID + "/attempts",
		"/v1/attempts/" + oldAttempts[0].ID,
	} {
		if status := alice.do("GET", path, nil, nil); status != http.StatusNotFound {
			t.Errorf("after retention: GET %s: status %d, want 404", path, status)
		}
	}
	if a := alice.attempts("/v1/endpoints/" + okEP + "/attempts"); len(a) != 0 {
		t.Errorf("after retention: endpoint attempts = %+v, want none", a)
	}
	for _, id := range []string{sent.ID, brokenSent.ID, disSent.ID, restartSent.ID} {
		if status := alice.do("GET", "/v1/events/"+id, nil, nil); status != http.StatusOK {
			t.Errorf("after retention: GET /v1/events/%s: status %d, want 200", id, status)
		}
	}

	// 10. Another account gets 404 for the attempt routes of the first
	// account's events, endpoints and attempts.
	for _, path := range []string{
		"/v1/events/" + sent.ID + "/attempts",
		"/v1/endpoints/" + orderEP + "/attempts",
		"/v1/endpoints/" + orderEP + "/attempts?outcome=failed",
		"/v1/attempts/" + evAttempts[0].ID,
	} {
		if status := bob.do("GET", path, nil, nil); status != http.StatusNotFound {
			t.Errorf("second account: GET %s: status %d, want 404", path, status)
		}
	}
}
