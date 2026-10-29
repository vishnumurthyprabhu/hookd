package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

	"hookd/internal/ids"
	"hookd/internal/signing"
)

const testBody = `{"type":"order.paid","data":{"id":42}}`

// newReceiver starts the receiver handler answering with status and returns
// its URL, its secret and the buffer it logs to.
func newReceiver(t *testing.T, status int) (string, string, *bytes.Buffer) {
	t.Helper()
	secret, err := ids.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	wh, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	srv := httptest.NewServer(newHandler(wh, status, slog.New(slog.NewTextHandler(&logs, nil))))
	t.Cleanup(srv.Close)
	return srv.URL, secret, &logs
}

// post sends body to url with header and returns the response status.
func post(t *testing.T, url string, header http.Header, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func sign(t *testing.T, secret, msgID, body string) http.Header {
	t.Helper()
	h, err := signing.Sign(secret, msgID, time.Now(), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestSignedRequestVerifies(t *testing.T) {
	url, secret, logs := newReceiver(t, http.StatusOK)
	msgID := ids.NewMessageID()

	if got := post(t, url, sign(t, secret, msgID, testBody), testBody); got != http.StatusOK {
		t.Errorf("status = %d, want 200", got)
	}
	out := logs.String()
	for _, want := range []string{"msg=verified", "verified=true", "id=" + msgID, "type=order.paid", `"id\":42`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
}

func TestTamperedRequestIsNotVerified(t *testing.T) {
	cases := map[string]func(secret string) (http.Header, string){
		"body changed": func(secret string) (http.Header, string) {
			return sign(t, secret, ids.NewMessageID(), testBody), strings.Replace(testBody, "42", "43", 1)
		},
		"id changed": func(secret string) (http.Header, string) {
			h := sign(t, secret, ids.NewMessageID(), testBody)
			h.Set(signing.HeaderID, ids.NewMessageID())
			return h, testBody
		},
		"other secret": func(string) (http.Header, string) {
			other, err := ids.NewSecret()
			if err != nil {
				t.Fatal(err)
			}
			return sign(t, other, ids.NewMessageID(), testBody), testBody
		},
		"unsigned": func(string) (http.Header, string) {
			return http.Header{}, testBody
		},
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			url, secret, logs := newReceiver(t, http.StatusOK)
			header, body := tamper(secret)
			if got := post(t, url, header, body); got != http.StatusOK {
				t.Errorf("status = %d, want 200", got)
			}
			out := logs.String()
			if !strings.Contains(out, `msg="not verified"`) || !strings.Contains(out, "verified=false") {
				t.Errorf("log %q does not say not verified", out)
			}
		})
	}
}

func TestConfiguredStatus(t *testing.T) {
	url, secret, logs := newReceiver(t, http.StatusInternalServerError)
	if got := post(t, url, sign(t, secret, ids.NewMessageID(), testBody), testBody); got != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", got)
	}
	if !strings.Contains(logs.String(), "verified=true") {
		t.Errorf("log %q does not say verified", logs.String())
	}
}

func TestRunRejectsBadFlags(t *testing.T) {
	for name, args := range map[string][]string{
		"no secret":    {},
		"bad secret":   {"--secret", "whsec_!!!"},
		"bad status":   {"--secret", "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "--status", "42"},
		"unknown flag": {"--nope"},
	} {
		t.Run(name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := run(args, &stderr); got != 2 {
				t.Errorf("exit = %d, want 2 (stderr %q)", got, stderr.String())
			}
		})
	}
}
