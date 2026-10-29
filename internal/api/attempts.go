package api

import (
	"errors"
	"net/http"
	"time"

	"hookd/internal/store"
)

type attemptResponse struct {
	ID         string    `json:"id"`
	EventID    string    `json:"event_id"`
	EndpointID string    `json:"endpoint_id"`
	Number     int       `json:"number"`
	StartedAt  time.Time `json:"started_at"`
	// Outcome is succeeded or failed.
	Outcome string `json:"outcome"`
	// ResponseStatus is the endpoint's HTTP status, or null if there was none.
	ResponseStatus *int  `json:"response_status"`
	LatencyMS      int64 `json:"latency_ms"`
	// Error is left out on success.
	Error string `json:"error,omitempty"`
}

type attemptListResponse struct {
	Data []attemptResponse `json:"data"`
	// HasMore is true when there are attempts after the last one in Data.
	// Pass its ID as "after" (event list) or "before" (endpoint list) to get
	// them.
	HasMore bool `json:"has_more"`
}

func newAttemptResponse(a store.Attempt) attemptResponse {
	out := attemptResponse{
		ID:         a.ID,
		EventID:    a.EventID,
		EndpointID: a.EndpointID,
		Number:     a.Number,
		StartedAt:  a.StartedAt.UTC(),
		Outcome:    a.Outcome,
		LatencyMS:  a.Latency.Milliseconds(),
		Error:      a.Error,
	}
	if a.ResponseStatus != 0 {
		status := a.ResponseStatus
		out.ResponseStatus = &status
	}
	return out
}

// newAttemptList makes a page from attempts, which were asked for with a
// limit one higher than the page holds.
func newAttemptList(attempts []store.Attempt, limit int) attemptListResponse {
	out := attemptListResponse{Data: make([]attemptResponse, 0, min(len(attempts), limit))}
	if len(attempts) > limit {
		attempts = attempts[:limit]
		out.HasMore = true
	}
	for _, a := range attempts {
		out.Data = append(out.Data, newAttemptResponse(a))
	}
	return out
}

// listEventAttempts handles GET /v1/events/{id}/attempts (R11, R14). It
// returns up to "limit" attempts of the event, oldest first, after the
// attempt ID given as "after", only those to "endpoint_id" if it is given.
func (s *Server) listEventAttempts(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())

	q := r.URL.Query()
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	// Ask for one more than the page holds to learn whether there are more.
	attempts, err := s.store.ListEventAttempts(r.Context(), acct.ID, r.PathValue("id"), store.AttemptFilter{
		EndpointID: q.Get("endpoint_id"),
		After:      q.Get("after"),
		Limit:      limit + 1,
	})
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, "event not found")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "list event attempts", "err", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, newAttemptList(attempts, limit))
}

// listEndpointAttempts handles GET /v1/endpoints/{id}/attempts (R12, R14).
// It returns up to "limit" attempts made to the endpoint, newest first,
// before the attempt ID given as "before", only those with "outcome" if it
// is given. A deleted endpoint's attempts are still listed.
func (s *Server) listEndpointAttempts(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())

	q := r.URL.Query()
	limit, ok := parseLimit(w, q.Get("limit"))
	if !ok {
		return
	}
	outcome := q.Get("outcome")
	if outcome != "" && outcome != store.AttemptSucceeded && outcome != store.AttemptFailed {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, `outcome must be "succeeded" or "failed"`)
		return
	}
	attempts, err := s.store.ListEndpointAttempts(r.Context(), acct.ID, r.PathValue("id"), store.AttemptFilter{
		Outcome: outcome,
		Before:  q.Get("before"),
		Limit:   limit + 1,
	})
	if err != nil {
		s.endpointError(w, r, "list endpoint attempts", err)
		return
	}
	writeJSON(w, http.StatusOK, newAttemptList(attempts, limit))
}

// getAttempt handles GET /v1/attempts/{id} (R13, R14).
func (s *Server) getAttempt(w http.ResponseWriter, r *http.Request) {
	acct, _ := accountFrom(r.Context())
	a, err := s.store.GetAttempt(r.Context(), acct.ID, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeNotFound, "attempt not found")
		return
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "get attempt", "err", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, newAttemptResponse(a))
}
