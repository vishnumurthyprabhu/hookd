package api

import (
	"encoding/json"
	"net/http"

	"hookd/internal/validate"
)

// Error codes returned in the "code" field of error responses. The input
// check codes come from package validate.
const (
	CodeUnauthorized     = "unauthorized"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeUnavailable      = "unavailable"
	CodeInternal         = "internal_error"

	CodeInvalidJSON           = "invalid_json"
	CodeInvalidRequest        = validate.CodeInvalidRequest
	CodeInvalidURL            = validate.CodeInvalidURL
	CodeInvalidEventType      = validate.CodeInvalidEventType
	CodeInvalidPayload        = validate.CodeInvalidPayload
	CodePayloadTooLarge       = validate.CodePayloadTooLarge
	CodeInvalidIdempotencyKey = validate.CodeInvalidIdempotencyKey
)

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError writes a JSON error response: {"error":{"code":…,"message":…}}.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
