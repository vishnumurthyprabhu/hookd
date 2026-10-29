<!-- Synced by Software Factory from commit a2e6af58e55011b0d8aa5fd8623257669503f524, 2026-10-29T07:24:51.888Z. Do not edit this line. -->
# Hookd

## What it is

Hookd is a webhook sending service. An application that has to notify its own customers' systems of events (an order paid, a user created) registers those customers' URLs with Hookd as endpoints and sends Hookd the events. Hookd stores each event, delivers it in the background to every endpoint subscribed to its type, and signs each delivery under the [Standard Webhooks](https://www.standardwebhooks.com/) spec. Receivers can then check signatures with an existing library and don't need custom code.

The application doesn't have to build and run its own webhook sending, signing and delivery tracking. It makes one HTTP call per event and can look up later whether each delivery succeeded.

Hookd also serves a browser admin UI at `/admin`, from the same binary. Whoever runs Hookd can use it to create accounts and API keys, register endpoints, send events, check deliveries, receive and verify test webhooks with a receiver built into Hookd, and change settings while Hookd runs, without a shell on the host. The UI has no sign-in yet, so it must be reachable only by trusted people, and it can be turned off.

In this version each delivery is attempted once. Failures are recorded but not retried.

## Who it is for

- Developers whose application has to send webhooks to its customers. They use Hookd's JSON API with an API key.
- Operators who run Hookd and issue accounts and API keys from its command line or its admin UI.
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
- Every setting comes from a `HOOKD_*` environment variable, and every setting has a default that works locally. Seven of them can be overridden from the admin UI.
- An admin UI under `/admin` (no sign-in, can be turned off with `HOOKD_ADMIN_UI=false`): accounts and API keys (each key shown once), endpoints with their secrets shown on demand, sending events and following their deliveries, and a settings and health page.
- Test endpoints, created in the admin UI, that deliver to a receiver built into Hookd. It checks each request's signature, answers with a status of your choice (for example 500 to see failures), and keeps the last 100 requests for the UI to show.
- Seven settings (worker count, poll interval, delivery timeout and lease, payload limit, idempotency window, log level) can be changed in the admin UI. They apply without a restart, on every instance within 5 seconds.
- A development test receiver, `cmd/testreceiver`, that verifies signatures with the official library.

## Built with

- Go (1.26 or later), standard library `net/http` and `log/slog`, no web framework.
- SQLite locally through `modernc.org/sqlite` (pure Go, nothing to install).
- PostgreSQL in production through `github.com/jackc/pgx/v5`.
- `github.com/oklog/ulid/v2` for prefixed IDs.
- `github.com/standard-webhooks/standard-webhooks/libraries`, used by the built-in test receiver, the tests and `cmd/testreceiver` to verify signatures.
- The admin UI uses Go's `html/template` and `embed`, with plain HTML forms and no JavaScript framework or build step.
