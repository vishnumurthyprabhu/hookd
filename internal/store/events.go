package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"hookd/internal/ids"
)

// CreateEvent implements Store.
func (s *SQLStore) CreateEvent(ctx context.Context, accountID string, in NewEvent) (Event, bool, error) {
	now := s.nowUTC()
	e := Event{
		ID:             ids.NewMessageID(),
		AccountID:      accountID,
		EventType:      in.EventType,
		Payload:        slices.Clone(in.Payload),
		IdempotencyKey: in.IdempotencyKey,
		CreatedAt:      now,
		Deliveries:     []DeliveryStatus{},
	}
	created := true

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var id string
		err := s.queryRow(ctx, tx, `SELECT id FROM accounts WHERE id = ?`, accountID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: create event: %w", err)
		}

		var key any // NULL when there is no idempotency key
		if in.IdempotencyKey != "" {
			key = in.IdempotencyKey
			prev, found, err := s.eventIDByKey(ctx, tx, accountID, in.IdempotencyKey)
			if err != nil {
				return err
			}
			if found {
				if prev.createdAt.Time.After(now.Add(-s.idempotencyWindow)) {
					created = false
					e, err = s.getEvent(ctx, tx, accountID, prev.id)
					return err
				}
				// The match is outside the window: free the key for the new event.
				if _, err := s.exec(ctx, tx, `UPDATE events SET idempotency_key = NULL WHERE id = ?`, prev.id); err != nil {
					return fmt.Errorf("store: create event: clear old idempotency key: %w", err)
				}
			}
		}

		// A concurrent request with the same key may have inserted its event
		// since the lookup above. Then this insert does nothing, and that
		// event is the answer.
		res, err := s.exec(ctx, tx,
			`INSERT INTO events (id, account_id, event_type, payload, idempotency_key, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (account_id, idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`,
			e.ID, e.AccountID, e.EventType, string(e.Payload), key, s.d.timeArg(e.CreatedAt))
		if err != nil {
			return fmt.Errorf("store: create event: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: create event: %w", err)
		}
		if n == 0 {
			prev, found, err := s.eventIDByKey(ctx, tx, accountID, in.IdempotencyKey)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("store: create event: idempotency key conflict without a matching event")
			}
			created = false
			e, err = s.getEvent(ctx, tx, accountID, prev.id)
			return err
		}

		targets, err := s.targetEndpoints(ctx, tx, accountID, e.EventType)
		if err != nil {
			return err
		}
		for _, epID := range targets {
			if _, err := s.exec(ctx, tx,
				`INSERT INTO deliveries (id, event_id, endpoint_id, status, created_at) VALUES (?, ?, ?, ?, ?)`,
				ids.NewDeliveryID(), e.ID, epID, DeliveryPending, s.d.timeArg(now)); err != nil {
				return fmt.Errorf("store: create event: insert delivery: %w", err)
			}
			e.Deliveries = append(e.Deliveries, DeliveryStatus{EndpointID: epID, Status: DeliveryPending})
		}
		return nil
	})
	if err != nil {
		return Event{}, false, err
	}
	return e, created, nil
}

// keyedEvent identifies the event holding an idempotency key.
type keyedEvent struct {
	id        string
	createdAt dbTime
}

// eventIDByKey finds the account's event holding an idempotency key.
func (s *SQLStore) eventIDByKey(ctx context.Context, q queryer, accountID, key string) (keyedEvent, bool, error) {
	var k keyedEvent
	err := s.queryRow(ctx, q,
		`SELECT id, created_at FROM events WHERE account_id = ? AND idempotency_key = ?`,
		accountID, key).Scan(&k.id, &k.createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return keyedEvent{}, false, nil
	}
	if err != nil {
		return keyedEvent{}, false, fmt.Errorf("store: look up idempotency key: %w", err)
	}
	return k, true, nil
}

// targetEndpoints returns the IDs of the account's enabled, non-deleted
// endpoints that subscribe to eventType, ordered by ID.
func (s *SQLStore) targetEndpoints(ctx context.Context, q queryer, accountID, eventType string) ([]string, error) {
	rows, err := s.query(ctx, q,
		`SELECT id, event_types FROM endpoints
		WHERE account_id = ? AND enabled = ? AND deleted_at IS NULL
		ORDER BY id`,
		accountID, true)
	if err != nil {
		return nil, fmt.Errorf("store: select target endpoints: %w", err)
	}
	defer rows.Close()

	var targets []string
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("store: select target endpoints: %w", err)
		}
		var types []string
		if err := json.Unmarshal([]byte(raw), &types); err != nil {
			return nil, fmt.Errorf("store: decode event_types of %s: %w", id, err)
		}
		if len(types) == 0 || slices.Contains(types, eventType) {
			targets = append(targets, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: select target endpoints: %w", err)
	}
	return targets, nil
}

// GetEvent implements Store.
func (s *SQLStore) GetEvent(ctx context.Context, accountID, id string) (Event, error) {
	return s.getEvent(ctx, s.db, accountID, id)
}

func (s *SQLStore) getEvent(ctx context.Context, q queryer, accountID, id string) (Event, error) {
	var (
		e       Event
		payload string
		key     sql.NullString
		created dbTime
	)
	err := s.queryRow(ctx, q,
		`SELECT id, account_id, event_type, payload, idempotency_key, created_at
		FROM events WHERE account_id = ? AND id = ?`,
		accountID, id).Scan(&e.ID, &e.AccountID, &e.EventType, &payload, &key, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("store: get event: %w", err)
	}
	e.Payload = []byte(payload)
	e.IdempotencyKey = key.String
	e.CreatedAt = created.Time

	rows, err := s.query(ctx, q,
		`SELECT endpoint_id, status, response_status, error FROM deliveries
		WHERE event_id = ?
		ORDER BY endpoint_id`,
		e.ID)
	if err != nil {
		return Event{}, fmt.Errorf("store: get event deliveries: %w", err)
	}
	defer rows.Close()

	e.Deliveries = []DeliveryStatus{}
	for rows.Next() {
		var (
			d          DeliveryStatus
			respStatus sql.NullInt64
			errText    sql.NullString
		)
		if err := rows.Scan(&d.EndpointID, &d.Status, &respStatus, &errText); err != nil {
			return Event{}, fmt.Errorf("store: get event deliveries: %w", err)
		}
		d.ResponseStatus = int(respStatus.Int64)
		d.Error = errText.String
		e.Deliveries = append(e.Deliveries, d)
	}
	if err := rows.Err(); err != nil {
		return Event{}, fmt.Errorf("store: get event deliveries: %w", err)
	}
	return e, nil
}
