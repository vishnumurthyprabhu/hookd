package store_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"hookd/internal/ids"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

func mustTestEndpoint(t *testing.T, s store.Store, accountID string, in store.NewTestEndpoint) store.TestEndpoint {
	t.Helper()
	if in.BaseURL == "" {
		in.BaseURL = "http://127.0.0.1:8080"
	}
	te, err := s.CreateTestEndpoint(context.Background(), accountID, in)
	if err != nil {
		t.Fatal(err)
	}
	return te
}

func mustRecord(t *testing.T, s store.Store, receiverID string, in store.NewTestRequest) store.TestRequest {
	t.Helper()
	req, err := s.RecordTestRequest(context.Background(), receiverID, in)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func mustTestRequests(t *testing.T, s store.Store, accountID, endpointID string) []store.TestRequest {
	t.Helper()
	list, err := s.ListTestRequests(context.Background(), accountID, endpointID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func countTestRequests(t *testing.T, db *storetest.DB) int {
	t.Helper()
	var n int
	if err := db.SQL(t).QueryRow(`SELECT COUNT(*) FROM test_requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateTestEndpoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 987654321, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)

		te, err := s.CreateTestEndpoint(ctx, acct.ID, store.NewTestEndpoint{
			Description: "test orders",
			EventTypes:  []string{"order.created"},
			BaseURL:     "http://127.0.0.1:8080",
		})
		if err != nil {
			t.Fatal(err)
		}
		ts := c.t.Truncate(time.Microsecond)
		r := te.Receiver
		if !strings.HasPrefix(r.ID, ids.PrefixTestReceiver) {
			t.Errorf("receiver ID %q lacks prefix %q", r.ID, ids.PrefixTestReceiver)
		}
		wantR := store.TestReceiver{
			ID: r.ID, AccountID: acct.ID, EndpointID: te.Endpoint.ID,
			ResponseStatus: 200, CreatedAt: ts,
		}
		if r != wantR {
			t.Errorf("Receiver = %+v, want %+v", r, wantR)
		}
		wantE := store.Endpoint{
			ID:          te.Endpoint.ID,
			AccountID:   acct.ID,
			URL:         "http://127.0.0.1:8080/admin/test-receiver/" + r.ID,
			Description: "test orders",
			EventTypes:  []string{"order.created"},
			Enabled:     true,
			CreatedAt:   ts,
			UpdatedAt:   ts,
		}
		if !reflect.DeepEqual(te.Endpoint, wantE) {
			t.Fatalf("Endpoint = %+v, want %+v", te.Endpoint, wantE)
		}
		if !strings.HasPrefix(te.Secret, ids.SecretPrefix) {
			t.Errorf("secret %q lacks prefix %q", te.Secret, ids.SecretPrefix)
		}

		// It is an ordinary endpoint.
		got, err := s.GetEndpoint(ctx, acct.ID, te.Endpoint.ID)
		if err != nil || !reflect.DeepEqual(got, wantE) {
			t.Errorf("GetEndpoint = %+v, %v, want %+v", got, err, wantE)
		}
		if secret, err := s.GetEndpointSecret(ctx, acct.ID, te.Endpoint.ID); err != nil || secret != te.Secret {
			t.Errorf("GetEndpointSecret = %q, %v, want %q", secret, err, te.Secret)
		}
		plain := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
		list, err := s.ListEndpoints(ctx, acct.ID, 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 || list[0].ID != te.Endpoint.ID || list[1].ID != plain.ID {
			t.Fatalf("ListEndpoints = %+v, want the test endpoint and %s", list, plain.ID)
		}

		// The receiver is found by its ID alone.
		d, err := s.GetTestReceiver(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantD := store.TestReceiverDetail{Receiver: wantR, Endpoint: wantE, Secret: te.Secret}
		if !reflect.DeepEqual(d, wantD) {
			t.Fatalf("GetTestReceiver = %+v, want %+v", d, wantD)
		}

		// Only test endpoints are listed as receivers.
		rs, err := s.ListTestReceivers(ctx, acct.ID)
		if err != nil || !reflect.DeepEqual(rs, []store.TestReceiver{wantR}) {
			t.Fatalf("ListTestReceivers = %+v, %v, want [%+v]", rs, err, wantR)
		}
	})
}

func TestCreateTestEndpointOptions(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		s := db.New(t)
		acct := mustAccount(t, s)

		te := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{
			ResponseStatus: 503,
			BaseURL:        "https://hookd.example.com/",
		})
		if te.Receiver.ResponseStatus != 503 {
			t.Errorf("ResponseStatus = %d, want 503", te.Receiver.ResponseStatus)
		}
		if want := "https://hookd.example.com/admin/test-receiver/" + te.Receiver.ID; te.Endpoint.URL != want {
			t.Errorf("URL = %q, want %q", te.Endpoint.URL, want)
		}
		if te.Endpoint.EventTypes == nil || len(te.Endpoint.EventTypes) != 0 {
			t.Errorf("EventTypes = %#v, want empty", te.Endpoint.EventTypes)
		}
	})
}

func TestCreateTestEndpointUnknownAccount(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		s := db.New(t)
		_, err := s.CreateTestEndpoint(context.Background(), "acct_nope", store.NewTestEndpoint{BaseURL: "http://127.0.0.1:8080"})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		var n int
		if err := db.SQL(t).QueryRow(`SELECT (SELECT COUNT(*) FROM endpoints) + (SELECT COUNT(*) FROM test_receivers)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d rows were created", n)
		}
	})
}

func TestGetTestReceiverUnknown(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		s := db.New(t)
		if _, err := s.GetTestReceiver(context.Background(), "trcv_nope"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

func TestSetTestReceiverStatus(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		acct := mustAccount(t, s)
		te := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{})

		if err := s.SetTestReceiverStatus(ctx, acct.ID, te.Endpoint.ID, 500); err != nil {
			t.Fatal(err)
		}
		d, err := s.GetTestReceiver(ctx, te.Receiver.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d.Receiver.ResponseStatus != 500 {
			t.Fatalf("ResponseStatus = %d, want 500", d.Receiver.ResponseStatus)
		}

		// An ordinary endpoint has no receiver.
		plain := mustEndpoint(t, s, acct.ID, store.NewEndpoint{URL: "https://example.com/hook"})
		if err := s.SetTestReceiverStatus(ctx, acct.ID, plain.ID, 500); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ordinary endpoint: err = %v, want ErrNotFound", err)
		}
		if err := s.SetTestReceiverStatus(ctx, acct.ID, "ep_nope", 500); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("unknown endpoint: err = %v, want ErrNotFound", err)
		}
	})
}

func TestRecordAndListTestRequests(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		c := &clock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
		s := db.New(t, store.WithClock(c.now))
		acct := mustAccount(t, s)
		te := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{})
		ev, _ := mustEvent(t, s, acct.ID, store.NewEvent{EventType: "order.created", Payload: []byte(`{}`)})

		if got := mustTestRequests(t, s, acct.ID, te.Endpoint.ID); got == nil || len(got) != 0 {
			t.Fatalf("before any request: %#v, want empty", got)
		}

		first := mustRecord(t, s, te.Receiver.ID, store.NewTestRequest{
			WebhookID:      ev.ID,
			Headers:        map[string][]string{"Webhook-Id": {ev.ID}, "Content-Type": {"application/json"}},
			Body:           []byte(`{"a":1}`),
			Verified:       true,
			ResponseStatus: 200,
		})
		c.advance(time.Second)
		second := mustRecord(t, s, te.Receiver.ID, store.NewTestRequest{
			WebhookID:      "msg_unknown",
			Body:           []byte("bad \xff\x00 bytes"),
			VerifyError:    "no matching signature found",
			ResponseStatus: 500,
		})

		if !strings.HasPrefix(first.ID, ids.PrefixTestRequest) {
			t.Errorf("ID %q lacks prefix %q", first.ID, ids.PrefixTestRequest)
		}
		wantFirst := store.TestRequest{
			ID:             first.ID,
			ReceiverID:     te.Receiver.ID,
			ReceivedAt:     time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
			WebhookID:      ev.ID,
			EventType:      "order.created",
			Headers:        map[string][]string{"Webhook-Id": {ev.ID}, "Content-Type": {"application/json"}},
			Body:           []byte(`{"a":1}`),
			Verified:       true,
			ResponseStatus: 200,
		}
		wantSecond := store.TestRequest{
			ID:             second.ID,
			ReceiverID:     te.Receiver.ID,
			ReceivedAt:     time.Date(2026, 10, 29, 12, 0, 1, 0, time.UTC),
			WebhookID:      "msg_unknown",
			Headers:        map[string][]string{},
			Body:           []byte("bad �� bytes"),
			VerifyError:    "no matching signature found",
			ResponseStatus: 500,
		}
		if !reflect.DeepEqual(first, wantFirst) {
			t.Errorf("RecordTestRequest = %+v, want %+v", first, wantFirst)
		}
		if !reflect.DeepEqual(second, wantSecond) {
			t.Errorf("RecordTestRequest = %+v, want %+v", second, wantSecond)
		}

		// Newest first, as stored.
		got := mustTestRequests(t, s, acct.ID, te.Endpoint.ID)
		if !reflect.DeepEqual(got, []store.TestRequest{wantSecond, wantFirst}) {
			t.Fatalf("ListTestRequests = %+v, want second then first", got)
		}
		got, err := s.ListTestRequests(ctx, acct.ID, te.Endpoint.ID, 1)
		if err != nil || len(got) != 1 || got[0].ID != second.ID {
			t.Fatalf("ListTestRequests limit 1 = %+v, %v, want the second", got, err)
		}
		if _, err := s.ListTestRequests(ctx, acct.ID, te.Endpoint.ID, 0); err == nil {
			t.Error("limit 0: no error")
		}
	})
}

func TestRecordTestRequestUnknownReceiver(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		s := db.New(t)
		_, err := s.RecordTestRequest(context.Background(), "trcv_nope", store.NewTestRequest{ResponseStatus: 200})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if n := countTestRequests(t, db); n != 0 {
			t.Fatalf("%d requests stored", n)
		}
	})
}

func TestTestRequestsKeepNewest(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		s := db.New(t)
		acct := mustAccount(t, s)
		te := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{})
		other := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{})
		mustRecord(t, s, other.Receiver.ID, store.NewTestRequest{ResponseStatus: 200})

		const total = store.MaxTestRequests + 5
		var recorded []string
		for i := 0; i < total; i++ {
			recorded = append(recorded, mustRecord(t, s, te.Receiver.ID, store.NewTestRequest{ResponseStatus: 200}).ID)
		}

		got := mustTestRequests(t, s, acct.ID, te.Endpoint.ID)
		if len(got) != store.MaxTestRequests {
			t.Fatalf("kept %d requests, want %d", len(got), store.MaxTestRequests)
		}
		for i, req := range got {
			if want := recorded[total-1-i]; req.ID != want {
				t.Fatalf("request %d is %s, want %s", i, req.ID, want)
			}
		}
		// Another receiver's requests are not trimmed.
		if n := len(mustTestRequests(t, s, acct.ID, other.Endpoint.ID)); n != 1 {
			t.Fatalf("other receiver has %d requests, want 1", n)
		}
		if n := countTestRequests(t, db); n != store.MaxTestRequests+1 {
			t.Fatalf("%d rows in test_requests, want %d", n, store.MaxTestRequests+1)
		}
	})
}

func TestClearTestRequests(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		acct := mustAccount(t, s)
		te := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{})
		other := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{})
		for i := 0; i < 3; i++ {
			mustRecord(t, s, te.Receiver.ID, store.NewTestRequest{ResponseStatus: 200})
		}
		mustRecord(t, s, other.Receiver.ID, store.NewTestRequest{ResponseStatus: 200})

		if err := s.ClearTestRequests(ctx, acct.ID, te.Endpoint.ID); err != nil {
			t.Fatal(err)
		}
		if got := mustTestRequests(t, s, acct.ID, te.Endpoint.ID); len(got) != 0 {
			t.Fatalf("after clear: %d requests", len(got))
		}
		if got := mustTestRequests(t, s, acct.ID, other.Endpoint.ID); len(got) != 1 {
			t.Fatalf("other receiver: %d requests, want 1", len(got))
		}
		// The receiver still records afterwards.
		mustRecord(t, s, te.Receiver.ID, store.NewTestRequest{ResponseStatus: 200})
		if got := mustTestRequests(t, s, acct.ID, te.Endpoint.ID); len(got) != 1 {
			t.Fatalf("after recording again: %d requests, want 1", len(got))
		}
	})
}

func TestDeleteTestEndpoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		acct := mustAccount(t, s)
		te := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{})
		keep := mustTestEndpoint(t, s, acct.ID, store.NewTestEndpoint{})
		for i := 0; i < 3; i++ {
			mustRecord(t, s, te.Receiver.ID, store.NewTestRequest{ResponseStatus: 200})
		}
		mustRecord(t, s, keep.Receiver.ID, store.NewTestRequest{ResponseStatus: 200})

		if err := s.DeleteEndpoint(ctx, acct.ID, te.Endpoint.ID); err != nil {
			t.Fatal(err)
		}

		// Its requests are gone, the other receiver's are not.
		if n := countTestRequests(t, db); n != 1 {
			t.Fatalf("%d rows in test_requests, want 1", n)
		}
		if got := mustTestRequests(t, s, acct.ID, keep.Endpoint.ID); len(got) != 1 {
			t.Fatalf("other receiver: %d requests, want 1", len(got))
		}

		// The receiver is still found, marked deleted, so it can answer 404.
		d, err := s.GetTestReceiver(ctx, te.Receiver.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !d.EndpointDeleted || d.Endpoint.ID != te.Endpoint.ID {
			t.Fatalf("GetTestReceiver = %+v, want the deleted endpoint", d)
		}
		if d, _ := s.GetTestReceiver(ctx, keep.Receiver.ID); d.EndpointDeleted {
			t.Fatal("kept receiver is marked deleted")
		}

		// Nothing more is recorded or changed.
		if _, err := s.RecordTestRequest(ctx, te.Receiver.ID, store.NewTestRequest{ResponseStatus: 200}); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("RecordTestRequest: err = %v, want ErrNotFound", err)
		}
		if n := countTestRequests(t, db); n != 1 {
			t.Fatalf("%d rows in test_requests after recording, want 1", n)
		}
		if err := s.SetTestReceiverStatus(ctx, acct.ID, te.Endpoint.ID, 500); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("SetTestReceiverStatus: err = %v, want ErrNotFound", err)
		}
		if _, err := s.ListTestRequests(ctx, acct.ID, te.Endpoint.ID, 10); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ListTestRequests: err = %v, want ErrNotFound", err)
		}
		if err := s.ClearTestRequests(ctx, acct.ID, te.Endpoint.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ClearTestRequests: err = %v, want ErrNotFound", err)
		}
		rs, err := s.ListTestReceivers(ctx, acct.ID)
		if err != nil || len(rs) != 1 || rs[0].ID != keep.Receiver.ID {
			t.Errorf("ListTestReceivers = %+v, %v, want only %s", rs, err, keep.Receiver.ID)
		}
	})
}

func TestTestReceiversOfAnotherAccountAreNotFound(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *storetest.DB) {
		ctx := context.Background()
		s := db.New(t)
		owner := mustAccount(t, s)
		intruder := mustAccount(t, s)
		te := mustTestEndpoint(t, s, owner.ID, store.NewTestEndpoint{})
		mustRecord(t, s, te.Receiver.ID, store.NewTestRequest{ResponseStatus: 200})

		if rs, err := s.ListTestReceivers(ctx, intruder.ID); err != nil || len(rs) != 0 {
			t.Errorf("ListTestReceivers = %+v, %v, want none", rs, err)
		}
		if err := s.SetTestReceiverStatus(ctx, intruder.ID, te.Endpoint.ID, 500); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("SetTestReceiverStatus: err = %v, want ErrNotFound", err)
		}
		if _, err := s.ListTestRequests(ctx, intruder.ID, te.Endpoint.ID, 10); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ListTestRequests: err = %v, want ErrNotFound", err)
		}
		if err := s.ClearTestRequests(ctx, intruder.ID, te.Endpoint.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ClearTestRequests: err = %v, want ErrNotFound", err)
		}
		if err := s.DeleteEndpoint(ctx, intruder.ID, te.Endpoint.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("DeleteEndpoint: err = %v, want ErrNotFound", err)
		}

		// Nothing was changed.
		d, err := s.GetTestReceiver(ctx, te.Receiver.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d.Receiver.ResponseStatus != 200 || d.EndpointDeleted {
			t.Errorf("receiver changed: %+v", d)
		}
		if got := mustTestRequests(t, s, owner.ID, te.Endpoint.ID); len(got) != 1 {
			t.Errorf("owner has %d requests, want 1", len(got))
		}

		// An event of another account does not name a request's type.
		ev, _ := mustEvent(t, s, intruder.ID, store.NewEvent{EventType: "order.created", Payload: []byte(`{}`)})
		req := mustRecord(t, s, te.Receiver.ID, store.NewTestRequest{WebhookID: ev.ID, ResponseStatus: 200})
		if req.EventType != "" {
			t.Errorf("EventType = %q from another account's event, want empty", req.EventType)
		}
	})
}
