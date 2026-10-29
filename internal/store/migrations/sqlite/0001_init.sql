-- Timestamps are fixed-width UTC text (2006-01-02T15:04:05.000000Z), so they
-- compare in time order.

CREATE TABLE accounts (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE TABLE api_keys (
    id         TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts (id),
    key_hash   TEXT NOT NULL UNIQUE,
    key_prefix TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX api_keys_account_id ON api_keys (account_id);

CREATE TABLE endpoints (
    id          TEXT PRIMARY KEY,
    account_id  TEXT NOT NULL REFERENCES accounts (id),
    url         TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    event_types TEXT NOT NULL DEFAULT '[]',
    enabled     INTEGER NOT NULL DEFAULT 1,
    secret      TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    deleted_at  TEXT
);

CREATE INDEX endpoints_account_id ON endpoints (account_id, id);

CREATE TABLE events (
    id              TEXT PRIMARY KEY,
    account_id      TEXT NOT NULL REFERENCES accounts (id),
    event_type      TEXT NOT NULL,
    payload         TEXT NOT NULL,
    idempotency_key TEXT,
    created_at      TEXT NOT NULL
);

CREATE INDEX events_account_id ON events (account_id, id);

CREATE UNIQUE INDEX events_account_idempotency_key
    ON events (account_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE deliveries (
    id              TEXT PRIMARY KEY,
    event_id        TEXT NOT NULL REFERENCES events (id),
    endpoint_id     TEXT NOT NULL REFERENCES endpoints (id),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'in_progress', 'succeeded', 'failed')),
    response_status INTEGER,
    error           TEXT,
    latency_ms      INTEGER,
    attempted_at    TEXT,
    locked_until    TEXT,
    created_at      TEXT NOT NULL,
    UNIQUE (event_id, endpoint_id)
);

CREATE INDEX deliveries_status_locked_until ON deliveries (status, locked_until);
