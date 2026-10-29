<!-- Synced by Software Factory from commit a2e6af58e55011b0d8aa5fd8623257669503f524, 2026-10-29T07:24:51.888Z. Do not edit this line. -->
# Hookd documentation

## Prerequisites

- Go 1.26 or later (<https://go.dev/dl/>). Check it with `go version`.
- `git` and `curl`.
- Nothing else locally: the SQLite driver is pure Go and the database is a local file.
- For production: Linux hosts, PostgreSQL 13 or later on its own node, and a load balancer or reverse proxy that terminates TLS.

## Quick local start

Use three terminals, all in the repository folder.

1. Get the code:

   ```bash
   git clone <repository-url> hookd
   cd hookd
   ```

2. Start Hookd (terminal 1). It creates `./hookd.db`, applies its migrations and listens on port 8080:

   ```bash
   go run ./cmd/hookd serve
   ```

   Check it from terminal 2:

   ```bash
   curl -s http://localhost:8080/healthz
   ```

   ```json
   {"status":"ok"}
   ```

3. Create an account (terminal 2). Hookd prints an `account_id` and an `api_key`, and shows the key only once:

   ```bash
   go run ./cmd/hookd account create --name acme
   export HOOKD_API_KEY=hk_...
   ```

4. Register an endpoint that points at the test receiver. The answer includes the endpoint's `secret` (`whsec_...`):

   ```bash
   curl -s -X POST http://localhost:8080/v1/endpoints \
     -H "Authorization: Bearer $HOOKD_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"url": "http://127.0.0.1:9000", "description": "local test receiver", "event_types": ["order.paid"]}'
   ```

5. Start the test receiver with that secret (terminal 3). It listens on `127.0.0.1:9000` and logs whether each signature verified:

   ```bash
   go run ./cmd/testreceiver --secret whsec_...
   ```

   Flags: `--addr` (default `127.0.0.1:9000`) and `--status` (default `200`). For example, `--status 500` makes it answer with a failure.

6. Send an event (terminal 2). Hookd answers `202` straight away, and terminal 3 logs `verified=true`:

   ```bash
   curl -s -X POST http://localhost:8080/v1/events \
     -H "Authorization: Bearer $HOOKD_API_KEY" \
     -H "Content-Type: application/json" \
     -H "Idempotency-Key: order-1001-paid" \
     -d '{"event_type": "order.paid", "payload": {"type": "order.paid", "order_id": "1001", "amount": 4200}}'
   ```

7. Fetch the event by its `msg_...` ID. The delivery now shows `"status":"succeeded"`:

   ```bash
   curl -s http://localhost:8080/v1/events/msg_... \
     -H "Authorization: Bearer $HOOKD_API_KEY"
   ```

To start again with an empty database, stop Hookd and delete `hookd.db`.

To run Hookd locally on PostgreSQL instead, start a throwaway container and point Hookd at it. The other `HOOKD_DB_*` defaults match the container. Run `account create` with the same two settings.

```bash
docker run --rm -d --name hookd-pg -p 5432:5432 \
  -e POSTGRES_USER=hookd -e POSTGRES_PASSWORD=hookd -e POSTGRES_DB=hookd \
  postgres:17
HOOKD_DB_DRIVER=postgres HOOKD_DB_PASSWORD=hookd go run ./cmd/hookd serve
```

### Tests

```bash
go test ./...                                # all tests, on SQLite
go test -count=1 ./...                       # ignore cached results
go test -v -run TestAcceptance ./cmd/hookd   # one test, with its output
go vet ./...                                 # static checks
```

The store suite can also run on PostgreSQL:

```bash
HOOKD_TEST_PG=1 \
HOOKD_DB_HOST=localhost HOOKD_DB_PORT=5432 \
HOOKD_DB_NAME=hookd HOOKD_DB_USER=hookd HOOKD_DB_PASSWORD=hookd \
HOOKD_DB_SSLMODE=disable \
go test -count=1 -v ./internal/store/ 2>&1 | grep -- '--- .*/postgres'
```

## Production deployment

In production, one or more identical `hookd serve` processes run behind a load balancer and share one PostgreSQL database. Don't run more than one instance on SQLite.

1. **Build** one static binary with the migrations built in, then copy it to each host, for example to `/usr/local/bin/hookd`. For ARM hosts, use `GOARCH=arm64`.

   ```bash
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o hookd ./cmd/hookd
   hookd help
   ```

2. **Prepare PostgreSQL** as a superuser (`sudo -u postgres psql`):

   ```sql
   SET password_encryption = 'scram-sha-256';
   CREATE USER hookd WITH PASSWORD 'change-me';
   CREATE DATABASE hookd OWNER hookd;
   REVOKE ALL ON DATABASE hookd FROM PUBLIC;
   GRANT CONNECT, CREATE, TEMPORARY ON DATABASE hookd TO hookd;
   \c hookd
   GRANT USAGE, CREATE ON SCHEMA public TO hookd;
   \q
   ```

3. **Allow the application hosts to connect over TLS.**
   - In `postgresql.conf`, set `listen_addresses = '*'` (or the node's private address), and also `ssl = on`, `ssl_cert_file` and `ssl_key_file`.
   - In `pg_hba.conf`, add a line such as `hostssl hookd hookd 10.0.1.0/24 scram-sha-256`.
   - Restart with `sudo systemctl restart postgresql`.
   - Make sure `max_connections` covers `HOOKD_DB_MAX_CONNS` (10 by default) multiplied by the number of instances.
   - If your own CA signed the database certificate, set `PGSSLROOTCERT=/etc/hookd/db-ca.crt`.

4. **Settings.** Put them in `/etc/hookd/hookd.env` and run `sudo chmod 600 /etc/hookd/hookd.env`:

   ```bash
   HOOKD_DB_DRIVER=postgres
   HOOKD_DB_HOST=db.internal.example.com
   HOOKD_DB_PORT=5432
   HOOKD_DB_NAME=hookd
   HOOKD_DB_USER=hookd
   HOOKD_DB_PASSWORD=change-me
   HOOKD_DB_SSLMODE=verify-full
   HOOKD_DB_MAX_CONNS=10
   HOOKD_AUTO_MIGRATE=false
   HOOKD_LOG_FORMAT=json
   ```

   To load the settings into a shell before running `hookd` commands, use `set -a; . /etc/hookd/hookd.env; set +a`.

5. **Migrate** from one host, at every deploy, before starting the new version:

   ```bash
   hookd migrate
   ```

6. **Run** `hookd serve` on each host under systemd, with the unit file `/etc/systemd/system/hookd.service`:

   ```ini
   [Unit]
   Description=Hookd webhook delivery
   After=network-online.target
   Wants=network-online.target

   [Service]
   EnvironmentFile=/etc/hookd/hookd.env
   ExecStart=/usr/local/bin/hookd serve
   Restart=always
   RestartSec=2
   KillSignal=SIGTERM
   TimeoutStopSec=30
   DynamicUser=yes
   NoNewPrivileges=yes

   [Install]
   WantedBy=multi-user.target
   ```

   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable --now hookd
   sudo journalctl -u hookd -f
   ```

7. **Load balancer.** Hookd serves plain HTTP, so terminate TLS in front of it. Sticky sessions aren't needed. Keep the proxy's body size limit above `HOOKD_MAX_PAYLOAD_BYTES` plus a few KB (for nginx, `client_max_body_size 1m;`). Point the health check at `GET /healthz`, which answers `200` when the database is reachable and `503` when it isn't.

8. **Upgrade:**
   1. Build the new binary and copy it to each host.
   2. Run `hookd migrate` once.
   3. Restart the instances one at a time with `sudo systemctl restart hookd`, checking `/healthz` after each.

9. **Back up** the PostgreSQL database, for example with `pg_dump`. It holds all of Hookd's state, signing secrets included, so protect the backups as secrets.

The full guide, with an nginx example and a `sslmode` table, is in `docs/deployment.md`.

### Settings

All settings are environment variables. Each one has a default that works locally, and the operator commands need the same `HOOKD_DB_*` values as `serve`.

| Variable | Default |
|---|---|
| `HOOKD_LISTEN_ADDR` | `:8080` |
| `HOOKD_DB_DRIVER` (`sqlite` or `postgres`) | `sqlite` |
| `HOOKD_DB_PATH` (SQLite only, `:memory:` allowed) | `./hookd.db` |
| `HOOKD_DB_HOST` / `HOOKD_DB_PORT` | `localhost` / `5432` |
| `HOOKD_DB_NAME` / `HOOKD_DB_USER` / `HOOKD_DB_PASSWORD` | `hookd` / `hookd` / empty |
| `HOOKD_DB_SSLMODE` | `disable` |
| `HOOKD_DB_MAX_CONNS` | `10` |
| `HOOKD_WORKERS` | `8` |
| `HOOKD_POLL_INTERVAL` | `1s` |
| `HOOKD_DELIVERY_TIMEOUT` | `15s` |
| `HOOKD_DELIVERY_LEASE` (must be longer than the timeout) | `60s` |
| `HOOKD_MAX_PAYLOAD_BYTES` | `262144` |
| `HOOKD_IDEMPOTENCY_WINDOW` | `24h` |
| `HOOKD_AUTO_MIGRATE` | `true` |
| `HOOKD_LOG_LEVEL` (`debug`, `info`, `warn`, `error`) | `info` |
| `HOOKD_LOG_FORMAT` (`text` or `json`) | `text` |

If a setting is invalid, Hookd names it and exits without doing anything. `docs/configuration.md` describes each setting.

## How to use it

### Operator commands

```
hookd serve                        Run the API server and delivery workers
hookd migrate                      Apply database migrations
hookd account create --name NAME   Create an account and print its API key
hookd apikey create --account ID   Create another API key for an account and print it
hookd apikey list --account ID     List an account's API keys
hookd apikey revoke --id KEY_ID    Revoke an API key so it no longer works
```

Hookd stores only a hash of each key, so a lost key can't be shown again. To rotate a key:

1. Create a new key with `apikey create`.
2. Give it to the customer.
3. Once they have switched, find the old key's ID with `apikey list` and revoke it with `apikey revoke`.

A revoked key gets `401`.

### API

Send `Authorization: Bearer hk_...` on every `/v1` route. An object that belongs to another account answers `404`.

| Method & path | What it does |
|---|---|
| `POST /v1/endpoints` | Create an endpoint (`url`, `description`, `event_types`). The answer includes its `secret`. |
| `GET /v1/endpoints?limit=&after=` | List endpoints, oldest first. `limit` is 1 to 100, default 50. To get the next page, pass the last `id` as `after`. |
| `GET /v1/endpoints/{id}` | Get one endpoint, without its secret. |
| `PATCH /v1/endpoints/{id}` | Change `url`, `description`, `event_types` or `enabled`. |
| `DELETE /v1/endpoints/{id}` | Delete an endpoint (`204`). |
| `GET /v1/endpoints/{id}/secret` | Get the signing secret. |
| `POST /v1/events` | Send `event_type` and a JSON object `payload`, plus an optional `Idempotency-Key` header. Answers `202`, or `200` with `Idempotent-Replayed: true` for a key repeated within the window. |
| `GET /v1/events/{id}` | Get the event and each delivery's `status`, `response_status` and `error`. |
| `GET /healthz` | Service and database check. Needs no key. |

Rules:

- An empty `event_types` list subscribes the endpoint to every type.
- Event types are 1 to 128 letters, digits, `.`, `_` or `-`.
- Changes to an endpoint apply to events sent afterwards.
- A pending delivery to an endpoint that has since been disabled or deleted fails with `endpoint_disabled`.
- A delivery succeeds only on a 2xx answer within the timeout. A 3xx answer counts as a failure, because redirects aren't followed.

Errors look like `{"error":{"code":"...","message":"..."}}`. The codes are `invalid_json`, `invalid_request`, `invalid_url`, `invalid_event_type`, `invalid_payload`, `invalid_idempotency_key`, `unauthorized`, `not_found`, `method_not_allowed`, `payload_too_large`, `internal_error` and `unavailable`.

### Receiving deliveries

Each delivery is a `POST` of the payload bytes, exactly as they were sent. It carries these headers:

- `Content-Type: application/json`
- `webhook-id` (the event ID)
- `webhook-timestamp`
- `webhook-signature` (`v1,<base64 HMAC-SHA256>`)

Verify each delivery against the raw body using any Standard Webhooks library and the endpoint's secret. Delivery is at least once: a delivery cut off by a crash is sent again with the same `webhook-id`, so drop repeats by that header. `docs/api.md` has complete receivers in Node.js (`npm install standardwebhooks`) and Go.
