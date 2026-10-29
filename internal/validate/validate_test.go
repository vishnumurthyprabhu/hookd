package validate

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestURL(t *testing.T) {
	cases := []struct {
		name, url, code, msg string
	}{
		{"https", "https://example.com/hook", "", ""},
		{"http with port and query", "http://127.0.0.1:9000/in?x=1", "", ""},
		{"longest", "https://example.com/" + strings.Repeat("a", MaxURLLen-len("https://example.com/")), "", ""},
		{"empty", "", CodeInvalidURL, "url is required"},
		{"too long", "https://example.com/" + strings.Repeat("a", MaxURLLen), CodeInvalidURL, "url must be at most 2048 bytes"},
		{"relative", "/hook", CodeInvalidURL, "url must be an absolute http or https URL with a host"},
		{"ftp", "ftp://example.com/hook", CodeInvalidURL, "url must be an absolute http or https URL with a host"},
		{"no host", "https:///hook", CodeInvalidURL, "url must be an absolute http or https URL with a host"},
		{"opaque", "https:example.com", CodeInvalidURL, "url must be an absolute http or https URL with a host"},
		{"unparsable", "https://exa mple.com/%zz", CodeInvalidURL, "url must be an absolute http or https URL with a host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg := URL(c.url)
			if code != c.code || msg != c.msg {
				t.Errorf("URL(%q) = %q, %q; want %q, %q", c.url, code, msg, c.code, c.msg)
			}
		})
	}
}

func TestDescription(t *testing.T) {
	cases := []struct {
		name, desc, code, msg string
	}{
		{"empty", "", "", ""},
		{"longest", strings.Repeat("a", MaxDescriptionLen), "", ""},
		{"too long", strings.Repeat("a", MaxDescriptionLen+1), CodeInvalidRequest, "description must be at most 1024 bytes"},
		{"too long in bytes", strings.Repeat("é", MaxDescriptionLen/2+1), CodeInvalidRequest, "description must be at most 1024 bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg := Description(c.desc)
			if code != c.code || msg != c.msg {
				t.Errorf("Description = %q, %q; want %q, %q", code, msg, c.code, c.msg)
			}
		})
	}
}

func TestValidEventType(t *testing.T) {
	cases := []struct {
		typ  string
		want bool
	}{
		{"order.paid", true},
		{"A-z_0.9", true},
		{strings.Repeat("a", 128), true},
		{"", false},
		{strings.Repeat("a", 129), false},
		{"order paid", false},
		{"order/paid", false},
		{"ordér", false},
		{"order.paid\n", false},
	}
	for _, c := range cases {
		if got := ValidEventType(c.typ); got != c.want {
			t.Errorf("ValidEventType(%q) = %v, want %v", c.typ, got, c.want)
		}
	}
}

func TestEventType(t *testing.T) {
	const bad = "event_type must be 1 to 128 letters, digits, '.', '_' or '-'"
	cases := []struct {
		name, typ, code, msg string
	}{
		{"valid", "order.paid", "", ""},
		{"empty", "", CodeInvalidEventType, bad},
		{"space", "order paid", CodeInvalidEventType, bad},
		{"too long", strings.Repeat("a", 129), CodeInvalidEventType, bad},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg := EventType(c.typ)
			if code != c.code || msg != c.msg {
				t.Errorf("EventType(%q) = %q, %q; want %q, %q", c.typ, code, msg, c.code, c.msg)
			}
		})
	}
}

func TestEventTypes(t *testing.T) {
	many := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("t%d", i)
		}
		return out
	}
	cases := []struct {
		name  string
		types []string
		want  []string
		code  string
		msg   string
	}{
		{"nil means all", nil, []string{}, "", ""},
		{"empty means all", []string{}, []string{}, "", ""},
		{"kept in order", []string{"b", "a"}, []string{"b", "a"}, "", ""},
		{"duplicates dropped", []string{"a", "b", "a"}, []string{"a", "b"}, "", ""},
		{"most allowed", many(MaxEventTypes), many(MaxEventTypes), "", ""},
		{"too many", many(MaxEventTypes + 1), nil, CodeInvalidEventType, "event_types may list at most 100 types"},
		{"empty type", []string{""}, nil, CodeInvalidEventType, `event type "" must be 1 to 128 letters, digits, '.', '_' or '-'`},
		{"bad type after good", []string{"ok", "order/paid"}, nil, CodeInvalidEventType, `event type "order/paid" must be 1 to 128 letters, digits, '.', '_' or '-'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, code, msg := EventTypes(c.types)
			if code != c.code || msg != c.msg {
				t.Errorf("EventTypes code, msg = %q, %q; want %q, %q", code, msg, c.code, c.msg)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("EventTypes list = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestIdempotencyKey(t *testing.T) {
	const tooLong = "Idempotency-Key must be at most 255 bytes"
	cases := []struct {
		name, raw, key, code, msg string
	}{
		{"none", "", "", "", ""},
		{"spaces only", "   ", "", "", ""},
		{"trimmed", "  key-1 ", "key-1", "", ""},
		{"longest", strings.Repeat("k", MaxIdempotencyKeyLen), strings.Repeat("k", MaxIdempotencyKeyLen), "", ""},
		{"longest with spaces", " " + strings.Repeat("k", MaxIdempotencyKeyLen) + " ", strings.Repeat("k", MaxIdempotencyKeyLen), "", ""},
		{"too long", strings.Repeat("k", MaxIdempotencyKeyLen+1), "", CodeInvalidIdempotencyKey, tooLong},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key, code, msg := IdempotencyKey(c.raw)
			if key != c.key || code != c.code || msg != c.msg {
				t.Errorf("IdempotencyKey(%q) = %q, %q, %q; want %q, %q, %q", c.raw, key, code, msg, c.key, c.code, c.msg)
			}
		})
	}
}

func TestPayload(t *testing.T) {
	const max = 20
	cases := []struct {
		name, payload, code, msg string
	}{
		{"object", `{"a":1}`, "", ""},
		{"empty object", `{}`, "", ""},
		{"object with spaces", " \n{\"a\":1}\n", "", ""},
		{"at the limit", `{"a":"` + strings.Repeat("x", max-8) + `"}`, "", ""},
		{"empty", "", CodeInvalidPayload, "payload is required"},
		{"spaces only", "  \n", CodeInvalidPayload, "payload is required"},
		{"not JSON", `{"a":`, CodeInvalidPayload, "payload is not valid JSON"},
		{"two values", `{} {}`, CodeInvalidPayload, "payload is not valid JSON"},
		{"array", `[1,2]`, CodeInvalidPayload, "payload must be a JSON object"},
		{"string", `"hi"`, CodeInvalidPayload, "payload must be a JSON object"},
		{"null", `null`, CodeInvalidPayload, "payload must be a JSON object"},
		{"over the limit", `{"a":"` + strings.Repeat("x", max-7) + `"}`, CodePayloadTooLarge, "payload is larger than 20 bytes"},
		{"large non-object is not an object", `[` + strings.Repeat("1,", max) + `1]`, CodeInvalidPayload, "payload must be a JSON object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, msg := Payload([]byte(c.payload), max)
			if code != c.code || msg != c.msg {
				t.Errorf("Payload(%q) = %q, %q; want %q, %q", c.payload, code, msg, c.code, c.msg)
			}
		})
	}
}
