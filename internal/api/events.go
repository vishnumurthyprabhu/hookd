package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"hookd/internal/store"
)

const (
	// envelopeOverhead is how much larger than the payload limit a
	// POST /v1/events body may be, to leave room for the event type and the
	// JSON around the payload.
	envelopeOverhead = 4 << 10
	// maxIdempotencyKeyLen is the longest Idempotency-Key header accepted.
	maxIdempotencyKeyLen = 255
)

// eventTypePattern is what an event type name looks like (R8).
var eventTypePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// validEventType reports whether t is a valid event type name (R8).
func validEventType(t string) bool {
	return eventTypePattern.MatchString(t)
}

type createEventRequest struct {
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload"`
}

type eventResponse struct {
	ID             string             `json:"id"`
	EventType      string             `json:"event_type"`
	Payload        json.RawMessage    `json:"payload"`
	IdempotencyKey string             `json:"idempotency_key,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
	Deliveries     []deliveryResponse `json:"deliveries"`
}

type deliveryResponse struct {
	EndpointID string `json:"endpoint_id"`
	// Status is pending, succeeded or failed. A delivery a worker is
	// sending right now is still pending.
	Status string `json:"status"`
	// ResponseStatus is the endpoint's HTTP status, or null if there was none.
	ResponseStatus *int   `json:"response_status"`
	Error          string `json:"error,omitempty"`
}

func newEventResponse(e store.Event) eventResponse {
	out := eventResponse{
		ID:             e.ID,
		EventType:      e.EventType,
		Payload:        json.RawMessage(e.Payload),
		IdempotencyKey: e.IdempotencyKey,
		CreatedAt:      e.CreatedAt.UTC(),
		Deliveries:     make([]deliveryResponse, 0, len(e.Deliveries)),
	}
	for _, d := range e.Deliveries {
		dr := deliveryResponse{EndpointID: d.EndpointID, Status: d.Status, Error: d.Error}
		if d.Status == store.DeliveryInProgress {
			dr.Status = store.DeliveryPending
		}
		if d.ResponseStatus != 0 {
			status := d.ResponseStatus
			dr.ResponseStatus = &status
		}
		out.Deliveries = append(out.Deliveries, dr)
	}
	return out
}

// createEvent handles POST /v1/events (R9, R10). It answers 202 with a new
// event, or 200 with the original event when the Idempotency-Key header
// repeats a key from within the idempotency window.
func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if len(key) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, CodeInvalidIdempotencyKey,
			fmt.Sprintf("Idempotency-Key must be at most %d bytes", maxIdempotencyKeyLen))
		return
	}

	in, status, code, msg := s.decodeEvent(w, r)
	if code != "" {
		writeError(w, status, code, msg)
		return
	}
	in.IdempotencyKey = key

	e, created, err := s.store.CreateEvent(r.Context(), acct.ID, in)
	if err != nil {
		s.log.ErrorContext(r.Context(), "create event", "err", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}
	if !created {
		w.Header().Set("Idempotent-Replayed", "true")
		writeJSON(w, http.StatusOK, newEventResponse(e))
		return
	}
	s.signalWake()
	w.Header().Set("Location", "/v1/events/"+e.ID)
	writeJSON(w, http.StatusAccepted, newEventResponse(e))
}

// decodeEvent reads and checks the body of POST /v1/events. On failure it
// returns the status, code and message of the error to answer with.
func (s *Server) decodeEvent(w http.ResponseWriter, r *http.Request) (in store.NewEvent, status int, code, msg string) {
	tooLarge := func() (store.NewEvent, int, string, string) {
		return store.NewEvent{}, http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
			fmt.Sprintf("payload is larger than %d bytes", s.maxPayloadBytes)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxPayloadBytes+envelopeOverhead))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return tooLarge()
		}
		return store.NewEvent{}, http.StatusBadRequest, CodeInvalidRequest, "could not read the request body"
	}

	var req createEventRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntaxErr), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
			return store.NewEvent{}, http.StatusBadRequest, CodeInvalidJSON, "request body is not valid JSON"
		case errors.As(err, &typeErr):
			return store.NewEvent{}, http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("field %q has the wrong type", typeErr.Field)
		default:
			return store.NewEvent{}, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: " + err.Error()
		}
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return store.NewEvent{}, http.StatusBadRequest, CodeInvalidJSON, "request body must hold a single JSON object"
	}

	if !validEventType(req.EventType) {
		return store.NewEvent{}, http.StatusBadRequest, CodeInvalidEventType,
			"event_type must be 1 to 128 letters, digits, '.', '_' or '-'"
	}
	if len(req.Payload) == 0 {
		return store.NewEvent{}, http.StatusBadRequest, CodeInvalidPayload, "payload is required"
	}
	if req.Payload[0] != '{' {
		return store.NewEvent{}, http.StatusBadRequest, CodeInvalidPayload, "payload must be a JSON object"
	}
	if int64(len(req.Payload)) > s.maxPayloadBytes {
		return tooLarge()
	}
	return store.NewEvent{EventType: req.EventType, Payload: req.Payload}, 0, "", ""
}

// signalWake tells the delivery workers that new work is waiting, without
// blocking.
func (s *Server) signalWake() {
	if s.wake == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// getEvent handles GET /v1/events/{id} (R12). Events of other accounts are
// not found.
func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())
	e, err := s.store.GetEvent(r.Context(), acct.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, "event not found")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "get event", "err", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, newEventResponse(e))
}
