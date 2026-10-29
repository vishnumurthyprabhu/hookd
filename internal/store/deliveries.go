package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClaimDelivery implements Store. The claim is a single statement (see
// dialect.claimDelivery), so two callers never get the same row.
func (s *SQLStore) ClaimDelivery(ctx context.Context, lease time.Duration) (ClaimedDelivery, bool, error) {
	if lease <= 0 {
		return ClaimedDelivery{}, false, fmt.Errorf("store: claim delivery: lease %v is not positive", lease)
	}
	now := s.nowUTC()
	d := ClaimedDelivery{LockedUntil: now.Add(lease)}
	err := s.queryRow(ctx, s.db, s.d.claimDelivery, s.d.timeArg(d.LockedUntil), s.d.timeArg(now), s.d.timeArg(now)).
		Scan(&d.ID, &d.EventID, &d.EndpointID, &d.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ClaimedDelivery{}, false, nil
	}
	if err != nil {
		return ClaimedDelivery{}, false, fmt.Errorf("store: claim delivery: %w", err)
	}
	return d, true, nil
}

// GetDeliveryJob implements Store. It is not scoped by account: workers serve
// every account, and the delivery ties the event and endpoint together.
func (s *SQLStore) GetDeliveryJob(ctx context.Context, deliveryID string) (DeliveryJob, error) {
	var (
		j       DeliveryJob
		eventID string
		epID    string
		deleted dbTime
	)
	err := s.queryRow(ctx, s.db,
		`SELECT d.event_id, d.endpoint_id, e.secret, e.deleted_at
		FROM deliveries d JOIN endpoints e ON e.id = d.endpoint_id
		WHERE d.id = ?`,
		deliveryID).Scan(&eventID, &epID, &j.Secret, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return DeliveryJob{}, ErrNotFound
	}
	if err != nil {
		return DeliveryJob{}, fmt.Errorf("store: get delivery job: %w", err)
	}
	j.DeliveryID = deliveryID
	j.EndpointDeleted = deleted.Valid

	row := s.queryRow(ctx, s.db, `SELECT `+endpointColumns+` FROM endpoints WHERE id = ?`, epID)
	if j.Endpoint, err = scanEndpoint(row); err != nil {
		return DeliveryJob{}, fmt.Errorf("store: get delivery job: endpoint: %w", err)
	}

	var (
		payload string
		key     sql.NullString
		created dbTime
	)
	err = s.queryRow(ctx, s.db,
		`SELECT id, account_id, event_type, payload, idempotency_key, created_at FROM events WHERE id = ?`,
		eventID).Scan(&j.Event.ID, &j.Event.AccountID, &j.Event.EventType, &payload, &key, &created)
	if err != nil {
		return DeliveryJob{}, fmt.Errorf("store: get delivery job: event: %w", err)
	}
	j.Event.Payload = []byte(payload)
	j.Event.IdempotencyKey = key.String
	j.Event.CreatedAt = created.Time
	j.Event.Deliveries = []DeliveryStatus{}
	return j, nil
}

// CountDeliveriesByStatus implements Store. The (status, locked_until) index
// covers the query.
func (s *SQLStore) CountDeliveriesByStatus(ctx context.Context) (map[string]int, error) {
	counts := map[string]int{
		DeliveryPending:    0,
		DeliveryInProgress: 0,
		DeliverySucceeded:  0,
		DeliveryFailed:     0,
	}
	rows, err := s.query(ctx, s.db, `SELECT status, count(*) FROM deliveries GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("store: count deliveries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("store: count deliveries: %w", err)
		}
		counts[status] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: count deliveries: %w", err)
	}
	return counts, nil
}

// CountWaitingRetries implements Store.
func (s *SQLStore) CountWaitingRetries(ctx context.Context) (int, error) {
	var n int
	err := s.queryRow(ctx, s.db,
		`SELECT count(*) FROM deliveries WHERE status = ? AND attempts > 0`,
		DeliveryPending).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count waiting retries: %w", err)
	}
	return n, nil
}
