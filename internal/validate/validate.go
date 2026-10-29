// Package validate holds the input checks shared by the HTTP API and the
// admin UI, so both apply the same rules. Each check returns an error code
// and a message, both empty when the input is valid.
package validate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Error codes the checks return. The API answers with them unchanged.
const (
	CodeInvalidRequest        = "invalid_request"
	CodeInvalidURL            = "invalid_url"
	CodeInvalidEventType      = "invalid_event_type"
	CodeInvalidPayload        = "invalid_payload"
	CodePayloadTooLarge       = "payload_too_large"
	CodeInvalidIdempotencyKey = "invalid_idempotency_key"
)

const (
	// MaxURLLen is the longest endpoint URL accepted.
	MaxURLLen = 2048
	// MaxDescriptionLen is the longest endpoint description accepted, in
	// bytes.
	MaxDescriptionLen = 1024
	// MaxEventTypes is the most event types one endpoint may subscribe to.
	MaxEventTypes = 100
	// MaxIdempotencyKeyLen is the longest idempotency key accepted.
	MaxIdempotencyKeyLen = 255
)

// eventTypePattern is what an event type name looks like (R8).
var eventTypePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ValidEventType reports whether t is a valid event type name (R8).
func ValidEventType(t string) bool {
	return eventTypePattern.MatchString(t)
}

// URL checks that raw is an absolute http or https URL with a host (R4).
func URL(raw string) (code, msg string) {
	const want = "url must be an absolute http or https URL with a host"
	if raw == "" {
		return CodeInvalidURL, "url is required"
	}
	if len(raw) > MaxURLLen {
		return CodeInvalidURL, fmt.Sprintf("url must be at most %d bytes", MaxURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" {
		return CodeInvalidURL, want
	}
	return "", ""
}

// Description checks the length of an endpoint description.
func Description(d string) (code, msg string) {
	if len(d) > MaxDescriptionLen {
		return CodeInvalidRequest, fmt.Sprintf("description must be at most %d bytes", MaxDescriptionLen)
	}
	return "", ""
}

// EventType checks the event type of one event (R8).
func EventType(t string) (code, msg string) {
	if !ValidEventType(t) {
		return CodeInvalidEventType, "event_type must be 1 to 128 letters, digits, '.', '_' or '-'"
	}
	return "", ""
}

// EventTypes checks each event type name of an endpoint's list (R8) and
// returns the list without duplicates, in the order given. An empty list
// means all types.
func EventTypes(types []string) (out []string, code, msg string) {
	if len(types) > MaxEventTypes {
		return nil, CodeInvalidEventType, fmt.Sprintf("event_types may list at most %d types", MaxEventTypes)
	}
	out = []string{}
	seen := map[string]bool{}
	for _, t := range types {
		if !ValidEventType(t) {
			return nil, CodeInvalidEventType,
				fmt.Sprintf("event type %q must be 1 to 128 letters, digits, '.', '_' or '-'", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, "", ""
}

// IdempotencyKey trims spaces around raw and checks its length. An empty
// key means none was given.
func IdempotencyKey(raw string) (key, code, msg string) {
	key = strings.TrimSpace(raw)
	if len(key) > MaxIdempotencyKeyLen {
		return "", CodeInvalidIdempotencyKey,
			fmt.Sprintf("Idempotency-Key must be at most %d bytes", MaxIdempotencyKeyLen)
	}
	return key, "", ""
}

// Payload checks an event payload: it must be a JSON object of at most
// maxBytes bytes. Spaces around the object are allowed and count towards
// the size.
func Payload(p []byte, maxBytes int64) (code, msg string) {
	trimmed := bytes.TrimSpace(p)
	if len(trimmed) == 0 {
		return CodeInvalidPayload, "payload is required"
	}
	if !json.Valid(trimmed) {
		return CodeInvalidPayload, "payload is not valid JSON"
	}
	if trimmed[0] != '{' {
		return CodeInvalidPayload, "payload must be a JSON object"
	}
	if int64(len(p)) > maxBytes {
		return CodePayloadTooLarge, PayloadTooLargeMessage(maxBytes)
	}
	return "", ""
}

// PayloadTooLargeMessage is the message for a payload over maxBytes bytes.
func PayloadTooLargeMessage(maxBytes int64) string {
	return fmt.Sprintf("payload is larger than %d bytes", maxBytes)
}
