# Summary: Hookd 1/6 — Signed event delivery

## What was delivered

- One Go binary, `hookd`, with an HTTP API to register endpoints, pick the event types each one receives, and send events.
- Each event is stored, then sent in the background to every enabled, subscribed endpoint: one attempt, 15 s timeout, no redirects.
- Every delivery is signed under Standard Webhooks; the included test receiver checks it with the official library.
- Separate accounts with API keys; one account can't see another's data. Operator commands create accounts and issue, list and revoke keys.
- SQLite locally, PostgreSQL in production, plus setup, configuration, deployment, testing and API guides.

## What it looks like

Run locally with default settings (sent over a raw socket, as this machine has no `curl`; bodies shortened).

```
POST /v1/events   Idempotency-Key: order-1001-paid
{"event_type": "order.paid", "payload": {"type": "order.paid", "order_id": "1001", "amount": 4200}}

HTTP/1.1 202 Accepted
{"id":"msg_01M638KJSKXFNHE3J3KS4A68RE", ..., "deliveries":[{"endpoint_id":"ep_01M638KCHM2EF46NEBBC6X6EZE","status":"pending","response_status":null}]}
```

The test receiver logged:

```
level=INFO msg=verified id=msg_01M638KJSKXFNHE3J3KS4A68RE type=order.paid ... verified=true
```

```
GET /v1/events/msg_01M638KJSKXFNHE3J3KS4A68RE

HTTP/1.1 200 OK
{..., "deliveries":[{"endpoint_id":"ep_01M638KCHM2EF46NEBBC6X6EZE","status":"succeeded","response_status":200}]}
```

## How to try it

From `README.md`, in three terminals:

```bash
go run ./cmd/hookd serve
go run ./cmd/hookd account create --name acme   # prints an API key
go run ./cmd/testreceiver --secret whsec_...    # secret from POST /v1/endpoints
go test ./...
```
## Not done or worth knowing

- Failures are recorded, not retried. Retries, replay, secret rotation, per-endpoint isolation and network safety are pieces 2–6 (open issues hookd-620, hookd-pu3, hookd-ati, hookd-8qw, hookd-xxk).
- Nobody has yet run the systemd unit (`systemctl`, `journalctl`) or the `docker run` PostgreSQL steps in the guides on a real host, because this environment has neither systemd nor Docker. Follow-up hookd-y40 is marked closed, but its notes say this was not done.
