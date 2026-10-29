<!-- Synced by Software Factory from commit b8aa672967f49b84534294ffeb135ae03f357d12, 2026-10-29T12:53:01.611Z. Do not edit this line. -->
# Hookd

## What it is

Hookd is a webhook sending service. An application that has to notify its own customers' systems of events (an order paid, a user created) registers those customers' URLs with Hookd as endpoints and sends Hookd the events. Hookd stores each event, delivers it in the background to every endpoint subscribed to its type, and signs each delivery under the [Standard Webhooks](https://www.standardwebhooks.com/) spec. Receivers can then check signatures with an existing library and don't need custom code.

The application doesn't have to build and run its own webhook sending, signing and delivery tracking. It makes one HTTP call per event and can look up later whether each delivery succeeded.

Hookd also serves a browser admin UI at `/admin`, from the same binary. Whoever runs Hookd can use it to create accounts and API keys, register endpoints, send events, check deliveries, receive and verify test webhooks with a receiver built into Hookd, and change settings while Hookd runs, without a shell on the host. The UI has no sign-in yet, so it must be reachable only by trusted people, and it can be turned off.

A failed delivery is retried automatically on a backoff schedule for about a day, and every attempt is recorded, so the application can find out later why a customer did not get a webhook. Events and their history are kept for 30 days and then deleted.

## Who it is for

- Developers whose application has to send webhooks to its customers. They use Hookd's JSON API with an API key.
- Operators who run Hookd and issue accounts and API keys from its command line or its admin UI.
- People who run webhook receivers and verify deliveries with the endpoint's signing secret.

## Main features

- JSON HTTP API to create, list, update, disable and delete endpoints.
- Each endpoint subscribes to chosen event types, or to all of them.
- Events are accepted at once with `202` and delivered in the background.
- Deliveries are signed under Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`), with a secret per endpoint.
- Delivery status per endpoint (`pending`, `succeeded`, `failed`), with the response status and the error of the last attempt, the number of attempts, and when the next retry is due.
- Automatic retries of failed deliveries (any non-2xx answer, timeout or connection error): by default 8 attempts over about 27½ hours, with waits of 5 s, 5 min, 30 min, 2 h, 5 h, 10 h and 10 h, each varied by up to 10%. The schedule is a setting, `HOOKD_RETRY_SCHEDULE`, and `none` turns retries off. Waiting retries are stored in the database, so they survive restarts and are shared by every instance. Disabling or deleting an endpoint stops its waiting retries.
- Attempt history: every attempt is recorded with its number, start time, outcome, HTTP status, latency and error. The API lists an event's attempts (oldest first) and an endpoint's attempts (newest first, filtered by outcome, paged), and gets one attempt by ID.
- Retention: events are deleted with their deliveries and attempts 30 days after they were accepted (`HOOKD_RETENTION`), in small batches in the background, by every instance.
- `Idempotency-Key` header, so a repeated send returns the original event instead of a new one.
- Accounts are kept apart: each API key sees only its own account's endpoints and events.
- Operator commands to create accounts and to create, list and revoke API keys.
- Delivery survives restarts, because the database is the queue. Several instances can share one PostgreSQL database.
- `/healthz` check that includes the database.
- Every setting comes from a `HOOKD_*` environment variable, and every setting has a default that works locally. Eight of them can be overridden from the admin UI.
- An admin UI under `/admin` (no sign-in, can be turned off with `HOOKD_ADMIN_UI=false`): accounts and API keys (each key shown once), endpoints with their secrets shown on demand and their recent attempts, sending events and following their deliveries and attempts, including when the next retry is due, and a settings and health page that also counts the deliveries waiting for a retry.
- Test endpoints, created in the admin UI, that deliver to a receiver built into Hookd. It checks each request's signature, answers with a status of your choice (for example 500 to see failures), and keeps the last 100 requests for the UI to show.
- Eight settings (worker count, poll interval, delivery timeout and lease, payload limit, idempotency window, log level, retry schedule) can be changed in the admin UI. They apply without a restart, on every instance within 5 seconds.
- A development test receiver, `cmd/testreceiver`, that verifies signatures with the official library.

## Built with

- Go (1.26 or later), standard library `net/http` and `log/slog`, no web framework.
- SQLite locally through `modernc.org/sqlite` (pure Go, nothing to install).
- PostgreSQL in production through `github.com/jackc/pgx/v5`.
- `github.com/oklog/ulid/v2` for prefixed IDs.
- `github.com/standard-webhooks/standard-webhooks/libraries`, used by the built-in test receiver, the tests and `cmd/testreceiver` to verify signatures.
- The admin UI uses Go's `html/template` and `embed`, with plain HTML forms and no JavaScript framework or build step.
- AWS CDK in TypeScript (`deploy/aws`) for a single EC2 test box running Hookd on SQLite behind nginx.
