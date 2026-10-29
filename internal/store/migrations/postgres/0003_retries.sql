-- Retries and the delivery history: the retry columns on deliveries, one row
-- per attempt, and an index for the retention delete.

ALTER TABLE deliveries ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deliveries ADD COLUMN next_attempt_at TIMESTAMPTZ;

CREATE TABLE attempts (
    id              TEXT PRIMARY KEY,
    account_id      TEXT NOT NULL REFERENCES accounts (id),
    delivery_id     TEXT NOT NULL REFERENCES deliveries (id),
    event_id        TEXT NOT NULL REFERENCES events (id),
    endpoint_id     TEXT NOT NULL REFERENCES endpoints (id),
    number          INTEGER NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    outcome         TEXT NOT NULL CHECK (outcome IN ('succeeded', 'failed')),
    response_status INTEGER,
    latency_ms      INTEGER NOT NULL,
    error           TEXT,
    UNIQUE (delivery_id, number)
);

CREATE INDEX attempts_event_id ON attempts (event_id, id);
CREATE INDEX attempts_endpoint_id ON attempts (endpoint_id, id);
CREATE INDEX attempts_endpoint_id_outcome ON attempts (endpoint_id, outcome, id);

CREATE INDEX deliveries_status_next_attempt_at ON deliveries (status, next_attempt_at);
CREATE INDEX deliveries_endpoint_id_status ON deliveries (endpoint_id, status);
CREATE INDEX events_created_at ON events (created_at);

-- Existing data: every delivery is due from its creation, and each finished
-- one gets its single attempt, under the delivery's ULID.
UPDATE deliveries SET next_attempt_at = created_at;
ALTER TABLE deliveries ALTER COLUMN next_attempt_at SET NOT NULL;

INSERT INTO attempts (id, account_id, delivery_id, event_id, endpoint_id, number,
                      started_at, outcome, response_status, latency_ms, error)
SELECT 'att_' || substr(d.id, 5), e.account_id, d.id, d.event_id, d.endpoint_id, 1,
       d.attempted_at, d.status, d.response_status, COALESCE(d.latency_ms, 0), d.error
FROM deliveries d JOIN events e ON e.id = d.event_id
WHERE d.status IN ('succeeded', 'failed') AND d.attempted_at IS NOT NULL;

UPDATE deliveries SET attempts = 1
WHERE status IN ('succeeded', 'failed') AND attempted_at IS NOT NULL;
