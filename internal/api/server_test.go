package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"hookd/internal/ids"
	"hookd/internal/store"
	"hookd/internal/store/storetest"
)

type testServer struct {
	srv  *Server
	st   store.Store
	logs *bytes.Buffer
}

// newTestServer returns a Server on a fresh store that logs at debug level
// to a buffer. It adds GET /v1/whoami, which answers with the account ID.
func newTestServer(t *testing.T) *testServer {
	t.Helper()
	st := storetest.New(t)
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := New(st, log)
	srv.mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		acct, ok := accountFrom(r.Context())
		if !ok {
			t.Error("no account in the context")
		}
		writeJSON(w, http.StatusOK, map[string]string{"account_id": acct.ID})
	})
	return &testServer{srv: srv, st: st, logs: logs}
}

// newKey creates an account and returns its ID and API key.
func (ts *testServer) newKey(t *testing.T) (string, string) {
	t.Helper()
	ctx := context.Background()
	acct, err := ts.st.CreateAccount(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ts.st.CreateAPIKey(ctx, acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	return acct.ID, key
}

func (ts *testServer) do(t *testing.T, method, path, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeError checks that rec holds a JSON error with the given status and
// code, and returns its message.
func decodeError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) string {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if body.Error.Code != code {
		t.Errorf("error code = %q, want %q", body.Error.Code, code)
	}
	if body.Error.Message == "" {
		t.Error("error message is empty")
	}
	return body.Error.Message
}

func TestAuthRejects(t *testing.T) {
	ts := newTestServer(t)
	_, key := ts.newKey(t)
	unknown, err := ids.NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, auth, msg string
	}{
		{"missing", "", "missing API key"},
		{"basic scheme", "Basic dXNlcjpwYXNz", "malformed"},
		{"no token", "Bearer", "malformed"},
		{"empty token", "Bearer   ", "malformed"},
		{"token with space", "Bearer " + key + " extra", "malformed"},
		{"bare key", key, "malformed"},
		{"unknown key", "Bearer " + unknown, "invalid API key"},
		{"key with a changed letter", "Bearer " + key[:len(key)-1] + "!", "invalid API key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := ts.do(t, "GET", "/v1/whoami", tt.auth)
			msg := decodeError(t, rec, http.StatusUnauthorized, CodeUnauthorized)
			if !strings.Contains(msg, tt.msg) {
				t.Errorf("message = %q, want it to contain %q", msg, tt.msg)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("no WWW-Authenticate header")
			}
		})
	}

	// Unknown /v1 paths need a key too, so they reveal nothing.
	decodeError(t, ts.do(t, "GET", "/v1/nope", ""), http.StatusUnauthorized, CodeUnauthorized)
}

func TestAuthAccepts(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	otherID, otherKey := ts.newKey(t)

	for _, tc := range []struct{ auth, want string }{
		{"Bearer " + key, acctID},
		{"bearer " + key, acctID},
		{"Bearer " + otherKey, otherID},
	} {
		rec := ts.do(t, "GET", "/v1/whoami", tc.auth)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
		var body map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body["account_id"] != tc.want {
			t.Errorf("account_id = %q, want %q", body["account_id"], tc.want)
		}
	}

	// With a valid key, an unknown route is a JSON 404 and a wrong method a
	// JSON 405.
	decodeError(t, ts.do(t, "GET", "/v1/nope", "Bearer "+key), http.StatusNotFound, CodeNotFound)
	rec := ts.do(t, "POST", "/v1/whoami", "Bearer "+key)
	decodeError(t, rec, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q, want it to list GET", allow)
	}
}

func TestRequestLogHasNoKeyMaterial(t *testing.T) {
	ts := newTestServer(t)
	acctID, key := ts.newKey(t)
	unknown, _ := ids.NewAPIKey()

	ts.do(t, "GET", "/v1/whoami", "Bearer "+key)
	ts.do(t, "GET", "/v1/whoami?api_key="+key, "Bearer "+key)
	ts.do(t, "GET", "/v1/whoami", "Bearer "+unknown)
	ts.do(t, "GET", "/v1/whoami", "Basic "+key)
	ts.do(t, "GET", "/healthz", "Bearer "+key)

	logs := ts.logs.String()
	for _, secret := range []string{
		key, unknown,
		ids.HashAPIKey(key), ids.HashAPIKey(unknown),
		ids.APIKeyDisplayPrefix(key), ids.APIKeyDisplayPrefix(unknown),
		"Bearer", "Basic", "Authorization",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("log contains %q:\n%s", secret, logs)
		}
	}
	// The log still says who made the request and how it went.
	for _, want := range []string{"path=/v1/whoami", "status=200", "status=401", "account_id=" + acctID} {
		if !strings.Contains(logs, want) {
			t.Errorf("log does not contain %q:\n%s", want, logs)
		}
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(t, "GET", "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %s", got)
	}

	ts.st.Close()
	decodeError(t, ts.do(t, "GET", "/healthz", ""), http.StatusServiceUnavailable, CodeUnavailable)
}

func TestNotFoundIsJSON(t *testing.T) {
	ts := newTestServer(t)
	decodeError(t, ts.do(t, "GET", "/nope", ""), http.StatusNotFound, CodeNotFound)
	decodeError(t, ts.do(t, "POST", "/healthz", ""), http.StatusMethodNotAllowed, CodeMethodNotAllowed)
}

func TestPanicIsJSON500(t *testing.T) {
	ts := newTestServer(t)
	ts.srv.mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	decodeError(t, ts.do(t, "GET", "/boom", ""), http.StatusInternalServerError, CodeInternal)
}

func TestServeShutsDownGracefully(t *testing.T) {
	ts := newTestServer(t)
	started := make(chan struct{})
	release := make(chan struct{})
	ts.srv.mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		writeJSON(w, http.StatusOK, map[string]string{"done": "yes"})
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ts.srv.Serve(ctx, ln) }()

	got := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			t.Errorf("in-flight request: %v", err)
			got <- 0
			return
		}
		resp.Body.Close()
		got <- resp.StatusCode
	}()

	<-started
	cancel()
	// The in-flight request finishes before Serve returns.
	select {
	case err := <-served:
		t.Fatalf("Serve returned %v before the in-flight request finished", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if code := <-got; code != http.StatusOK {
		t.Errorf("in-flight request status = %d, want 200", code)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve = %v, want nil", err)
	}
	if _, err := http.Get("http://" + ln.Addr().String() + "/healthz"); err == nil {
		t.Error("server still answers after shutdown")
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	ts := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if err := ts.srv.Serve(context.Background(), ln); err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve on a closed listener = %v, want an error", err)
	}
}

// TestOversizedBodyOverTCP sends bodies far over the limit on a real
// connection, the way nginx does: the whole body is written before the
// response is read. The rest of the body is drained, so the 413 always
// arrives (no RST from closing with unread data) and the connection stays
// usable for the next request.
func TestOversizedBodyOverTCP(t *testing.T) {
	ts := newTestServer(t)
	WithMaxPayloadBytes(1000)(ts.srv)
	_, key := ts.newKey(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- ts.srv.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-served })

	for _, path := range []string{"/v1/events", "/v1/endpoints"} {
		t.Run(path, func(t *testing.T) {
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))

			body := `{"event_type":"big","url":"https://example.com","payload":{"x":"` + strings.Repeat("a", 1<<20) + `"}}`
			req := "POST " + path + " HTTP/1.1\r\nHost: hookd\r\nAuthorization: Bearer " + key +
				"\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body +
				"GET /healthz HTTP/1.1\r\nHost: hookd\r\n\r\n"
			wrote := make(chan error, 1)
			go func() { _, err := io.WriteString(conn, req); wrote <- err }()
			if err := <-wrote; err != nil {
				t.Fatalf("write request: %v", err)
			}

			br := bufio.NewReader(conn)
			for _, want := range []int{http.StatusRequestEntityTooLarge, http.StatusOK} {
				resp, err := http.ReadResponse(br, nil)
				if err != nil {
					t.Fatalf("read response (want %d): %v", want, err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != want {
					t.Fatalf("status = %d, want %d", resp.StatusCode, want)
				}
			}
		})
	}
}
