package admin_test

import (
	"bytes"
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"hookd/internal/admin"
	"hookd/internal/api"
	"hookd/internal/delivery"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

// refreshTag starts the tag that makes an event page refresh itself.
const refreshTag = `<meta http-equiv="refresh"`

// events lists up to 1000 of the account's events in the store.
func (u *testUI) events(acctID string) []store.EventSummary {
	u.t.Helper()
	evs, err := u.st.ListEvents(context.Background(), acctID, store.EventFilter{Limit: 1000})
	if err != nil {
		u.t.Fatal(err)
	}
	return evs
}

// newEvent creates an event through the store.
func (u *testUI) newEvent(acctID, eventType string) store.Event {
	u.t.Helper()
	e, _, err := u.st.CreateEvent(context.Background(), acctID, store.NewEvent{EventType: eventType, Payload: []byte(`{"n":1}`)})
	if err != nil {
		u.t.Fatal(err)
	}
	return e
}

// sentEventPath checks that rec redirects to an event page after the
// send-event form, with the given ?sent= result, and returns the page's
// path without the query.
func sentEventPath(t *testing.T, rec *httptest.ResponseRecorder, acctID, sent string) string {
	t.Helper()
	loc := rec.Header().Get("Location")
	prefix := "/admin/accounts/" + acctID + "/events/msg_"
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, prefix) || !strings.HasSuffix(loc, "?sent="+sent) {
		t.Fatalf("status %d, Location %q, want 303 to %s…?sent=%s (body %q)", rec.Code, loc, prefix, sent, rec.Body.String())
	}
	return strings.TrimSuffix(loc, "?sent="+sent)
}

// newDeliveringUI returns the API server with the admin UI mounted and
// delivery workers running, woken by the UI. The workers poll rarely, so
// a delivery happens soon only if the UI wakes them.
func newDeliveringUI(t *testing.T) *testUI {
	t.Helper()
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
	t.Cleanup(func() {
		cancel()
		<-done
	})
	srv := api.New(st, log, api.WithAdmin(admin.New(st, log, admin.WithWake(pool.Wake()))))
	return &testUI{t: t, st: st, handler: srv.Handler(), logs: logs}
}

func TestSendEventDelivered(t *testing.T) {
	received := make(chan string, 1)
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		select {
		case received <- string(body):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer rcv.Close()

	u := newDeliveringUI(t)
	acct, _ := u.newAccount("acme")
	u.newEndpoint(acct.ID, store.NewEndpoint{URL: rcv.URL + "/hook", EventTypes: []string{"order.paid"}})
	base := "/admin/accounts/" + acct.ID + "/events"

	list := wantPage(t, u.get(base), http.StatusOK)
	if !strings.Contains(list, `href="`+base+`/new"`) {
		t.Fatalf("event list has no link to the send form: %q", list)
	}
	form := wantPage(t, u.get(base+"/new"), http.StatusOK)
	for _, want := range []string{`action="` + base + `/new"`, `name="event_type"`, `name="payload"`, `name="idempotency_key"`} {
		if !strings.Contains(form, want) {
			t.Fatalf("send form has no %s: %q", want, form)
		}
	}

	payload := `{"order_id": 1234, "items": [1, 2]}`
	path := sentEventPath(t, u.post(base+"/new", url.Values{
		"event_type": {" order.paid "},
		"payload":    {payload},
	}), acct.ID, "created")

	select {
	case body := <-received:
		if body != payload {
			t.Fatalf("receiver got %q, want the payload as entered %q", body, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the receiver got nothing: the workers were not woken")
	}

	page := wantPage(t, u.get(path+"?sent=created"), http.StatusOK)
	if !strings.Contains(page, "Event sent.") {
		t.Fatalf("event page does not say the event was sent: %q", page)
	}

	// The worker records the result just after the receiver answers.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(page, `<td class="status">succeeded</td>`) {
		if time.Now().After(deadline) {
			t.Fatalf("event page never showed succeeded: %q", page)
		}
		time.Sleep(20 * time.Millisecond)
		page = wantPage(t, u.get(path), http.StatusOK)
	}
	for _, want := range []string{
		"<code>order.paid</code>",
		html.EscapeString(rcv.URL + "/hook"),
		"<td>204</td>",
		html.EscapeString("\"order_id\": 1234,\n  \"items\": ["),
		`href="` + path + `">Refresh</a>`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("event page has no %q: %q", want, page)
		}
	}
	if strings.Contains(page, refreshTag) || strings.Contains(page, "Event sent.") {
		t.Fatalf("finished event page refreshes or repeats the sent notice: %q", page)
	}

	list = wantPage(t, u.get(base), http.StatusOK)
	if !strings.Contains(list, "1 succeeded, 0 failed, 0 pending") {
		t.Fatalf("event list has no delivery counts: %q", list)
	}
}

func TestSendEventInvalid(t *testing.T) {
	u := newTestUI(t, admin.WithMaxPayloadBytes(func() int64 { return 100 }))
	acct, _ := u.newAccount("acme")
	path := "/admin/accounts/" + acct.ID + "/events/new"

	cases := []struct {
		name, eventType, payload, key, msg string
	}{
		{"invalid JSON", "order.paid", `{"a": `, "", "Payload is not valid JSON."},
		{"not an object", "order.paid", `[1, 2]`, "", "Payload must be a JSON object."},
		{"empty payload", "order.paid", "  ", "", "Payload is required."},
		{"oversized", "order.paid", `{"a": "` + strings.Repeat("x", 100) + `"}`, "", "Payload is larger than 100 bytes."},
		{"bad event type", "order paid!", `{}`, "", "Event type must be 1 to 128 letters"},
		{"no event type", "", `{}`, "", "Event type must be 1 to 128 letters"},
		{"long key", "order.paid", `{}`, strings.Repeat("k", 256), "Idempotency key must be at most 255 bytes."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := wantPage(t, u.post(path, url.Values{
				"event_type":      {c.eventType},
				"payload":         {c.payload},
				"idempotency_key": {c.key},
			}), http.StatusUnprocessableEntity)
			if !strings.Contains(body, html.EscapeString(c.msg)) {
				t.Fatalf("page does not say %q: %q", c.msg, body)
			}
			// The input is kept (R5).
			if !strings.Contains(body, `value="`+html.EscapeString(c.eventType)+`"`) ||
				!strings.Contains(body, ">"+html.EscapeString(c.payload)+"</textarea>") {
				t.Fatalf("input not kept: %q", body)
			}
			if evs := u.events(acct.ID); len(evs) != 0 {
				t.Fatalf("events = %+v, want none", evs)
			}
		})
	}

	// A form far over the limit is refused before it is read in full.
	body := wantPage(t, u.post(path, url.Values{
		"event_type": {"order.paid"},
		"payload":    {`{"a": "` + strings.Repeat("x", 10000) + `"}`},
	}), http.StatusUnprocessableEntity)
	if !strings.Contains(body, "Payload is larger than 100 bytes.") {
		t.Fatalf("page does not say the payload is too large: %q", body)
	}
	if evs := u.events(acct.ID); len(evs) != 0 {
		t.Fatalf("events = %+v, want none", evs)
	}
}

func TestSendEventIdempotencyKey(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	path := "/admin/accounts/" + acct.ID + "/events/new"
	form := url.Values{"event_type": {"order.paid"}, "payload": {`{"n": 1}`}, "idempotency_key": {" order-1 "}}

	first := sentEventPath(t, u.post(path, form), acct.ID, "created")
	form.Set("payload", `{"n": 2}`)
	second := sentEventPath(t, u.post(path, form), acct.ID, "existing")
	if second != first {
		t.Fatalf("repeated key went to %s, want the existing event %s", second, first)
	}
	if evs := u.events(acct.ID); len(evs) != 1 || evs[0].IdempotencyKey != "order-1" {
		t.Fatalf("events = %+v, want one with key order-1", evs)
	}

	page := wantPage(t, u.get(second+"?sent=existing"), http.StatusOK)
	if !strings.Contains(page, "Existing event returned") || !strings.Contains(page, "<code>order-1</code>") {
		t.Fatalf("event page does not say the existing event was returned: %q", page)
	}
	if !strings.Contains(page, html.EscapeString(`"n": 1`)) {
		t.Fatalf("event page does not show the first payload: %q", page)
	}
}

func TestEventListFilterAndPaging(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	base := "/admin/accounts/" + acct.ID + "/events"

	var paid []store.Event
	for i := 0; i < 55; i++ {
		paid = append(paid, u.newEvent(acct.ID, "order.paid"))
		if i%10 == 0 {
			u.newEvent(acct.ID, "user.created")
		}
	}

	// All types: 61 events, the newest first.
	first := wantPage(t, u.get(base), http.StatusOK)
	if n := strings.Count(first, `<tr id="event-`); n != 50 {
		t.Fatalf("first page has %d events, want 50", n)
	}
	if !strings.Contains(first, "user.created") {
		t.Fatalf("unfiltered page has no user.created: %q", first)
	}
	if strings.Index(first, paid[54].ID) > strings.Index(first, paid[53].ID) {
		t.Fatalf("events are not newest first: %q", first)
	}

	// Filtered by type: 55 events in two pages, the cursor keeping the filter.
	page1 := wantPage(t, u.get(base+"?type=order.paid"), http.StatusOK)
	if strings.Contains(page1, "<code>user.created</code>") {
		t.Fatalf("filtered page has another type: %q", page1)
	}
	if n := strings.Count(page1, `<tr id="event-`); n != 50 {
		t.Fatalf("filtered first page has %d events, want 50", n)
	}
	next := base + "?type=order.paid&amp;before=" + paid[5].ID
	if !strings.Contains(page1, `href="`+next+`"`) {
		t.Fatalf("filtered first page has no link %s: %q", next, page1)
	}
	if !strings.Contains(page1, `value="order.paid"`) {
		t.Fatalf("filter not shown in the form: %q", page1)
	}

	page2 := wantPage(t, u.get(html.UnescapeString(next)), http.StatusOK)
	if n := strings.Count(page2, `<tr id="event-`); n != 5 {
		t.Fatalf("filtered second page has %d events, want 5", n)
	}
	for _, e := range paid[:5] {
		if !strings.Contains(page2, e.ID) {
			t.Fatalf("second page has no %s: %q", e.ID, page2)
		}
	}
	if strings.Contains(page2, "Next page") || !strings.Contains(page2, `href="`+base+`?type=order.paid">First page`) {
		t.Fatalf("second page links: %q", page2)
	}

	none := wantPage(t, u.get(base+"?type=refund.issued"), http.StatusOK)
	if !strings.Contains(none, "no events of this type") {
		t.Fatalf("empty filter page: %q", none)
	}
}

func TestEventPageRefreshesWhilePending(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/b"})
	e := u.newEvent(acct.ID, "order.paid")
	path := "/admin/accounts/" + acct.ID + "/events/" + e.ID
	ctx := context.Background()

	wantRefresh := func(want bool, state string) {
		t.Helper()
		page := wantPage(t, u.get(path), http.StatusOK)
		if got := strings.Contains(page, refreshTag+` content="2; url=`+path+`"`); got != want {
			t.Fatalf("%s: refresh tag present = %v, want %v: %q", state, got, want, page)
		}
		if !strings.Contains(page, `href="`+path+`">Refresh</a>`) {
			t.Fatalf("%s: no Refresh link: %q", state, page)
		}
	}
	wantRefresh(true, "pending")

	// Claim both deliveries: they are in progress, then finish one by one.
	var claimed []store.ClaimedDelivery
	for range 2 {
		d, ok, err := u.st.ClaimDelivery(ctx, time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim: ok %v, err %v", ok, err)
		}
		claimed = append(claimed, d)
	}
	wantRefresh(true, "in progress")
	if _, err := u.st.RecordAttempt(ctx, claimed[0].ID, store.AttemptResult{Number: 1, Outcome: store.AttemptSucceeded, ResponseStatus: 200}); err != nil {
		t.Fatal(err)
	}
	wantRefresh(true, "one in progress")
	if _, err := u.st.RecordAttempt(ctx, claimed[1].ID, store.AttemptResult{Number: 1, Outcome: store.AttemptFailed, ResponseStatus: 500, Error: "endpoint answered 500"}); err != nil {
		t.Fatal(err)
	}
	wantRefresh(false, "finished")

	page := wantPage(t, u.get(path), http.StatusOK)
	for _, want := range []string{"<td>500</td>", "endpoint answered 500", `<td class="status">failed</td>`, `<td class="status">succeeded</td>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("event page has no %q: %q", want, page)
		}
	}
}

func TestEventPageDeletedEndpoint(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	e := u.newEvent(acct.ID, "order.paid")
	if err := u.st.DeleteEndpoint(context.Background(), acct.ID, ep.ID); err != nil {
		t.Fatal(err)
	}
	page := wantPage(t, u.get("/admin/accounts/"+acct.ID+"/events/"+e.ID), http.StatusOK)
	if !strings.Contains(page, "<code>"+ep.ID+"</code> (deleted)") {
		t.Fatalf("event page does not show the deleted endpoint: %q", page)
	}
}

func TestOtherAccountsEventNotFound(t *testing.T) {
	u := newTestUI(t)
	acme, _ := u.newAccount("acme")
	other, _ := u.newAccount("other")
	e := u.newEvent(acme.ID, "order.paid")

	for _, path := range []string{
		"/admin/accounts/" + other.ID + "/events/" + e.ID,
		"/admin/accounts/" + acme.ID + "/events/msg_unknown",
		"/admin/accounts/acct_unknown/events",
		"/admin/accounts/acct_unknown/events/new",
	} {
		wantPage(t, u.get(path), http.StatusNotFound)
	}
	list := wantPage(t, u.get("/admin/accounts/"+other.ID+"/events"), http.StatusOK)
	if strings.Contains(list, e.ID) {
		t.Fatalf("other account's list shows %s: %q", e.ID, list)
	}
	wantPage(t, u.post("/admin/accounts/acct_unknown/events/new", url.Values{"event_type": {"a"}, "payload": {"{}"}}), http.StatusNotFound)
}

func TestSendEventCrossSiteRefused(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	req := httptest.NewRequest(http.MethodPost, "/admin/accounts/"+acct.ID+"/events/new",
		strings.NewReader(url.Values{"event_type": {"order.paid"}, "payload": {"{}"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	wantPage(t, u.serve(req), http.StatusForbidden)
	if evs := u.events(acct.ID); len(evs) != 0 {
		t.Fatalf("events = %+v, want none", evs)
	}
}

// recordAttempt claims the next due delivery and records a failed attempt
// whose retry is due at next, or a final one if next is nil.
func (u *testUI) recordAttempt(next *time.Time) store.Attempt {
	u.t.Helper()
	ctx := context.Background()
	d, ok, err := u.st.ClaimDelivery(ctx, time.Minute)
	if err != nil || !ok {
		u.t.Fatalf("claim: ok %v, err %v", ok, err)
	}
	a, err := u.st.RecordAttempt(ctx, d.ID, store.AttemptResult{
		Number: d.Attempts + 1, Outcome: store.AttemptFailed, ResponseStatus: 500,
		Error: "endpoint answered 500", Latency: 12 * time.Millisecond, NextAttemptAt: next,
	})
	if err != nil {
		u.t.Fatal(err)
	}
	return a
}

func TestEventPageAttemptsAndNextRetry(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	e := u.newEvent(acct.ID, "order.paid")
	path := "/admin/accounts/" + acct.ID + "/events/" + e.ID

	past := time.Now().Add(-time.Second)
	first := u.recordAttempt(&past)
	next := time.Now().Add(time.Hour)
	second := u.recordAttempt(&next)

	page := wantPage(t, u.get(path), http.StatusOK)
	for _, want := range []string{
		`<td class="status">pending, next retry at ` + next.UTC().Format("2006-01-02 15:04:05 UTC") + `</td>`,
		`<td class="attempts">2</td>`,
		"endpoint answered 500",
		"<td>12ms</td>",
		"<td>" + first.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC") + "</td>",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("event page has no %q: %q", want, page)
		}
	}
	if got, want := attemptRowIDs(page), []string{first.ID, second.ID}; !slices.Equal(got, want) {
		t.Fatalf("attempt rows = %v, want %v", got, want)
	}
	if strings.Contains(page, "More attempts") {
		t.Fatalf("event page with 2 attempts has a More link: %q", page)
	}
}

func TestEventPageRefreshInterval(t *testing.T) {
	cases := []struct {
		name string
		// next is when the retry is due from now, or 0 for a final attempt.
		next time.Duration
		want string
	}{
		{"retry due within 10 s", 5 * time.Second, `content="2; url=`},
		{"retry further off", 5 * time.Minute, `content="30; url=`},
		{"final", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newTestUI(t)
			acct, _ := u.newAccount("acme")
			u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
			e := u.newEvent(acct.ID, "order.paid")
			path := "/admin/accounts/" + acct.ID + "/events/" + e.ID
			var next *time.Time
			if c.next > 0 {
				at := time.Now().Add(c.next)
				next = &at
			}
			u.recordAttempt(next)
			page := wantPage(t, u.get(path), http.StatusOK)
			if c.want == "" {
				if strings.Contains(page, refreshTag) {
					t.Fatalf("final event page refreshes: %q", page)
				}
				return
			}
			if !strings.Contains(page, refreshTag+" "+c.want+path+`"`) {
				t.Fatalf("no %q refresh: %q", c.want, page)
			}
		})
	}
}

func TestEventPageMoreAttempts(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/a"})
	e := u.newEvent(acct.ID, "order.paid")
	path := "/admin/accounts/" + acct.ID + "/events/" + e.ID

	past := time.Now().Add(-time.Second)
	var ids []string
	for range 201 {
		ids = append(ids, u.recordAttempt(&past).ID)
	}

	page := wantPage(t, u.get(path), http.StatusOK)
	if got := attemptRowIDs(page); !slices.Equal(got, ids[:200]) {
		t.Fatalf("first page shows %d attempts, want the first 200", len(got))
	}
	more := `href="` + path + `?after=` + ids[199] + `#attempts">More attempts</a>`
	if !strings.Contains(page, more) {
		t.Fatalf("first page has no %q", more)
	}

	page = wantPage(t, u.get(path+"?after="+ids[199]), http.StatusOK)
	if got := attemptRowIDs(page); !slices.Equal(got, ids[200:]) {
		t.Fatalf("second page shows %v, want %v", got, ids[200:])
	}
	if strings.Contains(page, "More attempts") || !strings.Contains(page, `href="`+path+`#attempts">First attempts</a>`) {
		t.Fatalf("second page links: %q", page)
	}
	// The page refreshes to the same page of attempts.
	if !strings.Contains(page, `content="2; url=`+path+`?after=`+ids[199]+`"`) {
		t.Fatalf("second page does not refresh to itself: %q", page)
	}
}
