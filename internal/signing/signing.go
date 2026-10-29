package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Standard Webhooks header names.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

// secretPrefix starts every Standard Webhooks secret.
const secretPrefix = "whsec_"

// Sign returns the Standard Webhooks headers for body sent as message msgID at
// time ts. The signature is "v1," followed by the base64 HMAC-SHA256 of
// "{msgID}.{unix seconds}.{body}", keyed with the base64-decoded part of
// secret after "whsec_".
func Sign(secret, msgID string, ts time.Time, body []byte) (http.Header, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return nil, err
	}
	timestamp := strconv.FormatInt(ts.Unix(), 10)

	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(msgID))
	mac.Write([]byte("."))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)

	h := http.Header{}
	h.Set(HeaderID, msgID)
	h.Set(HeaderTimestamp, timestamp)
	h.Set(HeaderSignature, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	return h, nil
}

func decodeSecret(secret string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, secretPrefix))
	if err != nil {
		return nil, fmt.Errorf("decode signing secret: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("decode signing secret: empty key")
	}
	return key, nil
}
