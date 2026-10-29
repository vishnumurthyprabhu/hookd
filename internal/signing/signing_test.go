package signing

import (
	"bytes"
	"strings"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

	"hookd/internal/ids"
)

// Test vector from the Standard Webhooks reference libraries.
const (
	vectorSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	vectorMsgID     = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vectorTimestamp = 1614265330
	vectorBody      = `{"test": 2432232314}`
	vectorSignature = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func TestSignKnownVector(t *testing.T) {
	h, err := Sign(vectorSecret, vectorMsgID, time.Unix(vectorTimestamp, 0), []byte(vectorBody))
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get(HeaderID); got != vectorMsgID {
		t.Errorf("webhook-id = %q, want %q", got, vectorMsgID)
	}
	if got := h.Get(HeaderTimestamp); got != "1614265330" {
		t.Errorf("webhook-timestamp = %q, want %q", got, "1614265330")
	}
	if got := h.Get(HeaderSignature); got != vectorSignature {
		t.Errorf("webhook-signature = %q, want %q", got, vectorSignature)
	}
}

func TestSignVerifiesWithOfficialLibrary(t *testing.T) {
	secret, err := ids.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	wh, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		t.Fatal(err)
	}
	bodies := map[string][]byte{
		"json":    []byte(`{"type":"order.paid","data":{"id":42,"ok":true}}`),
		"empty":   {},
		"unicode": []byte(`{"name":"Zoë 日本語 🚀","note":"naïve café — ünïcödé"}`),
		"large":   []byte(`{"blob":"` + strings.Repeat("abcdefghij", 256*1024/10) + `"}`),
		"binary":  {0x00, 0xff, '.', 0x80, '\n'},
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			h, err := Sign(secret, ids.NewMessageID(), time.Now(), body)
			if err != nil {
				t.Fatal(err)
			}
			if err := wh.Verify(body, h); err != nil {
				t.Fatalf("official library rejected signature: %v", err)
			}
		})
	}
}

func TestTamperedBodyFailsVerification(t *testing.T) {
	secret, err := ids.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	wh, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"amount":100}`)
	h, err := Sign(secret, ids.NewMessageID(), time.Now(), body)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(body, []byte("100"), []byte("900"), 1)
	if err := wh.Verify(tampered, h); err == nil {
		t.Fatal("tampered body verified")
	}
}

func TestTamperedHeadersFailVerification(t *testing.T) {
	secret, err := ids.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	wh, err := standardwebhooks.NewWebhook(secret)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"amount":100}`)
	ts := time.Now()

	h, err := Sign(secret, ids.NewMessageID(), ts, body)
	if err != nil {
		t.Fatal(err)
	}
	h.Set(HeaderID, ids.NewMessageID())
	if err := wh.Verify(body, h); err == nil {
		t.Error("changed webhook-id verified")
	}

	h, err = Sign(secret, ids.NewMessageID(), ts, body)
	if err != nil {
		t.Fatal(err)
	}
	h.Set(HeaderTimestamp, "1")
	if err := wh.VerifyIgnoringTimestamp(body, h); err == nil {
		t.Error("changed webhook-timestamp verified")
	}
}

func TestWrongSecretFailsVerification(t *testing.T) {
	secret, err := ids.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	other, err := ids.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	wh, err := standardwebhooks.NewWebhook(other)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"amount":100}`)
	h, err := Sign(secret, ids.NewMessageID(), time.Now(), body)
	if err != nil {
		t.Fatal(err)
	}
	if err := wh.Verify(body, h); err == nil {
		t.Fatal("signature verified with the wrong secret")
	}
}

func TestSignAcceptsSecretWithoutPrefix(t *testing.T) {
	ts := time.Unix(vectorTimestamp, 0)
	h, err := Sign(strings.TrimPrefix(vectorSecret, "whsec_"), vectorMsgID, ts, []byte(vectorBody))
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Get(HeaderSignature); got != vectorSignature {
		t.Errorf("webhook-signature = %q, want %q", got, vectorSignature)
	}
}

func TestSignRejectsBadSecret(t *testing.T) {
	for _, secret := range []string{"", "whsec_", "whsec_not base64!"} {
		if _, err := Sign(secret, vectorMsgID, time.Now(), []byte(vectorBody)); err == nil {
			t.Errorf("Sign(%q) succeeded, want error", secret)
		}
	}
}
