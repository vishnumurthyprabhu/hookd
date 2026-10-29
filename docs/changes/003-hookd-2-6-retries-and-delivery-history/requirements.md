# Requirements: Hookd 2/6 — Retries and delivery history

## Summary

In piece 1, Hookd tries each delivery once and records only the last outcome. This piece makes delivery reliable and visible. A failed delivery is retried automatically on a backoff schedule for about a day. Every attempt is recorded with its time, response status, latency and error, and the API lists them, per event and per endpoint. The admin UI shows the same history. Events, with their deliveries and attempts, are kept for 30 days and then deleted.

Everything else from piece 1 (Hookd 1/6) and the admin UI keeps working as it does now. Replay, resending what an endpoint missed and auto-pausing (piece 3), secret rotation (4), per-endpoint isolation (5) and network safety (6) are later pieces, not this one.

## Users

The same as in piece 1:

- **Developer**: calls the API. Looks up attempts to find out why a customer did not get a webhook.
- **Receiver**: gets the retries. Uses `webhook-id` to drop duplicates, as before.
- **Operator / Admin**: runs Hookd, sets the retry schedule and retention, and checks deliveries in the admin UI.

## Functional requirements

### Retries

- R1. A failed attempt is retried automatically. What counts as a failure is unchanged from Hookd 1/6 R16 and R18: any status other than 2xx (3xx included, redirects are still not followed), a timeout, or a connection or TLS error.
- R2. The default schedule, counted from the previous attempt, is: right away, then after 5 seconds, 5 minutes, 30 minutes, 2 hours, 5 hours, 10 hours and 10 hours. That is 8 attempts over about 27½ hours. Each wait is varied at random by up to 10%, so retries to many endpoints do not all fire at the same moment.
- R3. A delivery that succeeds on any attempt is `succeeded`, and nothing more is sent. A delivery whose last scheduled attempt fails is `failed`, and nothing more is sent.
- R4. If the endpoint is disabled or deleted while a delivery is waiting for a retry, the delivery is not sent again. It ends as `failed` with the error `endpoint_disabled`, as in Hookd 1/6 R7. Enabling the endpoint again does not bring it back (resending missed events is piece 3).
- R5. A retry sends the same event: the same body and the same `webhook-id`. `webhook-timestamp` and `webhook-signature` are made new for each attempt, with the endpoint's current secret.
- R6. Retries waiting for their time survive a restart: a retry due while Hookd was down is sent soon after it starts again. They are shared between instances like any other delivery, and a retry is never sent by two instances at once (apart from the existing at-least-once case after a crash in the middle of an attempt).
- R7. A retry is sent within a few seconds of its due time under normal load. Waiting retries do not hold workers or delay new events.
- R8. The schedule is a setting, `HOOKD_RETRY_SCHEDULE`: a list of waits, for example `5s,5m,30m,2h,5h,10h,10h`. An empty list means no retries (one attempt, as in piece 1). The admin can change it in the admin UI like the other editable settings, and a change applies to the next wait that is scheduled; waits already scheduled keep their time. The values are checked when they are saved and at start-up.

### Delivery status

- R9. `GET /v1/events/{id}` keeps its shape. For each endpoint it also gives the number of attempts made so far, the time of the last attempt, and, while a retry is waiting, the time of the next one. A delivery waiting for a retry has the status `pending`, as it does while waiting for its first attempt. Its `response_status` and `error` are those of the last attempt.

### Attempt history

- R10. Every attempt is recorded, including the one that found the endpoint disabled: the event, the endpoint, the attempt number (1, 2, …), the time it started, the outcome (`succeeded` or `failed`), the HTTP status if there was one, the latency in milliseconds, and the error text if it failed.
- R11. A developer can list the attempts of one event, across all its endpoints, oldest first. The list can be filtered to one endpoint.
- R12. A developer can list the attempts made to one endpoint, newest first, across all events. The list can be filtered by outcome (`succeeded` or `failed`) and is paged.
- R13. A developer can get one attempt by its ID.
- R14. Attempts are scoped by account like everything else: an event, endpoint or attempt of another account answers 404. The attempts of a deleted endpoint can still be listed until they expire (R17).

### Admin UI

- R15. The event page shows, for each endpoint, the number of attempts, when the next retry is due if one is waiting, and the list of its attempts with their time, outcome, HTTP status, latency and error. It keeps refreshing on its own while any delivery is pending or in progress, and also while a retry is waiting (for example with a slower refresh and the "Refresh" link).
- R16. The endpoint page shows the endpoint's recent attempts across events, newest first and paged, with a link to each attempt's event. The event list's summary counts deliveries waiting for a retry as pending, and the health section shows how many deliveries are waiting for a retry.

### Retention

- R17. Events are kept for 30 days after they were accepted. After that, Hookd deletes the event together with its deliveries and attempts, without the operator having to do anything. Expired events then answer 404 in the API and "not found" in the admin UI.
- R18. The retention period is a setting, `HOOKD_RETENTION`, default `720h` (30 days). It must be longer than both the idempotency window and the total of the retry schedule, so that no event is deleted while its key is still honoured or a retry is still waiting. It is set with an environment variable only.
- R19. Expired events are removed in small batches in the background, at least once an hour, on any instance. Deleting them does not lock the tables for long or slow down delivery, and it is safe with several instances running.

## Non-functional requirements

- N1. An attempt's record and the scheduling of its next retry are stored together, so a crash never leaves a delivery with a recorded failure and no next step, or with a next step and no record.
- N2. The attempt lists answer in under a second with 100,000 events per account and 8 attempts per delivery, because they are paged and indexed.
- N3. Everything works on SQLite and PostgreSQL, with one or several instances. Existing databases are migrated without losing data: deliveries made before this piece get one attempt each from their recorded outcome.
- N4. Secrets and API keys are still never logged. Retries are logged like first attempts, with the attempt number.
- N5. The API stays backward compatible: existing fields and status values keep their meaning, and only fields and routes are added.

## Acceptance

This piece is done when, starting from a fresh local setup with `go run ./cmd/hookd serve`:

1. The admin sets `HOOKD_RETRY_SCHEDULE` to `5s,10s,20s` in the admin UI, so the test does not take hours.
2. The admin creates a test endpoint that answers 500 and sends an event to it.
3. The event page shows the first attempt failed with 500, then a second attempt about 5 seconds later, also failed, and the time of the next retry. `GET /v1/events/{id}` shows the delivery as `pending` with 2 attempts and the next attempt time.
4. Before the next retry, the admin sets the test endpoint to answer 200. The next attempt succeeds, the delivery shows `succeeded` after 3 attempts, and no further attempt is made. The test endpoint's received requests show three requests with the same `webhook-id`, each with "signature verified".
5. The event's attempt list in the API shows the three attempts with their status, latency and error, and the endpoint's attempts list shows the same three, newest first, and can be filtered to the two failed ones.
6. An event sent to an endpoint that keeps answering 500 ends `failed` after 4 attempts, and no fifth is made.
7. Disabling an endpoint while a retry is waiting makes the delivery end `failed` with `endpoint_disabled`, and no further request reaches the receiver.
8. Stopping Hookd while a retry is waiting and starting it again after the due time sends the retry soon after start.
9. With `HOOKD_RETENTION` set short in a test (for example in the Store tests with a fake clock), events older than it are deleted with their deliveries and attempts, and newer ones are kept.
10. Another account's key gets 404 for the attempt routes of the first account's events, endpoints and attempts.

## Out of scope for this piece

- Replaying an event, resending everything an endpoint missed, and auto-pausing broken endpoints (piece 3)
- Secret rotation (piece 4), per-endpoint isolation from slow endpoints (piece 5), blocking internal networks and fixed egress IPs (piece 6)
- Retrying a delivery by hand, or cancelling a waiting retry, other than by disabling the endpoint
- Storing the endpoint's response body or response headers with an attempt
- A schedule per account or per endpoint, or honouring a `Retry-After` header
- Per-account retention periods, and exporting history before it expires
- Alerting or notifying anyone when deliveries fail

## Assumptions

- The schedule follows the one common in webhook services (and suggested by Standard Webhooks providers): a few quick retries for brief blips, then longer waits, ending a little over a day after the event. "About a day" is read as that.
- Every kind of failure is retried, including 4xx answers, because a receiver's 4xx is often a passing misconfiguration. Only a disabled or deleted endpoint stops the retries early.
- The API keeps three delivery statuses (`pending`, `succeeded`, `failed`); waiting for a retry is shown with the new next-attempt field rather than a new status, so existing clients keep working (N5).
- The retry schedule is editable in the admin UI because it makes demos and tests practical; this makes eight editable settings instead of seven. Retention is kept to the environment, since shortening it by mistake deletes data.
- The attempt routes are `GET /v1/events/{id}/attempts`, `GET /v1/endpoints/{id}/attempts` and `GET /v1/attempts/{id}`; the exact shape is settled in the design.
- 30-day retention applies to events, deliveries and attempts. Accounts, API keys, endpoints, settings and test receivers are not expired. A test receiver's stored requests keep their existing rule (the last 100).
- An attempt cut short by a crash is not recorded; its delivery is claimed again after the lease, as today, and that attempt is recorded normally.
