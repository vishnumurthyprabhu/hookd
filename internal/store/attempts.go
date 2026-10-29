package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"hookd/internal/ids"
)

// RecordAttempt implements Store. The delivery is updated first, guarded by
// its status and attempt count, so of two workers recording the same attempt
// after a lease expired only one gets past it; the other gets ErrNotFound
// and nothing is written.
func (s *SQLStore) RecordAttempt(ctx context.Context, deliveryID string, r AttemptResult) (Attempt, error) {
	if r.Number < 1 {
		return Attempt{}, fmt.Errorf("store: record attempt: number %d is not positive", r.Number)
	}
	status := r.Outcome
	switch {
	case r.Outcome != AttemptSucceeded && r.Outcome != AttemptFailed:
		return Attempt{}, fmt.Errorf("store: record attempt: outcome %q is not known", r.Outcome)
	case r.NextAttemptAt != nil && r.Outcome != AttemptFailed:
		return Attempt{}, fmt.Errorf("store: record attempt: only a failed attempt can have a next attempt")
	case r.NextAttemptAt != nil:
		status = DeliveryPending
	}
	started := r.StartedAt.UTC().Truncate(time.Microsecond)
	if r.StartedAt.IsZero() {
		started = s.nowUTC()
	}
	a := Attempt{
		ID:             ids.NewAttemptID(),
		DeliveryID:     deliveryID,
		Number:         r.Number,
		StartedAt:      started,
		Outcome:        r.Outcome,
		ResponseStatus: r.ResponseStatus,
		Latency:        r.Latency.Truncate(time.Millisecond),
		Error:          r.Error,
	}
	var respStatus, errText any // NULL when there is none
	if r.ResponseStatus != 0 {
		respStatus = r.ResponseStatus
	}
	if r.Error != "" {
		errText = r.Error
	}

	set := `status = ?, attempts = ?, response_status = ?, error = ?, latency_ms = ?, attempted_at = ?, locked_until = NULL`
	args := []any{status, r.Number, respStatus, errText, r.Latency.Milliseconds(), s.d.timeArg(started)}
	if r.NextAttemptAt != nil {
		set += `, next_attempt_at = ?`
		args = append(args, s.d.timeArg(*r.NextAttemptAt))
	}
	args = append(args, deliveryID, DeliveryInProgress, r.Number-1)

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		err := s.queryRow(ctx, tx,
			`UPDATE deliveries SET `+set+`
			WHERE id = ? AND status = ? AND attempts = ?
			RETURNING event_id, endpoint_id`,
			args...).Scan(&a.EventID, &a.EndpointID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: record attempt: update delivery: %w", err)
		}
		if err := s.queryRow(ctx, tx, `SELECT account_id FROM events WHERE id = ?`, a.EventID).Scan(&a.AccountID); err != nil {
			return fmt.Errorf("store: record attempt: event: %w", err)
		}
		if _, err := s.exec(ctx, tx,
			`INSERT INTO attempts (id, account_id, delivery_id, event_id, endpoint_id, number,
				started_at, outcome, response_status, latency_ms, error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.ID, a.AccountID, a.DeliveryID, a.EventID, a.EndpointID, a.Number,
			s.d.timeArg(a.StartedAt), a.Outcome, respStatus, r.Latency.Milliseconds(), errText); err != nil {
			return fmt.Errorf("store: record attempt: insert attempt: %w", err)
		}
		return nil
	})
	if err != nil {
		return Attempt{}, err
	}
	return a, nil
}
