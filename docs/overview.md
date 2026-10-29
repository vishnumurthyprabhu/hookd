<!-- Synced by Software Factory from commit a2e6af58e55011b0d8aa5fd8623257669503f524, 2026-10-29T07:24:51.888Z. Do not edit this line. -->
# Hookd

## What it is

Hookd is a webhook sending service. An application that has to notify its own customers' systems of events (an order paid, a user created) registers those customers' URLs with Hookd as endpoints and sends Hookd the events. Hookd stores each event, delivers it in the background to every endpoint subscribed to its type, and signs each delivery under the [Standard Webhooks](https://www.standardwebhooks.com/) spec. Receivers can then check signatures with an existing library and don't need custom code.

The application doesn't have to build and run its own webhook sending, signing and delivery tracking. It makes one HTTP call per event and can look up later whether each delivery succeeded.

In this version each delivery is attempted once. Failures are recorded but not retried.

## Who it is for

- Developers whose application has to send webhooks to its customers. They use Hookd's JSON API with an API key.
- Operators who run Hookd and issue accounts and API keys from its command line.
- People who run webhook receivers and verify deliveries with the endpoint's signing secret.

## Main features

- JSON HTTP API to create, list, update, disable and delete endpoints.
- Each endpoint subscribes to chosen event types, or to all of them.
- Events are accepted at once with `202` and delivered in the background.
- Deliveries are signed under Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`), with a secret per endpoint.
- Delivery status per endpoint (`pending`, `succeeded`, `failed`), with the response status and the error.
- `Idempotency-Key` header, so a repeated send returns the original event instead of a new one.
- Accounts are kept apart: each API key sees only its own account's endpoints and events.
- Operator commands to create accounts and to create, list and revoke API keys.
- Delivery survives restarts, because the database is the queue. Several instances can share one PostgreSQL database.
- `/healthz` check that includes the database.
- Every setting comes from a `HOOKD_*` environment variable, and every setting has a default that works locally.
- A development test receiver that verifies signatures with the official library.

## Built with

- Go (1.26 or later), standard library `net/http` and `log/slog`, no web framework.
- SQLite locally through `modernc.org/sqlite` (pure Go, nothing to install).
- PostgreSQL in production through `github.com/jackc/pgx/v5`.
- `github.com/oklog/ulid/v2` for prefixed IDs.
- `github.com/standard-webhooks/standard-webhooks/libraries`, used in tests and the test receiver only.
