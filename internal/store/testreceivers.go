package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"hookd/internal/ids"
)

// testReceiverColumns are the columns read by scanTestReceiver, in order,
// from test_receivers aliased as r.
const testReceiverColumns = `r.id, r.account_id, r.endpoint_id, r.response_status, r.created_at`

// testRequestColumns are the columns read by scanTestRequest, in order.
const testRequestColumns = `id, receiver_id, received_at, webhook_id, event_type, headers, body, verified, verify_error, response_status`

// TestReceiverPath is the path of the admin route that a test receiver with
// the given ID answers on.
func TestReceiverPath(id string) string {
	return "/admin/test-receiver/" + id
}

// CreateTestEndpoint implements Store.
func (s *SQLStore) CreateTestEndpoint(ctx context.Context, accountID string, in NewTestEndpoint) (TestEndpoint, error) {
	r := TestReceiver{
		ID:             ids.NewTestReceiverID(),
		AccountID:      accountID,
		ResponseStatus: in.ResponseStatus,
		CreatedAt:      s.nowUTC(),
	}
	if r.ResponseStatus == 0 {
		r.ResponseStatus = DefaultTestResponseStatus
	}
	var out TestEndpoint
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		e, secret, err := s.insertEndpoint(ctx, tx, accountID, NewEndpoint{
			URL:         strings.TrimSuffix(in.BaseURL, "/") + TestReceiverPath(r.ID),
			Description: in.Description,
			EventTypes:  in.EventTypes,
		})
		if err != nil {
			return err
		}
		r.EndpointID = e.ID
		if _, err := s.exec(ctx, tx,
			`INSERT INTO test_receivers (id, account_id, endpoint_id, response_status, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			r.ID, r.AccountID, r.EndpointID, r.ResponseStatus, s.d.timeArg(r.CreatedAt)); err != nil {
			return fmt.Errorf("store: create test endpoint: %w", err)
		}
		out = TestEndpoint{Endpoint: e, Secret: secret, Receiver: r}
		return nil
	})
	if err != nil {
		return TestEndpoint{}, err
	}
	return out, nil
}

// GetTestReceiver implements Store.
func (s *SQLStore) GetTestReceiver(ctx context.Context, id string) (TestReceiverDetail, error) {
	var (
		d       TestReceiverDetail
		deleted dbTime
	)
	row := s.queryRow(ctx, s.db,
		`SELECT `+testReceiverColumns+`, e.secret, e.deleted_at
		FROM test_receivers r JOIN endpoints e ON e.id = r.endpoint_id
		WHERE r.id = ?`, id)
	r, err := scanTestReceiver(row, &d.Secret, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return TestReceiverDetail{}, ErrNotFound
	}
	if err != nil {
		return TestReceiverDetail{}, fmt.Errorf("store: get test receiver: %w", err)
	}
	d.Receiver = r
	d.EndpointDeleted = deleted.Valid

	row = s.queryRow(ctx, s.db, `SELECT `+endpointColumns+` FROM endpoints WHERE id = ?`, r.EndpointID)
	if d.Endpoint, err = scanEndpoint(row); err != nil {
		return TestReceiverDetail{}, fmt.Errorf("store: get test receiver: endpoint: %w", err)
	}
	return d, nil
}

// ListTestReceivers implements Store.
func (s *SQLStore) ListTestReceivers(ctx context.Context, accountID string) ([]TestReceiver, error) {
	rows, err := s.query(ctx, s.db,
		`SELECT `+testReceiverColumns+`
		FROM test_receivers r JOIN endpoints e ON e.id = r.endpoint_id
		WHERE r.account_id = ? AND e.deleted_at IS NULL
		ORDER BY r.id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("store: list test receivers: %w", err)
	}
	defer rows.Close()
	list := []TestReceiver{}
	for rows.Next() {
		r, err := scanTestReceiver(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list test receivers: %w", err)
		}
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list test receivers: %w", err)
	}
	return list, nil
}

// SetTestReceiverStatus implements Store.
func (s *SQLStore) SetTestReceiverStatus(ctx context.Context, accountID, endpointID string, status int) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		id, err := s.testReceiverOf(ctx, tx, accountID, endpointID)
		if err != nil {
			return err
		}
		if _, err := s.exec(ctx, tx,
			`UPDATE test_receivers SET response_status = ? WHERE id = ?`, status, id); err != nil {
			return fmt.Errorf("store: set test receiver status: %w", err)
		}
		return nil
	})
}

// RecordTestRequest implements Store.
func (s *SQLStore) RecordTestRequest(ctx context.Context, receiverID string, in NewTestRequest) (TestRequest, error) {
	req := TestRequest{
		ID:             ids.NewTestRequestID(),
		ReceiverID:     receiverID,
		ReceivedAt:     s.nowUTC(),
		WebhookID:      storedText(in.WebhookID),
		Headers:        map[string][]string{},
		Body:           []byte(storedText(string(in.Body))),
		Verified:       in.Verified,
		VerifyError:    storedText(in.VerifyError),
		ResponseStatus: in.ResponseStatus,
	}
	for name, values := range in.Headers {
		vs := make([]string, len(values))
		for i, v := range values {
			vs[i] = storedText(v)
		}
		req.Headers[storedText(name)] = vs
	}
	headers, err := json.Marshal(req.Headers)
	if err != nil {
		return TestRequest{}, fmt.Errorf("store: record test request: %w", err)
	}

	err = s.inTx(ctx, func(tx *sql.Tx) error {
		// Locking the endpoint makes a concurrent DeleteEndpoint wait, or
		// be waited for, so no request is left behind once it is deleted.
		// It also keeps two recordings for one receiver from trimming at
		// the same time.
		var accountID string
		err := s.queryRow(ctx, tx,
			`SELECT e.account_id FROM endpoints e
			WHERE e.id = (SELECT endpoint_id FROM test_receivers WHERE id = ?)
			AND e.deleted_at IS NULL`+s.d.lockRows,
			receiverID).Scan(&accountID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: record test request: %w", err)
		}

		if req.WebhookID != "" {
			err := s.queryRow(ctx, tx,
				`SELECT event_type FROM events WHERE account_id = ? AND id = ?`,
				accountID, req.WebhookID).Scan(&req.EventType)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: record test request: event type: %w", err)
			}
		}

		if _, err := s.exec(ctx, tx,
			`INSERT INTO test_requests (`+testRequestColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			req.ID, req.ReceiverID, s.d.timeArg(req.ReceivedAt), req.WebhookID, req.EventType,
			string(headers), string(req.Body), req.Verified, req.VerifyError, req.ResponseStatus); err != nil {
			return fmt.Errorf("store: record test request: %w", err)
		}
		if _, err := s.exec(ctx, tx,
			`DELETE FROM test_requests
			WHERE receiver_id = ? AND id NOT IN (
				SELECT id FROM test_requests WHERE receiver_id = ? ORDER BY id DESC LIMIT ?
			)`,
			receiverID, receiverID, MaxTestRequests); err != nil {
			return fmt.Errorf("store: record test request: trim: %w", err)
		}
		return nil
	})
	if err != nil {
		return TestRequest{}, err
	}
	return req, nil
}

// ListTestRequests implements Store. IDs are ULIDs, so ordering by ID is
// ordering by the time they were received.
func (s *SQLStore) ListTestRequests(ctx context.Context, accountID, endpointID string, limit int) ([]TestRequest, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: list test requests: limit %d is not positive", limit)
	}
	id, err := s.testReceiverOf(ctx, s.db, accountID, endpointID)
	if err != nil {
		return nil, err
	}
	rows, err := s.query(ctx, s.db,
		`SELECT `+testRequestColumns+` FROM test_requests
		WHERE receiver_id = ?
		ORDER BY id DESC
		LIMIT ?`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list test requests: %w", err)
	}
	defer rows.Close()
	list := []TestRequest{}
	for rows.Next() {
		req, err := scanTestRequest(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list test requests: %w", err)
		}
		list = append(list, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list test requests: %w", err)
	}
	return list, nil
}

// ClearTestRequests implements Store.
func (s *SQLStore) ClearTestRequests(ctx context.Context, accountID, endpointID string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		id, err := s.testReceiverOf(ctx, tx, accountID, endpointID)
		if err != nil {
			return err
		}
		if _, err := s.exec(ctx, tx, `DELETE FROM test_requests WHERE receiver_id = ?`, id); err != nil {
			return fmt.Errorf("store: clear test requests: %w", err)
		}
		return nil
	})
}

// testReceiverOf returns the ID of the receiver of one of the account's test
// endpoints, or ErrNotFound if the endpoint does not exist, belongs to
// another account, has been deleted or is not a test endpoint.
func (s *SQLStore) testReceiverOf(ctx context.Context, q queryer, accountID, endpointID string) (string, error) {
	var id string
	err := s.queryRow(ctx, q,
		`SELECT r.id FROM test_receivers r JOIN endpoints e ON e.id = r.endpoint_id
		WHERE r.account_id = ? AND r.endpoint_id = ? AND e.deleted_at IS NULL`,
		accountID, endpointID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: find test receiver: %w", err)
	}
	return id, nil
}

// scanTestReceiver reads one row of testReceiverColumns followed by extra.
func scanTestReceiver(sc scanner, extra ...any) (TestReceiver, error) {
	var (
		r       TestReceiver
		created dbTime
	)
	dest := append([]any{&r.ID, &r.AccountID, &r.EndpointID, &r.ResponseStatus, &created}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return TestReceiver{}, err
	}
	r.CreatedAt = created.Time
	return r, nil
}

// scanTestRequest reads one row of testRequestColumns.
func scanTestRequest(sc scanner) (TestRequest, error) {
	var (
		req           TestRequest
		received      dbTime
		headers, body string
	)
	if err := sc.Scan(&req.ID, &req.ReceiverID, &received, &req.WebhookID, &req.EventType,
		&headers, &body, &req.Verified, &req.VerifyError, &req.ResponseStatus); err != nil {
		return TestRequest{}, err
	}
	if err := json.Unmarshal([]byte(headers), &req.Headers); err != nil {
		return TestRequest{}, fmt.Errorf("decode headers of %s: %w", req.ID, err)
	}
	if req.Headers == nil {
		req.Headers = map[string][]string{}
	}
	req.ReceivedAt = received.Time
	req.Body = []byte(body)
	return req, nil
}

// storedText returns s as it can be kept in a text column of either
// database: invalid UTF-8 and NUL bytes become U+FFFD.
func storedText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "�")
}
