# Configuration

Hookd reads all its settings from environment variables. Every setting has a default that works locally, so `hookd serve` runs with none set. A variable that is unset or empty keeps its default.

Hookd checks every setting when it starts. If any is invalid it names each bad one and exits without doing anything:

```
hookd: invalid configuration:
  HOOKD_WORKERS="0": must be a whole number greater than 0
```

The settings apply to every command (`serve`, `migrate`, `account create`, `apikey create`, `apikey list`, `apikey revoke`), so the operator commands must be run with the same `HOOKD_DB_*` settings as `serve`.

## Settings

| Variable | What it does | Default |
|---|---|---|
| `HOOKD_LISTEN_ADDR` | Address the API listens on, as `host:port`. `:8080` listens on every interface. | `:8080` |
| `HOOKD_DB_DRIVER` | Database to use: `sqlite` or `postgres`. | `sqlite` |
| `HOOKD_DB_PATH` | SQLite file path. `:memory:` keeps everything in memory and loses it on exit. SQLite only. | `./hookd.db` |
| `HOOKD_DB_HOST` | PostgreSQL host name or IP address. | `localhost` |
| `HOOKD_DB_PORT` | PostgreSQL port, 1 to 65535. | `5432` |
| `HOOKD_DB_NAME` | PostgreSQL database name. | `hookd` |
| `HOOKD_DB_USER` | PostgreSQL user. | `hookd` |
| `HOOKD_DB_PASSWORD` | PostgreSQL password. | empty |
| `HOOKD_DB_SSLMODE` | PostgreSQL `sslmode`: `disable`, `allow`, `prefer`, `require`, `verify-ca` or `verify-full`. Use `verify-full` in production; see [Deploy to production](deployment.md#4-set-up-tls-to-the-database). | `disable` |
| `HOOKD_DB_MAX_CONNS` | Largest number of open database connections, for each Hookd process. Applies to both drivers. | `10` |
| `HOOKD_WORKERS` | Number of delivery workers, so the most deliveries one process sends at once. | `8` |
| `HOOKD_POLL_INTERVAL` | How often idle workers look for work. New events wake the workers at once; polling picks up work left after a restart or by another instance. | `1s` |
| `HOOKD_DELIVERY_TIMEOUT` | How long an endpoint has to answer one delivery. A slower answer counts as a failure. | `15s` |
| `HOOKD_DELIVERY_LEASE` | How long a claimed delivery stays locked to its worker. If the process dies while sending, another worker sends the delivery again once the lease has run out. Must be longer than `HOOKD_DELIVERY_TIMEOUT`; Hookd refuses to start otherwise. | `60s` |
| `HOOKD_MAX_PAYLOAD_BYTES` | Largest event payload accepted, in bytes. Larger events are rejected with 413. | `262144` (256 KB) |
| `HOOKD_IDEMPOTENCY_WINDOW` | How long an `Idempotency-Key` is remembered. Sending the same key again within this time returns the original event. | `24h` |
| `HOOKD_AUTO_MIGRATE` | Apply database migrations when `serve` starts: `true` or `false`. | `true` |
| `HOOKD_LOG_LEVEL` | Lowest level logged: `debug`, `info`, `warn` or `error`. | `info` |
| `HOOKD_LOG_FORMAT` | Log format: `text` or `json`. | `text` |

Durations are Go durations, such as `500ms`, `15s`, `5m` or `24h`, and must be greater than zero. Choices such as `postgres` or `json` may be written in any case. Booleans accept `true`, `false`, `1` and `0`.

## Logging

Hookd logs to standard error with Go's `log/slog`: one line per API request, one per delivery, and one per startup step. API keys, signing secrets and `Authorization` headers are never logged. Use `HOOKD_LOG_FORMAT=json` when the logs go to a log collector.

## The test receiver

`cmd/testreceiver` takes flags instead of environment variables:

| Flag | What it does | Default |
|---|---|---|
| `--secret` | Endpoint signing secret, `whsec_...`. Required. | none |
| `--addr` | Address to listen on. | `127.0.0.1:9000` |
| `--status` | HTTP status to answer every request with. | `200` |
