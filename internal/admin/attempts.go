package admin

import (
	"net/http"

	"hookd/internal/store"
)

// attemptsData is the "Recent attempts" section of an endpoint page (R16).
type attemptsData struct {
	Attempts []store.Attempt
	// Outcome is the outcome filter, or empty for every attempt.
	Outcome string
	// AttemptsNext is the cursor of the next page, or empty on the last
	// page.
	AttemptsNext string
	// AttemptsPaged is true when this is not the first page.
	AttemptsPaged bool
}

// loadEndpointAttempts loads the endpoint's attempts across its events,
// newest first, pageSize at a time. The ?attempts_before= cursor is the ID
// of the last attempt of the previous page, and ?outcome= keeps only
// succeeded or failed attempts; any other outcome shows them all.
func (h *Handler) loadEndpointAttempts(r *http.Request, acctID, epID string) (attemptsData, error) {
	q := r.URL.Query()
	data := attemptsData{Outcome: q.Get("outcome"), AttemptsPaged: q.Get("attempts_before") != ""}
	if data.Outcome != store.AttemptSucceeded && data.Outcome != store.AttemptFailed {
		data.Outcome = ""
	}
	attempts, err := h.store.ListEndpointAttempts(r.Context(), acctID, epID, store.AttemptFilter{
		Outcome: data.Outcome,
		Before:  q.Get("attempts_before"),
		Limit:   pageSize + 1,
	})
	if err != nil {
		return attemptsData{}, err
	}
	if len(attempts) > pageSize {
		attempts = attempts[:pageSize]
		data.AttemptsNext = attempts[pageSize-1].ID
	}
	data.Attempts = attempts
	return data, nil
}
