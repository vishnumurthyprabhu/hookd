-- Tables for the admin UI: settings saved in the UI, and the built-in test
-- receiver with the requests it received.

CREATE TABLE settings (
    name       TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE test_receivers (
    id              TEXT PRIMARY KEY,
    account_id      TEXT NOT NULL REFERENCES accounts (id),
    endpoint_id     TEXT NOT NULL UNIQUE REFERENCES endpoints (id),
    response_status INTEGER NOT NULL DEFAULT 200,
    created_at      TEXT NOT NULL
);

CREATE INDEX test_receivers_account_id ON test_receivers (account_id, id);

CREATE TABLE test_requests (
    id              TEXT PRIMARY KEY,
    receiver_id     TEXT NOT NULL REFERENCES test_receivers (id),
    received_at     TEXT NOT NULL,
    webhook_id      TEXT NOT NULL DEFAULT '',
    event_type      TEXT NOT NULL DEFAULT '',
    headers         TEXT NOT NULL DEFAULT '{}',
    body            TEXT NOT NULL DEFAULT '',
    verified        INTEGER NOT NULL DEFAULT 0,
    verify_error    TEXT NOT NULL DEFAULT '',
    response_status INTEGER NOT NULL
);

CREATE INDEX test_requests_receiver_id ON test_requests (receiver_id, id);

CREATE INDEX events_account_event_type ON events (account_id, event_type, id);
