# Summary: Retries and delivery history

## What was delivered

- A failed delivery is retried on its own: by default 5s, 5m, 30m, 2h, 5h, 10h and 10h after each failure (±10%), 8 attempts over about a day. Waiting retries survive a restart.
- The schedule is a setting, `HOOKD_RETRY_SCHEDULE`, changeable in the admin UI while Hookd runs; `none` turns retries off.
- Every attempt is recorded (time, HTTP status, latency, error) and listed by new API routes: per event, per endpoint (filter by outcome, paged) and one by ID.
- The admin UI shows each delivery's attempts and next retry, and an endpoint's recent attempts.
- Disabling or deleting an endpoint stops its retries with `endpoint_disabled`.
- Events are deleted with their history after 30 days (`HOOKD_RETENTION`).

## What it looks like

I ran it locally (SQLite, schedule `5s,10s,20s`, `cmd/testreceiver` answering 500). Seven seconds after sending, `GET /v1/events/{id}`:

```json
{"endpoint_id":"ep_01M64FTFXY…","status":"pending","response_status":500,"error":"non-2xx response: 500 Internal Server Error","attempts":2,"last_attempt_at":"2026-10-29T18:47:51.559341Z","next_attempt_at":"2026-10-29T18:48:02.048888Z"}
```

After I switched the receiver to 200, attempt 4 succeeded: `"status":"succeeded","response_status":200,"attempts":4,"next_attempt_at":null`. `GET /v1/endpoints/{id}/attempts?outcome=failed` returned attempts 3, 2, 1, newest first. The receiver logged the same `webhook-id` with `verified=true` every time.

## How to try it

```bash
go run ./cmd/hookd serve
```

Open <http://localhost:8080/> and follow README steps 3–8 ("Watch a retry"). The `curl` version is under "The same with the API", and `docs/api.md` covers the attempt routes.

## Not done or worth knowing

- Replay, auto-pause, manual retry and per-endpoint schedules are left to later pieces (hookd-pu3 onwards). No follow-up issues were filed for this change.
- Every failure is retried, 4xx included, so receivers must drop duplicates by `webhook-id`.
- Retention can only be set with an environment variable.
- All Hookd packages pass `go test ./...`. Locally it also reports a setup failure in `deploy/aws/node_modules`, which git ignores.
