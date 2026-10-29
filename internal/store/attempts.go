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

const attemptColumns = `id, account_id, delivery_id, event_id, endpoint_id, number,
	started_at, outcome, response_status, latency_ms, error`

// ListEventAttempts implements Store. Attempt IDs are ULIDs, so ordering by
// ID is ordering by time. The list uses the (event_id, id) index.
func (s *SQLStore) ListEventAttempts(ctx context.Context, accountID, eventID string, f AttemptFilter) ([]Attempt, error) {
	if f.Limit <= 0 {
		return nil, fmt.Errorf("store: list event attempts: limit %d is not positive", f.Limit)
	}
	var one int
	err := s.queryRow(ctx, s.db, `SELECT 1 FROM events WHERE account_id = ? AND id = ?`, accountID, eventID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: list event attempts: event: %w", err)
	}
	where, args := `event_id = ?`, []any{eventID}
	if f.EndpointID != "" {
		where += ` AND endpoint_id = ?`
		args = append(args, f.EndpointID)
	}
	if f.After != "" {
		where += ` AND id > ?`
		args = append(args, f.After)
	}
	args = append(args, f.Limit)
	return s.listAttempts(ctx, "list event attempts",
		`SELECT `+attemptColumns+` FROM attempts WHERE `+where+` ORDER BY id LIMIT ?`, args...)
}

// ListEndpointAttempts implements Store. Deleted endpoints are only
// soft-deleted, so their attempts are still found. The list uses the
// (endpoint_id, id) or (endpoint_id, outcome, id) index.
func (s *SQLStore) ListEndpointAttempts(ctx context.Context, accountID, endpointID string, f AttemptFilter) ([]Attempt, error) {
	if f.Limit <= 0 {
		return nil, fmt.Errorf("store: list endpoint attempts: limit %d is not positive", f.Limit)
	}
	if f.Outcome != "" && f.Outcome != AttemptSucceeded && f.Outcome != AttemptFailed {
		return nil, fmt.Errorf("store: list endpoint attempts: outcome %q is not known", f.Outcome)
	}
	var one int
	err := s.queryRow(ctx, s.db, `SELECT 1 FROM endpoints WHERE account_id = ? AND id = ?`, accountID, endpointID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: list endpoint attempts: endpoint: %w", err)
	}
	where, args := `endpoint_id = ?`, []any{endpointID}
	if f.Outcome != "" {
		where += ` AND outcome = ?`
		args = append(args, f.Outcome)
	}
	if f.Before != "" {
		where += ` AND id < ?`
		args = append(args, f.Before)
	}
	args = append(args, f.Limit)
	return s.listAttempts(ctx, "list endpoint attempts",
		`SELECT `+attemptColumns+` FROM attempts WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...)
}

// GetAttempt implements Store.
func (s *SQLStore) GetAttempt(ctx context.Context, accountID, id string) (Attempt, error) {
	a, err := scanAttempt(s.queryRow(ctx, s.db,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = ? AND account_id = ?`, id, accountID))
	if errors.Is(err, sql.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, fmt.Errorf("store: get attempt: %w", err)
	}
	return a, nil
}

func (s *SQLStore) listAttempts(ctx context.Context, op, query string, args ...any) ([]Attempt, error) {
	rows, err := s.query(ctx, s.db, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", op, err)
	}
	defer rows.Close()
	attempts := []Attempt{}
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("store: %s: %w", op, err)
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: %s: %w", op, err)
	}
	return attempts, nil
}

func scanAttempt(sc scanner) (Attempt, error) {
	var (
		a          Attempt
		started    dbTime
		respStatus sql.NullInt64
		latencyMS  int64
		errText    sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.AccountID, &a.DeliveryID, &a.EventID, &a.EndpointID, &a.Number,
		&started, &a.Outcome, &respStatus, &latencyMS, &errText); err != nil {
		return Attempt{}, err
	}
	a.StartedAt = started.Time
	a.ResponseStatus = int(respStatus.Int64)
	a.Latency = time.Duration(latencyMS) * time.Millisecond
	a.Error = errText.String
	return a, nil
}
