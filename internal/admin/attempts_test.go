package admin_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"hookd/internal/store"
)

// attempt claims the next due delivery and records one attempt with the
// given outcome. A failure with retry set goes back to pending, due at
// once, to wait for another attempt.
func (u *testUI) attempt(outcome string, retry bool) store.Attempt {
	u.t.Helper()
	ctx := context.Background()
	d, ok, err := u.st.ClaimDelivery(ctx, time.Minute)
	if err != nil || !ok {
		u.t.Fatalf("claim: ok %v, err %v", ok, err)
	}
	r := store.AttemptResult{Number: d.Attempts + 1, Outcome: outcome, Latency: 12 * time.Millisecond}
	if outcome == store.AttemptFailed {
		r.ResponseStatus, r.Error = 500, "endpoint answered 500"
	} else {
		r.ResponseStatus = 200
	}
	if retry {
		next := time.Now().Add(-time.Second)
		r.NextAttemptAt = &next
	}
	a, err := u.st.RecordAttempt(ctx, d.ID, r)
	if err != nil {
		u.t.Fatal(err)
	}
	return a
}

// attemptRowIDs returns the attempt IDs of the rows on an endpoint page, in
// page order.
func attemptRowIDs(body string) []string {
	var ids []string
	for _, m := range regexp.MustCompile(`<tr id="attempt-([^"]+)">`).FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

func TestEndpointPageAttempts(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
	path := endpointPathFor(acct.ID, ep.ID)

	body := wantPage(t, u.get(path), http.StatusOK)
	if !strings.Contains(body, "Recent attempts") || !strings.Contains(body, "No attempts yet.") {
		t.Fatalf("page has no empty attempts section: %q", body)
	}

	ev := u.newEvent(acct.ID, "order.paid")
	failed := u.attempt(store.AttemptFailed, true)
	succeeded := u.attempt(store.AttemptSucceeded, false)

	body = wantPage(t, u.get(path), http.StatusOK)
	if got := attemptRowIDs(body); len(got) != 2 || got[0] != succeeded.ID || got[1] != failed.ID {
		t.Fatalf("attempt rows = %v, want newest first [%s %s]", got, succeeded.ID, failed.ID)
	}
	row := regexp.MustCompile(`(?s)<tr id="attempt-` + failed.ID + `">.*?</tr>`).FindString(body)
	for _, want := range []string{
		`href="/admin/accounts/` + acct.ID + `/events/` + ev.ID + `"`,
		"<td>1</td>", "failed", "<td>500</td>", "12ms", "endpoint answered 500",
	} {
		if !strings.Contains(row, want) {
			t.Fatalf("failed attempt row lacks %q: %q", want, row)
		}
	}

	body = wantPage(t, u.get(path+"?outcome=failed"), http.StatusOK)
	if got := attemptRowIDs(body); len(got) != 1 || got[0] != failed.ID {
		t.Fatalf("failed filter rows = %v, want [%s]", got, failed.ID)
	}
	if !strings.Contains(body, `<option value="failed" selected>`) {
		t.Fatal("the failed filter is not shown as selected")
	}
	body = wantPage(t, u.get(path+"?outcome=succeeded"), http.StatusOK)
	if got := attemptRowIDs(body); len(got) != 1 || got[0] != succeeded.ID {
		t.Fatalf("succeeded filter rows = %v, want [%s]", got, succeeded.ID)
	}
	// An unknown outcome shows every attempt.
	body = wantPage(t, u.get(path+"?outcome=maybe"), http.StatusOK)
	if got := attemptRowIDs(body); len(got) != 2 {
		t.Fatalf("unknown filter rows = %v, want both", got)
	}

	// Another account's endpoint has no page, so none of its attempts show.
	other, _ := u.newAccount("other")
	wantPage(t, u.get(endpointPathFor(other.ID, ep.ID)), http.StatusNotFound)
}

func TestEndpointPageAttemptsPaging(t *testing.T) {
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
	path := endpointPathFor(acct.ID, ep.ID)
	var ids []string // oldest first
	for i := range 52 {
		u.newEvent(acct.ID, "order.paid")
		outcome := store.AttemptFailed
		if i%2 == 0 {
			outcome = store.AttemptSucceeded
		}
		ids = append(ids, u.attempt(outcome, false).ID)
	}

	first := wantPage(t, u.get(path), http.StatusOK)
	got := attemptRowIDs(first)
	if len(got) != 50 || got[0] != ids[51] || got[49] != ids[2] {
		t.Fatalf("first page holds %d rows from %s to %s, want the newest 50", len(got), got[0], got[len(got)-1])
	}
	next := path + "?attempts_before=" + ids[2] + "#attempts"
	if !strings.Contains(first, `href="`+next+`"`) {
		t.Fatalf("no next page link to %s", next)
	}
	second := wantPage(t, u.get(path+"?attempts_before="+ids[2]), http.StatusOK)
	if got := attemptRowIDs(second); len(got) != 2 || got[0] != ids[1] || got[1] != ids[0] {
		t.Fatalf("second page rows = %v, want [%s %s]", got, ids[1], ids[0])
	}
	if strings.Contains(second, "Next page") || !strings.Contains(second, `href="`+path+`#attempts">First page`) {
		t.Fatal("last page has a next page link or no first page link")
	}

	// The filter is kept across pages: 26 failed attempts, the oldest is
	// ids[1].
	first = wantPage(t, u.get(path+"?outcome=failed"), http.StatusOK)
	if got := attemptRowIDs(first); len(got) != 26 || strings.Contains(first, "Next page") {
		t.Fatalf("failed filter: %d rows, want all 26 on one page", len(got))
	}
	var failed []string // newest first
	for i := 51; i >= 0; i -= 2 {
		failed = append(failed, ids[i])
	}
	before := failed[9]
	page := wantPage(t, u.get(path+"?outcome=failed&attempts_before="+before), http.StatusOK)
	if got := attemptRowIDs(page); len(got) != 16 || got[0] != failed[10] {
		t.Fatalf("failed page after %s: %v, want from %s", before, got, failed[10])
	}
	if !strings.Contains(page, `href="`+path+`?outcome=failed#attempts">First page`) {
		t.Fatal("first page link does not keep the filter")
	}
}

func TestEndpointPageAttemptsDisabledEndpoint(t *testing.T) {
	// The attempts of a disabled endpoint still show on its page.
	u := newTestUI(t)
	acct, _ := u.newAccount("acme")
	ep, _ := u.newEndpoint(acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
	u.newEvent(acct.ID, "order.paid")
	a := u.attempt(store.AttemptSucceeded, false)
	off := false
	if _, err := u.st.UpdateEndpoint(context.Background(), acct.ID, ep.ID, store.EndpointUpdate{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	body := wantPage(t, u.get(endpointPathFor(acct.ID, ep.ID)), http.StatusOK)
	if got := attemptRowIDs(body); len(got) != 1 || got[0] != a.ID {
		t.Fatalf("attempt rows = %v, want [%s]", got, a.ID)
	}
}

func endpointPathFor(acctID, epID string) string {
	return "/admin/accounts/" + acctID + "/endpoints/" + epID
}
