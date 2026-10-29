# Configuration

Hookd reads its settings from environment variables. Every setting has a default that works locally, so `hookd serve` runs with none set. A variable that is unset or empty keeps its default. Eight of the settings can also be changed in the admin UI while Hookd runs; see [Settings changed in the admin UI](#settings-changed-in-the-admin-ui).

Hookd checks every setting when it starts. If any is invalid it names each bad one and exits without doing anything:

```
hookd: invalid configuration:
  HOOKD_WORKERS="0": must be a whole number greater than 0
```

The settings apply to every command (`serve`, `migrate`, `account create`, `apikey create`, `apikey list`, `apikey revoke`), so the operator commands must be run with the same `HOOKD_DB_*` settings as `serve`. Only `serve` uses the values saved in the admin UI; the other commands read the environment alone.

## Settings

| Variable | What it does | Default | In the UI |
|---|---|---|---|
| `HOOKD_LISTEN_ADDR` | Address the API and the admin UI listen on, as `host:port`. `:8080` listens on every interface. | `:8080` | read-only |
| `HOOKD_DB_DRIVER` | Database to use: `sqlite` or `postgres`. | `sqlite` | read-only |
| `HOOKD_DB_PATH` | SQLite file path. `:memory:` keeps everything in memory and loses it on exit. SQLite only. | `./hookd.db` | read-only |
| `HOOKD_DB_HOST` | PostgreSQL host name or IP address. | `localhost` | read-only |
| `HOOKD_DB_PORT` | PostgreSQL port, 1 to 65535. | `5432` | read-only |
| `HOOKD_DB_NAME` | PostgreSQL database name. | `hookd` | read-only |
| `HOOKD_DB_USER` | PostgreSQL user. | `hookd` | read-only |
| `HOOKD_DB_PASSWORD` | PostgreSQL password. The UI shows only whether it is set. | empty | read-only |
| `HOOKD_DB_SSLMODE` | PostgreSQL `sslmode`: `disable`, `allow`, `prefer`, `require`, `verify-ca` or `verify-full`. Use `verify-full` in production; see [Deploy to production](deployment.md#4-set-up-tls-to-the-database). | `disable` | read-only |
| `HOOKD_DB_MAX_CONNS` | Largest number of open database connections, for each Hookd process. Applies to both drivers. | `10` | read-only |
| `HOOKD_WORKERS` | Number of delivery workers, so the most deliveries one process sends at once. | `8` | editable |
| `HOOKD_POLL_INTERVAL` | How often idle workers look for work. New events wake the workers at once; polling picks up work left after a restart or by another instance. | `1s` | editable |
| `HOOKD_DELIVERY_TIMEOUT` | How long an endpoint has to answer one delivery. A slower answer counts as a failure. | `15s` | editable |
| `HOOKD_DELIVERY_LEASE` | How long a claimed delivery stays locked to its worker. If the process dies while sending, another worker sends the delivery again once the lease has run out. Must be longer than `HOOKD_DELIVERY_TIMEOUT`; Hookd refuses to start otherwise, and the UI refuses such a value. | `60s` | editable |
| `HOOKD_MAX_PAYLOAD_BYTES` | Largest event payload accepted, in bytes. Larger events are rejected with 413. | `262144` (256 KB) | editable |
| `HOOKD_IDEMPOTENCY_WINDOW` | How long an `Idempotency-Key` is remembered. Sending the same key again within this time returns the original event. | `24h` | editable |
| `HOOKD_AUTO_MIGRATE` | Apply database migrations when `serve` starts: `true` or `false`. | `true` | read-only |
| `HOOKD_LOG_LEVEL` | Lowest level logged: `debug`, `info`, `warn` or `error`. | `info` | editable |
| `HOOKD_LOG_FORMAT` | Log format: `text` or `json`. | `text` | read-only |
| `HOOKD_ADMIN_UI` | Serve the admin UI and the built-in test receiver under `/admin`: `true` or `false`. When `false`, `/` and everything under `/admin` answer 404; the API and `/healthz` are not affected. The UI has no sign-in; see [Deploy to production](deployment.md#keep-the-admin-ui-private). | `true` | read-only |
| `HOOKD_RETRY_SCHEDULE` | The waits before each retry of a failed delivery, counted from the end of the previous attempt, as comma-separated durations: the first wait comes after attempt 1 fails, the second after attempt 2, and so on. A delivery gets one attempt more than there are waits; when the last one fails, the delivery is `failed`. At most 20 waits, each greater than zero. `none` means no retries: one attempt per delivery. Each wait is varied at random by up to 10% either way. See [Retries](#retries). | `5s,5m,30m,2h,5h,10h,10h` (8 attempts over about 27½ hours) | editable |
| `HOOKD_RETENTION` | How long events are kept after they were accepted. Older events are deleted with their deliveries and attempts, in the background, about once an hour. Must be longer than `HOOKD_IDEMPOTENCY_WINDOW` and longer than the total of `HOOKD_RETRY_SCHEDULE` plus 10%. See [Retention](#retention). | `720h` (30 days) | read-only |
| `HOOKD_ADMIN_RECEIVER_URL` | Base URL that the delivery workers use to reach the built-in test receiver, such as `http://10.0.1.11:8080`. It must be an absolute `http` or `https` URL. It is put in the URL of each new test endpoint, as `<base URL>/admin/test-receiver/trcv_...`, and changing it later does not change existing test endpoints. When empty, it is `http://127.0.0.1:<port>` if `HOOKD_LISTEN_ADDR` listens on every interface (`:8080`, `0.0.0.0:8080`, `[::]:8080`), and `http://<host>:<port>` if it names a host. | empty (derived, `http://127.0.0.1:8080` by default) | read-only |

Durations are Go durations, such as `500ms`, `15s`, `5m` or `24h`, and must be greater than zero. Choices such as `postgres` or `json` may be written in any case. Booleans accept `true`, `false`, `1` and `0`.

## Settings changed in the admin UI

The **Settings** page of the admin UI (`/admin/settings`) lists every setting above with its current value and its source:

- `default`: neither the environment nor the UI sets it;
- `environment`: the `HOOKD_*` variable sets it;
- `UI`: a value saved in the admin UI sets it.

These eight can be changed there, each with a **Save** button:

- `HOOKD_WORKERS`
- `HOOKD_POLL_INTERVAL`
- `HOOKD_DELIVERY_TIMEOUT`
- `HOOKD_DELIVERY_LEASE`
- `HOOKD_MAX_PAYLOAD_BYTES`
- `HOOKD_IDEMPOTENCY_WINDOW`
- `HOOKD_LOG_LEVEL`
- `HOOKD_RETRY_SCHEDULE`

How it works:

- **A UI value overrides the environment.** Saved values are stored in the database (the `settings` table), so they survive a restart and apply to every instance that shares the database. While a setting has a UI value, changing its environment variable has no effect.
- **Values are checked as at start-up.** The whole configuration is checked with the new value in place, including that `HOOKD_DELIVERY_LEASE` is longer than `HOOKD_DELIVERY_TIMEOUT`, and that `HOOKD_RETENTION` stays longer than `HOOKD_IDEMPOTENCY_WINDOW` and than the retry schedule's total plus 10%. An invalid value is refused with the reason, and nothing is saved. To make the timeout longer than the current lease, raise the lease first.
- **No restart is needed.** A change applies at once on the instance where it was saved, and every other instance picks it up within 5 seconds. A change of `HOOKD_WORKERS` starts or stops workers; a stopped worker finishes the delivery it is sending first. A new timeout, lease or payload limit applies to the next delivery or event. A new retry schedule applies to the next wait that is scheduled; retries already waiting keep their time.
- **Reset.** A setting whose source is `UI` has a **Reset** button. It deletes the saved value, so the setting goes back to its environment variable, or to the default if that is unset. Like a save, a reset is refused if the configuration without the saved value would be invalid (for example, resetting a UI lease of `120s` while a UI timeout of `90s` is still saved); reset or change the other setting first.
- **Instances with different environments.** If the saved values together with one instance's environment are invalid (for example, a UI timeout of `90s` and an instance whose environment sets `HOOKD_DELIVERY_LEASE=60s`), that instance keeps its last valid configuration, logs an error, and its Settings page shows the problem until it is fixed.

Every other setting is environment-only. The UI shows it read-only, marked "set with environment variables": the database settings and the listen address are needed before Hookd can read its database, `HOOKD_AUTO_MIGRATE`, `HOOKD_LOG_FORMAT`, `HOOKD_ADMIN_UI` and `HOOKD_ADMIN_RECEIVER_URL` apply only at start-up, and `HOOKD_RETENTION` is kept out of the UI on purpose, because shortening it by mistake deletes data. To change one, change the environment variable and restart Hookd.

## Retries

A delivery fails when the endpoint answers with any status other than 2xx (3xx included: redirects are not followed), does not answer within `HOOKD_DELIVERY_TIMEOUT`, or cannot be reached (connection or TLS error). Every kind of failure is retried, 4xx included, on the schedule in `HOOKD_RETRY_SCHEDULE`:

- The first attempt is made right away. If attempt *n* fails and the schedule has an *n*th wait, attempt *n*+1 is due that long after attempt *n* ended. With the default `5s,5m,30m,2h,5h,10h,10h`, that is 8 attempts over about 27½ hours.
- Each wait is multiplied by a random factor between 0.9 and 1.1, so retries to many endpoints do not all fire at the same moment.
- The first attempt that succeeds makes the delivery `succeeded`. If the last attempt fails, the delivery is `failed`. Either way nothing more is sent.
- While a retry waits, the delivery is `pending`, with the time of its next attempt (`next_attempt_at` in the API, "next retry at …" in the admin UI). Waiting retries are rows in the database: they hold no worker, survive a restart (a retry that came due while Hookd was down is sent soon after it starts) and are shared by every instance.
- A retry is sent within about `HOOKD_POLL_INTERVAL` (1 s) of its due time, when a worker is free.
- If the endpoint is disabled or deleted while a retry waits, the delivery is not sent again. It ends `failed` with the error `endpoint_disabled` within moments. Enabling the endpoint again does not bring it back.
- A retry sends the same body with the same `webhook-id`, signed again with a new `webhook-timestamp` and the endpoint's current secret.

`none` (in any case) turns retries off: every delivery gets one attempt. An empty value keeps the default, as for every setting. Spaces around the commas are allowed. For a demo or a local test, a short schedule such as `5s,10s,20s` (4 attempts) shows retries within a minute; set it in the environment or on the Settings page.

The schedule is checked at start-up and when it is saved in the UI:

```
hookd: invalid configuration:
  HOOKD_RETRY_SCHEDULE="5s,x": must be "none" or up to 20 comma-separated positive durations such as "5s,5m,30m"
```

## Retention

Events are kept for `HOOKD_RETENTION` (30 days by default) after Hookd accepted them. After that Hookd deletes each event together with its deliveries and attempts, without anyone having to do anything. A deleted event answers 404 in the API and "not found" in the admin UI. Accounts, API keys, endpoints, saved settings and test receivers are never expired; a test endpoint's received requests keep their own rule (the newest 100).

- **When.** `hookd serve` checks one minute after it starts, then once an hour. Each run deletes the expired events in batches of 500, each batch one short transaction, until none are left. It logs `deleted expired events` with the cutoff time and the count.
- **Unfinished deliveries.** An expired event that still has a delivery `pending` or being sent is kept until that delivery is finished, then deleted at the next run.
- **Several instances.** Every instance runs the deletion. On PostgreSQL, the batches are taken with `FOR UPDATE SKIP LOCKED`, so instances running at the same time work on different events.
- **Checks.** `HOOKD_RETENTION` must be longer than `HOOKD_IDEMPOTENCY_WINDOW`, so that no event is deleted while its idempotency key is still honoured, and longer than the total of `HOOKD_RETRY_SCHEDULE` plus 10% (its largest jitter), so that no event is deleted while a retry is still waiting. With the defaults the total is about 30h20m, well under 720h. Hookd refuses to start otherwise, and the Settings page refuses a retry schedule or idempotency window that would break these rules:

  ```
  hookd: invalid configuration:
    HOOKD_RETENTION="24h0m0s": must be longer than HOOKD_IDEMPOTENCY_WINDOW="24h0m0s"
    HOOKD_RETENTION="24h0m0s": must be longer than the total of HOOKD_RETRY_SCHEDULE plus 10% ("30h20m35.5s")
  ```

Raising `HOOKD_RETENTION` keeps events that have not been deleted yet for longer; it cannot bring back deleted ones. Lowering it deletes the events that are now too old at the next run, within about an hour of the restart.

## Logging

Hookd logs to standard error with Go's `log/slog`: one line per API or admin UI request (method, path and status only), one per delivery attempt (`delivery attempt`, with the attempt number, the outcome and, when a retry is scheduled, `next_attempt_at`), one per retention run, and one per startup step. API keys, signing secrets and `Authorization` headers are never logged, nor are the headers and bodies that the built-in test receiver records; those are kept only in the database. Use `HOOKD_LOG_FORMAT=json` when the logs go to a log collector.

## The test receiver

The built-in test receiver of the admin UI has no settings of its own beyond `HOOKD_ADMIN_UI` and `HOOKD_ADMIN_RECEIVER_URL`. Each test endpoint's answer status is set on its page in the UI.

`cmd/testreceiver`, the separate development receiver, takes flags instead of environment variables:

| Flag | What it does | Default |
|---|---|---|
| `--secret` | Endpoint signing secret, `whsec_...`. Required. | none |
| `--addr` | Address to listen on. | `127.0.0.1:9000` |
| `--status` | HTTP status to answer every request with. | `200` |
