package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

	"hookd/internal/admin"
	"hookd/internal/api"
	"hookd/internal/delivery"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

// newTestEndpoint creates a test endpoint through the store.
func (u *testUI) newTestEndpoint(acctID string, in store.NewTestEndpoint) store.TestEndpoint {
	u.t.Helper()
	if in.BaseURL == "" {
		in.BaseURL = "http://127.0.0.1:8080"
	}
	te, err := u.st.CreateTestEndpoint(context.Background(), acctID, in)
	if err != nil {
		u.t.Fatal(err)
	}
	return te
}

// testRequests lists the requests a test endpoint received, in the store.
func (u *testUI) testRequests(acctID, epID string) []store.TestRequest {
	u.t.Helper()
	reqs, err := u.st.ListTestRequests(context.Background(), acctID, epID, 1000)
	if err != nil {
		u.t.Fatal(err)
	}
	return reqs
}

// deliver sends body to a test receiver as the delivery workers do: a POST
// signed with secret, without the browser's cross-site headers. extra
// headers are added.
func (u *testUI) deliver(receiverID, secret, msgID string, body []byte, extra http.Header) *httptest.ResponseRecorder {
	u.t.Helper()
	wh, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		u.t.Fatal(err)
	}
	now := time.Now()
	sig, err := wh.Sign(msgID, now, body)
	if err != nil {
		u.t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, store.TestReceiverPath(receiverID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", msgID)
	req.Header.Set("webhook-timestamp", fmt.Sprint(now.Unix()))
	req.Header.Set("webhook-signature", sig)
	for name, vs := range extra {
		req.Header[name] = vs
	}
	return u.serve(req)
}

// wantAnswer checks the receiver's status and JSON answer.
func wantAnswer(t *testing.T, rec *httptest.ResponseRecorder, status int, verified bool) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	var v struct {
		Verified bool `json:"verified"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Verified != verified {
		t.Fatalf("answer %q, want verified %v", rec.Body.String(), verified)
	}
}

func TestCreateTestEndpoint(t *testing.T) {
	u := newTestUI(t, admin.WithReceiverBaseURL("http://hookd.internal:9000/"))
	acct, _ := u.newAccount("acme")
	base := "/admin/accounts/" + acct.ID

	list := wantPage(t, u.get(base+"/endpoints"), http.StatusOK)
	if !strings.Contains(list, `href="`+base+`/test-endpoints/new"`) {
		t.Fatalf("endpoint list has no link to the test endpoint form: %q", list)
	}
	form := wantPage(t, u.get(base+"/test-endpoints/new"), http.StatusOK)
	for _, want := range []string{`action="` + base + `/test-endpoints/new"`, `name="description"`, `name="event_types"`, `name="status"`, `value="200"`} {
		if !strings.Contains(form, want) {
			t.Fatalf("form has no %s: %q", want, form)
		}
	}

	rec := u.post(base+"/test-endpoints/new", url.Values{
		"description": {" checks "},
		"event_types": {"order.paid, user.created"},
		"status":      {"503"},
	})
	eps := u.endpoints(acct.ID)
	if len(eps) != 1 {
		t.Fatalf("%d endpoints, want 1", len(eps))
	}
	ep := eps[0]
	wantRedirect(t, rec, base+"/endpoints/"+ep.ID)
	rcvs, err := u.st.ListTestReceivers(context.Background(), acct.ID)
	if err != nil || len(rcvs) != 1 {
		t.Fatalf("receivers %v, %v", rcvs, err)
	}
	if want := "http://hookd.internal:9000" + store.TestReceiverPath(rcvs[0].ID); ep.URL != want {
		t.Fatalf("URL = %q, want %q", ep.URL, want)
	}
	if ep.Description != "checks" || strings.Join(ep.EventTypes, ",") != "order.paid,user.created" || !ep.Enabled {
		t.Fatalf("endpoint %+v", ep)
	}
	if rcvs[0].ResponseStatus != 503 {
		t.Fatalf("status = %d, want 503", rcvs[0].ResponseStatus)
	}

	page := wantPage(t, u.get(base+"/endpoints/"+ep.ID), http.StatusOK)
	for _, want := range []string{"test endpoint", `action="` + base + `/endpoints/` + ep.ID + `/test-status"`, `value="503"`, "No requests received yet."} {
		if !strings.Contains(page, want) {
			t.Fatalf("endpoint page has no %q: %q", want, page)
		}
	}
}

func TestCreateTestEndpointInvalid(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	path := "/admin/accounts/" + acct.ID + "/test-endpoints/new"
	for _, status := range []string{"99", "600", "abc", ""} {
		page := wantPage(t, u.post(path, url.Values{
			"description": {"kept input"},
			"event_types": {"bad type!"},
			"status":      {status},
		}), http.StatusUnprocessableEntity)
		for _, want := range []string{"Answer status must be a whole number from 100 to 599.", "Event type ", "kept input", "bad type!"} {
			if !strings.Contains(html.UnescapeString(page), want) {
				t.Fatalf("status %q: page has no %q: %q", status, want, page)
			}
		}
	}
	if eps := u.endpoints(acct.ID); len(eps) != 0 {
		t.Fatalf("%d endpoints created", len(eps))
	}
	wantPage(t, u.get("/admin/accounts/acct_unknown/test-endpoints/new"), http.StatusNotFound)
}

func TestTestReceiverVerified(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	te := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})
	ev := u.newEvent(acct.ID, "order.paid")

	body := []byte(`{"secret_body_marker":true}`)
	wantAnswer(t, u.deliver(te.Receiver.ID, te.Secret, ev.ID, body,
		http.Header{"X-Marker": {"secret_header_marker"}}), http.StatusOK, true)

	reqs := u.testRequests(acct.ID, te.Endpoint.ID)
	if len(reqs) != 1 {
		t.Fatalf("%d requests stored, want 1", len(reqs))
	}
	got := reqs[0]
	if !got.Verified || got.VerifyError != "" || got.WebhookID != ev.ID || got.EventType != "order.paid" ||
		got.ResponseStatus != http.StatusOK || string(got.Body) != string(body) || got.Headers["X-Marker"][0] != "secret_header_marker" {
		t.Fatalf("stored %+v", got)
	}

	page := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints/"+te.Endpoint.ID), http.StatusOK)
	for _, want := range []string{"signature verified", ev.ID, "order.paid", "X-Marker: secret_header_marker", "secret_body_marker", "<details>"} {
		if !strings.Contains(page, want) {
			t.Fatalf("endpoint page has no %q: %q", want, page)
		}
	}
	for _, marker := range []string{"secret_body_marker", "secret_header_marker", te.Secret} {
		if strings.Contains(u.logs.String(), marker) {
			t.Fatalf("%q was logged: %s", marker, u.logs.String())
		}
	}
}

func TestTestReceiverWrongSecret(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	te := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})
	_, other := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})

	wantAnswer(t, u.deliver(te.Receiver.ID, other, "msg_unknown", []byte(`{}`), nil), http.StatusOK, false)

	reqs := u.testRequests(acct.ID, te.Endpoint.ID)
	if len(reqs) != 1 {
		t.Fatalf("%d requests stored, want 1", len(reqs))
	}
	got := reqs[0]
	if got.Verified || !strings.Contains(got.VerifyError, standardwebhooks.ErrNoMatchingSignature.Error()) || got.EventType != "" {
		t.Fatalf("stored %+v", got)
	}
	page := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints/"+te.Endpoint.ID), http.StatusOK)
	if !strings.Contains(page, "not verified: "+got.VerifyError) {
		t.Fatalf("endpoint page does not give the reason: %q", page)
	}

	// No signature headers at all.
	req := httptest.NewRequest(http.MethodPost, store.TestReceiverPath(te.Receiver.ID), strings.NewReader("plain"))
	wantAnswer(t, u.serve(req), http.StatusOK, false)
	if reqs := u.testRequests(acct.ID, te.Endpoint.ID); len(reqs) != 2 || reqs[0].VerifyError == "" || string(reqs[0].Body) != "plain" {
		t.Fatalf("stored %+v", reqs)
	}
}

func TestTestReceiverAnswerStatus(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	te := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})
	path := "/admin/accounts/" + acct.ID + "/endpoints/" + te.Endpoint.ID

	wantRedirect(t, u.post(path+"/test-status", url.Values{"status": {"500"}}), path)
	wantAnswer(t, u.deliver(te.Receiver.ID, te.Secret, "msg_1", []byte(`{}`), nil), http.StatusInternalServerError, true)
	if reqs := u.testRequests(acct.ID, te.Endpoint.ID); reqs[0].ResponseStatus != 500 {
		t.Fatalf("stored status %d, want 500", reqs[0].ResponseStatus)
	}
	if page := wantPage(t, u.get(path), http.StatusOK); !strings.Contains(page, `name="status" type="number" min="100" max="599" value="500"`) {
		t.Fatalf("page does not show status 500: %q", page)
	}

	// A status without a body.
	wantRedirect(t, u.post(path+"/test-status", url.Values{"status": {"204"}}), path)
	if rec := u.deliver(te.Receiver.ID, te.Secret, "msg_2", []byte(`{}`), nil); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("status %d, body %q", rec.Code, rec.Body.String())
	}

	page := wantPage(t, u.post(path+"/test-status", url.Values{"status": {"700"}}), http.StatusUnprocessableEntity)
	if !strings.Contains(page, "Answer status must be a whole number from 100 to 599.") || !strings.Contains(page, `value="700"`) {
		t.Fatalf("page has no error or lost the input: %q", page)
	}
	rcvs, _ := u.st.ListTestReceivers(context.Background(), acct.ID)
	if rcvs[0].ResponseStatus != 204 {
		t.Fatalf("status = %d after an invalid form, want 204", rcvs[0].ResponseStatus)
	}

	// An ordinary endpoint has no receiver.
	plain, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
	plainPath := "/admin/accounts/" + acct.ID + "/endpoints/" + plain.ID
	wantPage(t, u.post(plainPath+"/test-status", url.Values{"status": {"500"}}), http.StatusNotFound)
	wantPage(t, u.post(plainPath+"/test-requests/clear", nil), http.StatusNotFound)
	if page := wantPage(t, u.get(plainPath), http.StatusOK); strings.Contains(page, "test-status") || strings.Contains(page, "Received requests") {
		t.Fatalf("ordinary endpoint page shows test endpoint forms: %q", page)
	}
}

func TestTestReceiverKeepsLast100(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	te := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})
	for i := range store.MaxTestRequests + 5 {
		wantAnswer(t, u.deliver(te.Receiver.ID, te.Secret, fmt.Sprintf("msg_%03d", i), []byte(`{}`), nil), http.StatusOK, true)
	}
	reqs := u.testRequests(acct.ID, te.Endpoint.ID)
	if len(reqs) != store.MaxTestRequests {
		t.Fatalf("%d requests kept, want %d", len(reqs), store.MaxTestRequests)
	}
	if reqs[0].WebhookID != "msg_104" || reqs[len(reqs)-1].WebhookID != "msg_005" {
		t.Fatalf("kept %s … %s, want msg_104 … msg_005", reqs[0].WebhookID, reqs[len(reqs)-1].WebhookID)
	}
	page := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/endpoints/"+te.Endpoint.ID), http.StatusOK)
	if n := strings.Count(page, `<article class="request"`); n != store.MaxTestRequests {
		t.Fatalf("page shows %d requests, want %d", n, store.MaxTestRequests)
	}
	if strings.Index(page, "msg_104") > strings.Index(page, "msg_103") {
		t.Fatal("requests are not newest first")
	}
}

func TestClearTestRequests(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	te := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})
	path := "/admin/accounts/" + acct.ID + "/endpoints/" + te.Endpoint.ID
	u.deliver(te.Receiver.ID, te.Secret, "msg_1", []byte(`{}`), nil)

	page := wantPage(t, u.get(path), http.StatusOK)
	if !strings.Contains(page, `action="`+path+`/test-requests/clear"`) {
		t.Fatalf("page has no clear button: %q", page)
	}
	wantRedirect(t, u.post(path+"/test-requests/clear", nil), path)
	if reqs := u.testRequests(acct.ID, te.Endpoint.ID); len(reqs) != 0 {
		t.Fatalf("%d requests left", len(reqs))
	}

	req := httptest.NewRequest(http.MethodPost, path+"/test-requests/clear", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := u.serve(req); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site clear: status %d, want 403", rec.Code)
	}
}

func TestTestReceiverNotFound(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	te := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})

	if rec := u.deliver("trcv_unknown", te.Secret, "msg_1", []byte(`{}`), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown receiver: status %d, want 404", rec.Code)
	}
	if err := u.st.DeleteEndpoint(context.Background(), acct.ID, te.Endpoint.ID); err != nil {
		t.Fatal(err)
	}
	if rec := u.deliver(te.Receiver.ID, te.Secret, "msg_1", []byte(`{}`), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted endpoint: status %d, want 404", rec.Code)
	}
	// Nothing was stored for the deleted endpoint's receiver.
	other := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})
	if reqs := u.testRequests(acct.ID, other.Endpoint.ID); len(reqs) != 0 {
		t.Fatalf("%d requests stored", len(reqs))
	}
}

func TestTestReceiverBodyTooLarge(t *testing.T) {
	u := newTestUI(t, admin.WithMaxPayloadBytes(func() int64 { return 10 }))
	acct, _ := u.newAccount("acme")
	te := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})
	body := bytes.Repeat([]byte("a"), 10+64<<10+1)
	if rec := u.deliver(te.Receiver.ID, te.Secret, "msg_1", body, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
	if reqs := u.testRequests(acct.ID, te.Endpoint.ID); len(reqs) != 0 {
		t.Fatalf("%d requests stored", len(reqs))
	}
	wantAnswer(t, u.deliver(te.Receiver.ID, te.Secret, "msg_2", body[:10+64<<10], nil), http.StatusOK, true)
}

func TestOtherAccountsTestEndpointNotFound(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	other, _ := u.newAccount("other")
	te := u.newTestEndpoint(acct.ID, store.NewTestEndpoint{})
	path := "/admin/accounts/" + other.ID + "/endpoints/" + te.Endpoint.ID
	wantPage(t, u.post(path+"/test-status", url.Values{"status": {"500"}}), http.StatusNotFound)
	wantPage(t, u.post(path+"/test-requests/clear", nil), http.StatusNotFound)
	rcvs, _ := u.st.ListTestReceivers(context.Background(), acct.ID)
	if rcvs[0].ResponseStatus != 200 {
		t.Fatalf("status changed to %d through another account", rcvs[0].ResponseStatus)
	}
}

// TestSendEventToTestEndpoint sends an event from the UI to a test endpoint
// created in the UI, with real workers delivering over HTTP to the built-in
// receiver.
func TestSendEventToTestEndpoint(t *testing.T) {
	st := storetest.New(t)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pool := delivery.NewPool(st, log, func() delivery.Config {
		return delivery.Config{Workers: 2, PollInterval: time.Hour, Timeout: 5 * time.Second, Lease: time.Minute}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		pool.Run(ctx)
	}()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			<-done
		})
	}
	t.Cleanup(stop)
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	defer srv.Close()
	handler = api.New(st, log, api.WithAdmin(admin.New(st, log,
		admin.WithWake(pool.Wake()),
		admin.WithReceiverBaseURL(srv.URL),
	))).Handler()
	u := &testUI{t: t, st: st, handler: handler, logs: logs}

	acct, _ := u.newAccount("acme")
	base := "/admin/accounts/" + acct.ID
	u.post(base+"/test-endpoints/new", url.Values{"event_types": {"order.paid"}, "status": {"200"}})
	eps := u.endpoints(acct.ID)
	if len(eps) != 1 || !strings.HasPrefix(eps[0].URL, srv.URL+"/admin/test-receiver/trcv_") {
		t.Fatalf("endpoints %+v", eps)
	}
	ep := eps[0]

	payload := `{"order_id":"secret_payload_marker"}`
	path := sentEventPath(t, u.post(base+"/events/new", url.Values{
		"event_type": {"order.paid"},
		"payload":    {payload},
	}), acct.ID, "created")
	eventID := path[strings.LastIndex(path, "/")+1:]

	deadline := time.Now().Add(10 * time.Second)
	for {
		e, err := st.GetEvent(context.Background(), acct.ID, eventID)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.Deliveries) == 1 && e.Deliveries[0].Status == store.DeliverySucceeded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivery not succeeded: %+v", e.Deliveries)
		}
		time.Sleep(20 * time.Millisecond)
	}

	page := wantPage(t, u.get(base+"/endpoints/"+ep.ID), http.StatusOK)
	for _, want := range []string{"signature verified", eventID, "<code>order.paid</code>", "secret_payload_marker"} {
		if !strings.Contains(page, want) {
			t.Fatalf("endpoint page has no %q: %q", want, page)
		}
	}
	reqs := u.testRequests(acct.ID, ep.ID)
	if len(reqs) != 1 || !reqs[0].Verified || reqs[0].WebhookID != eventID || string(reqs[0].Body) != payload {
		t.Fatalf("stored %+v", reqs)
	}
	// Stop the server and the workers so the logs are complete.
	srv.Close()
	stop()
	logged := logs.String()
	for _, marker := range []string{"secret_payload_marker", reqs[0].Headers["Webhook-Signature"][0]} {
		if strings.Contains(logged, marker) {
			t.Fatalf("%q was logged: %s", marker, logged)
		}
	}
}
