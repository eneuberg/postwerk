# Requirements

Numbered so code, tests and reviews can point at them. Source: [design.md](design.md), [decisions.md](decisions.md),
[scope.md](scope.md). "Phase" says when a requirement is built: **F** = foundation (first build), **L** = later.

Requirements marked *(safe default, proposed)* come from the open gaps in scope.md section 7. They are built because
they reduce risk at little cost; each can be reverted if the maintainer decides otherwise.

Retention, encryption at rest and keyed hashing (S6–S11, E1–E5, K1–K4) follow the recommended defaults of
[privacy-and-security.md](privacy-and-security.md) sections 3, 4, 6 and 8 and are built in phase F. Every setting and
its default is listed in [configuration.md](configuration.md).

## Principles

| ID | Requirement | Phase |
|---|---|---|
| P1 | Never lose track of an accepted mail. Every accepted mail ends in a recorded final state or is visibly pending. | F |
| P2 | Never send a mail twice to the same target by accident. A resend after an unclear outcome is an explicit policy. | F |
| P3 | Small attack surface: few listeners, few dependencies, no content in logs, events, API or UI. | F |
| P4 | Few moving parts: one process, one SQLite file, environment configuration, no external services required. | F |
| P5 | Content-blind: message content is forwarded unchanged except for Postwerk's own headers. | F |

## Ingress

| ID | Requirement | Phase |
|---|---|---|
| I1 | SMTP server. Every transaction requires `AUTH PLAIN` with a known sender; `MAIL` before `AUTH` is refused. | F |
| I2 | Sender passwords are stored as salted PBKDF2-SHA256 hashes; unknown users cost the same time as known ones. | F |
| I3 | `AUTH` over cleartext only if explicitly allowed; otherwise the server needs a TLS certificate (STARTTLS). | F |
| I4 | A connection is closed after 3 failed `AUTH` attempts. | F |
| I5 | Limits: message size (`POSTWERK_MAX_MESSAGE_BYTES`, default 15 MiB, the `SIZE` Mailjet offers; see I17), recipients per message, line length, read/write timeouts, concurrent connections (`POSTWERK_MAX_CONNECTIONS`, default 100; a connection over the limit gets a fixed `421` and is closed). | F |
| I6 | `250` is sent only after the mail and its deliveries are committed with `synchronous=FULL`. The reply names the id: `250 2.0.0 queued as pw_…`. | F |
| I7 | If storing fails, reply `451` (temporary) and keep nothing half-written. Internal error texts never reach the client. | F |
| I8 | `X-Postwerk-*` headers set by the sender are read (`Ref`, `Skip`) and removed before storage. *(safe default, proposed; scope gap 7)* | F |
| I9 | An invalid `X-Postwerk-Ref` (contains `@`, is longer than 256 bytes or not printable ASCII) is not stored; the mail is still accepted, without a ref. The header is removed either way. | F |
| I10 | Ingress offers `8BITMIME` and `SMTPUTF8`, not `DSN`, `REQUIRETLS` or `BINARYMIME`. The MAIL parameters `BODY=7BIT\|8BITMIME`, `SMTPUTF8` and `SIZE=` are stored as declared. | F |
| I11 | Envelope addresses may hold UTF-8 (RFC 6531) only with `SMTPUTF8`; otherwise fixed reply `553 5.6.7`. Invalid UTF-8, control characters (C0, DEL, C1) and Unicode line separators get `553 5.1.3`. | F |
| I12 | Every password check that is not a cached login costs PBKDF2 with the strongest iteration count in the senders file, for unknown names, mismatched identities and weaker hashes alike. | F |
| I13 | At most `runtime.GOMAXPROCS(0)` password checks run at once; a login that finds no slot within 5 s gets `454 4.7.0 Temporary authentication failure, try again later` (not counted as a failure). Cached logins (one HMAC) need no slot. | F |
| I14 | A connection that has not logged in within 30 s is closed (`421 4.4.2`, the SMTP library's fixed idle reply); the deadline also covers a STARTTLS handshake. No per-IP blocking. | F |
| I15 | Memory budget `POSTWERK_MAX_INFLIGHT_BYTES` (default 256 MiB): each `DATA` reserves the maximum message size, or the declared `SIZE` if smaller, before reading; budget exhausted → fixed `451 4.3.1`, nothing buffered; released when the mail is stored or refused. | F |
| I16 | The STARTTLS certificate and key are re-read every 60 s and compared by content (a Kubernetes symlink swap counts). A changed pair is served from the next handshake only if it parses, matches and has not expired; otherwise the old one stays and `tls.reload_failed` (error) is written once per content. Each reload writes `tls.reloaded`; less than 14 days left writes `tls.expiring` once a day, a warning, an error below 7 days (at once when it crosses) and once expired; the time left is rounded down. | F |
| I17 | `POSTWERK_MAX_MESSAGE_BYTES` is the largest message a target receives. Ingress accepts and offers as `SIZE` that value minus the bytes of the header lines Postwerk adds, computed exactly for the configured targets (46 for `X-Postwerk-Id`, 46 more for `X-MJ-CustomID` with `STATUS=mailjet`, and 49 more for the two tracking switches with `MAILJET_TRACKING=off`, D23). Sizes are counted as sent: a bare LF counts as CRLF, at ingress and in `SIZE=` towards targets. | F |
| I18 | Free-space reserve `POSTWERK_MIN_FREE_BYTES` (default 64 MiB, `0` off) on the data volume, read with `statfs` at `MAIL`, `RCPT`, after `DATA` (with the message's size) and every 10 s: below it, fixed `452 4.3.1 Insufficient system storage, try again later` and nothing stored; one `disk.low` (error), `disk.ok` (info) at the reserve plus a quarter (D50). | F |
| I19 | Failed `AUTH` and refused commands of a logged-in sender are counted: at most one `ingress.auth_failed` (`failures=<n>`) per sender and minute, and one `ingress.rejected` (`reason=<r> rejected=<n>`) per sender, reason and minute, both `warn`, written when the minute ends and at shutdown before the final export. The sender only if the name is in the senders file; never a password or an address (D51). | F |

## Store

| ID | Requirement | Phase |
|---|---|---|
| S1 | One SQLite file, WAL, `synchronous=FULL`, foreign keys on, one connection (single writer). | F |
| S2 | Tables `mails`, `recipients`, `deliveries`, `delivery_recipients`, `events`; schema versioned with `user_version`, migrations embedded. | F |
| S3 | `events` is append-only, enforced by triggers: no update ever; a delete only for events older than the retention cutoff (S10). | F |
| S4 | State change and its event are written in the same transaction. | F |
| S5 | No message content and no recipient address in `events`. SMTP reply texts are stored with addresses redacted. | F |
| S6 | Retention job: runs at start (before any delivery) and then every 10 minutes; each run that changed something writes one `retention.purged` event with counts only. | F |
| S7 | Raw message removal: once every recipient of every delivery is `handed_off` or `skipped`, after `POSTWERK_RAW_KEEP_AFTER_HANDOFF` (default 0: in the same transaction that makes the last recipient final); once every recipient is final with one `failed` or `unknown`, after `POSTWERK_RAW_KEEP_AFTER_FAILURE` (72 h); in any state after `POSTWERK_RAW_MAX_AGE` (7 d). A recipient still `queued` or `unknown` at removal becomes `failed` with `retention.raw_expired` (`error`, `warn` for optional targets). Every retry window must be shorter than the maximum age. | F |
| S8 | Pseudonymization: after `POSTWERK_PSEUDONYMIZE_AFTER` (30 d), recipient addresses and the envelope sender are replaced by keyed hashes (K1), once no delivery may still be sent. | F |
| S9 | Record retention: `mails`, `recipients` and `deliveries` rows are deleted after `POSTWERK_RECORD_RETENTION` (90 d), except mails with a recipient still `queued`, `sending` or `unknown` at any target. | F |
| S10 | Event retention: events older than `POSTWERK_EVENT_RETENTION` (90 d) are deleted. The cutoff lives in a single-row table, can only move forward (trigger), and the delete trigger on `events` allows nothing newer than it. | F |
| S11 | Deleted content is overwritten (`POSTWERK_SECURE_DELETE`, default on), and every retention run checkpoints and truncates the WAL, so old page copies do not linger. | F |
| S12 | Retention periods are validated together at start: keep periods ≤ maximum age ≤ pseudonymization age (if on) ≤ record retention; maximum age ≤ event retention. | F |

## Encryption at rest

| ID | Requirement | Phase |
|---|---|---|
| E1 | The stored message is encrypted with AES-256-GCM under the active key of `POSTWERK_STORE_KEY_FILE`, with a random 96-bit nonce. The blob holds a version byte and the key id. | F |
| E2 | The mail id is authenticated with the ciphertext (additional data), so a blob moved to another row fails to decrypt. | F |
| E3 | A blob is decrypted with the key it names. An unknown key id, failed authentication or an unencrypted blob while encryption is on fails that delivery with `store.decrypt_failed` (`error`); nothing is sent, no plaintext is logged. | F |
| E4 | Encryption is on unless `POSTWERK_STORE_ENCRYPTION=off` is set explicitly; then a warning is logged at every start. | F |
| E5 | Key files: one `<key-id>:<base64 of 32 bytes>` per line, first line active, older lines for reading; read once at start, never from environment variables; `postwerk gen-key` creates a line. Store and hash key files must not share a key. Secret files readable by others cause a warning, not a refusal. | F |

## Keyed hashing of addresses

| ID | Requirement | Phase |
|---|---|---|
| K1 | A hash is HMAC-SHA256 under the active key of `POSTWERK_HASH_KEY_FILE`, stored as `<key-id>:<hex>`. | F |
| K2 | Normalization before hashing is fixed and documented: trim ASCII white space, lower-case ASCII `A`–`Z` in the whole address. | F |
| K3 | A lookup by address matches plain rows by the normalized address and pseudonymized rows by the query hashed with every key in the file. | F |
| K4 | `POSTWERK_HASH_KEY_FILE` is required while pseudonymization is on (the default). | F |

## Delivery

| ID | Requirement | Phase |
|---|---|---|
| D1 | One delivery per mail and target, each with its own attempts and next attempt time; its status is derived from its recipients (D15). | F |
| D2 | One worker per target; a slow or broken target never delays another. | F |
| D3 | Retry policy as in design.md section 5: `4xx` and network errors retry; `535` keeps the mail queued and raises an error at once, once for the target (D19); other `5xx` fail. | F |
| D4 | Backoff 30 s, 1 min, 2 min, 5 min, 10 min, 30 min, then hourly, until the retry window ends; then `failed`. | F |
| D5 | The moment before the message body is sent is recorded (`data_sent_at`). A connection error after that is an unclear outcome, handled by the target's `ON_UNKNOWN` policy (`hold`, `retry`; `check` needs a status adapter). | F |
| D6 | On start, deliveries left in `sending` are recovered: before `data_sent_at` → queued again; after → unclear outcome. | F |
| D7 | TLS towards targets: `smtps://` (implicit TLS) or `smtp://` (STARTTLS required). Cleartext only with the explicit scheme `smtp+plain://`, and never with credentials. *(safe default, proposed; scope gap 4)* | F |
| D8 | Per-target recipient allow-list, all or nothing per target: if one recipient is outside the list, the whole delivery to that target is skipped (`delivery.not_allowed`, warning). Nothing is rejected at `RCPT`; other targets still get the mail. *(safe default, proposed; scope gap 5)* | F |
| D9 | `X-Postwerk-Skip` excludes named targets for one mail. | F |
| D10 | Postwerk adds `X-Postwerk-Id` and, for Mailjet targets, `X-MJ-CustomID` (and the tracking switches of D23); for Mailjet targets any `X-MJ-CustomID` set by the sender is removed first, so tracking is unambiguous. | F |
| D11 | Graceful shutdown: stop accepting, let in-flight sends finish within a deadline, then close the store. One attempt is capped at 90 s (dial 15 s, command 30 s, reply after the message 60 s); the default deadline `POSTWERK_SHUTDOWN_TIMEOUT` is 100 s, longer than the cap; the orchestrator's grace period must be longer still (120 s). *(safe default, proposed; scope gap 3)* | F |
| D12 | Rate and connection limits per target. | L |
| D13 | MAIL parameters are passed on where the target offers the extension: `SMTPUTF8` as declared; `BODY=8BITMIME` to every target that offers it (the SMTP library always declares it; 7-bit content is valid 8BITMIME). | F |
| D14 | A mail that needs an extension the target does not offer is never sent without it: `SMTPUTF8` when declared and the envelope or header section holds non-ASCII; `8BITMIME` when the message holds any 8-bit byte. That delivery fails at once with `delivery.unsupported` (`error`, `warn` for optional targets). | F |
| D15 | The unit of tracking is (mail, target, recipient); its status (`queued`, `sending`, `handed_off`, `failed`, `unknown`, `skipped`) is the only stored status. Scheduling and attempts stay per (mail, target). Every result and every result event is per recipient. | F |
| D16 | RFC 5321 relay behaviour: `DATA` goes to the recipients the target accepted; `5xx` at `RCPT` fails that recipient, `4xx` (and `552`) keeps it queued and only queued recipients go into the next attempt; all refused → no `DATA`. An unclear outcome after `DATA` applies only to the recipients of that attempt. | F |
| D17 | The status of a delivery and of a mail is derived when reading, never stored: `queued` while any recipient is queued, `unknown` while any is unknown, `handed_off` if all were handed off, `failed` if none, `partial` if some were handed off and the others failed. A mail's status counts the recipients of its required targets only (all of them if it has no required delivery); optional targets keep their own delivery status. List filters and the recent problems use the same rule. | F |
| D18 | `SIZE=` is sent with the exact size of the outgoing message to targets that offer `SIZE`. A message larger than a target's `SIZE` is not sent there: `failed` with `delivery.too_large` (`error`, `warn` for optional targets). | F |
| D19 | Target health (decision D45): a `535` before the message, or a failure while connecting (dial, TLS, greeting, `EHLO`), takes the target down. Its worker sends nothing until a probe succeeds; one `target.down` event (`error`, `warn` for optional targets; reason `auth` or `unreachable`, the redacted reply or error) when it goes down (for `unreachable` once `POSTWERK_TARGET_DOWN_AFTER_PROBES` probes failed too, D58), one `target.recovered` (`info`, with the time it was down) when it comes back; no per-mail error events for it. The state lives in memory; after a restart every target starts up. | F |
| D20 | A probe connects, says `EHLO`, logs in if the target has credentials and quits; it never sends a mail. The first runs `POSTWERK_PROBE_INTERVAL` (30 s) after the target went down, each failed probe doubles the wait up to `POSTWERK_PROBE_MAX_INTERVAL` (5 min). When a probe succeeds, and when a worker starts, every queued delivery of its target becomes due at once. | F |
| D21 | While a target is down, its queued recipients whose retry window has ended become `failed` (`delivery.failed`) without another attempt. | F |
| D22 | A recipient address refused for good (`550`, `551` or `553` with an enhanced code `5.1.x` or `5.2.x`, or at `RCPT` without an enhanced code) is `failed` with the event `delivery.rejected` at `warn` (decision D46); every other permanent failure stays `delivery.failed` at `error`. | F |
| D23 | For `STATUS=mailjet` targets, `POSTWERK_TARGET_<NAME>_MAILJET_TRACKING` is `off` by default: Postwerk adds `X-Mailjet-TrackOpen: 0` and `X-Mailjet-TrackClick: 0` and removes the sender's fields of those names; `account` adds and removes nothing (decision D47). | F |
| D24 | `POSTWERK_TARGET_<NAME>_CA_FILE`: a PEM bundle of certificates, each checked at start, used instead of the system roots for that target (decision D48). Not allowed with `smtp+plain`. | F |

## Status, audit, reporting

| ID | Requirement | Phase |
|---|---|---|
| A1 | Every state change is an event with `seq`, time, mail id, target, kind, level; a result event also names the recipient's position, never its address. | F |
| A2 | Reporter reads events by cursor per sink; a sink that fails retries later and catches up. | F |
| A3 | stdout sink: one JSON line per event, always on. | F |
| A4 | Sinks for OpenTelemetry, Sentry protocol, signed webhook. The Sentry part is built (A6–A9); OpenTelemetry and webhook are later. | L (Sentry: F) |
| A5 | Provider status adapter (Mailjet), and the `check` policy for unclear outcomes. | L |
| A6 | Sentry-protocol sink, on when `POSTWERK_SENTRY_DSN_FILE` is set: one envelope per event to `/api/<project>/envelope/` with `X-Sentry-Auth`, standard library only. The DSN is checked at start and must be `https` unless `POSTWERK_SENTRY_ALLOW_HTTP=true`; a DSN in a variable is refused. `POSTWERK_SENTRY_MIN_LEVEL` `error` (default) or `warn`; lower events only move the cursor. | F |
| A7 | Each Sentry event is built from the event record only: message `<kind> at <target>: <code> <text>`, fingerprint kind and target, tags `kind`, `target`, `sender`, `instance`, `level`, `mail_id` (`actor` for UI events), extra `seq`, `attempt`, `recipient` (position), `smtp_code` and `smtp_text` for an event with a reply code, otherwise `text`, `ui_path`; environment is the instance. The reply text is redacted again. No addresses, content, user, request, breadcrumbs or host name. | F |
| A8 | The Sentry event id is derived from instance, seq, time, mail id and kind, so a repeated export is the same event for the receiver. | F |
| A9 | Every sink runs on its own; a slow sink holds up no other and never delivery. Sentry: 10 s timeout, `429` with `Retry-After` and `X-Sentry-Rate-Limits` respected, backoff 5 s to 5 min on network errors, `5xx` and a refused DSN, events refused with `400`/`413`/`422` skipped and logged, no redirects, body at most 16 KiB, final export at shutdown at most 10 s. | F |
| A10 | Every minute, per target: `queue.stale` (error, warn for optional targets; count and oldest age) when the oldest mail with a recipient `queued` or `sending` is older than `POSTWERK_QUEUE_STALE_AFTER` (default 15 min, `0` off), `queue.recovered` (info) when none is; once per change, again after `POSTWERK_QUEUE_STALE_REPEAT` if set. The state survives a restart through the event log (D49). | F |
| A11 | The free-space and queue checks start from their newest event in the event log, so a restart neither repeats a `disk.low` or `queue.stale` nor loses the recovery. | F |
| A12 | An exported event carries `"smtp": {code, text}` only when it has an SMTP reply code; every other text is `"text"` (D52). | F |

## HTTP

| ID | Requirement | Phase |
|---|---|---|
| H1 | Separate HTTP listener, default `127.0.0.1:8080`. `/healthz` (alive) and `/readyz` (store reachable and writable, volume above its reserve, workers running; H10). | F |
| H2 | JSON query API with bearer tokens (`read`, `admin`), every use logged, on its own listener (H4), on top of the query layer (U15): H4–H9 below, [api.md](api.md). Read-only except for an admin's close and resend of a delivery (H11–H13), which the UI offers too (U25). | F |
| H3 | Small UI, server-rendered, no content, mail records read-only except for close and resend: U1–U25 below, [ui.md](ui.md). | F |
| H4 | The API has its own listener `POSTWERK_API_LISTEN`, default `off`; it may bind a pod address and must differ from the health, SMTP and admin listeners. Optional TLS (`POSTWERK_API_TLS_CERT_FILE`, `POSTWERK_API_TLS_KEY_FILE`), re-read while running; without TLS on a non-loopback address, a warning at every start. | F |
| H5 | Tokens from `POSTWERK_API_TOKENS_FILE`, one `name:role:sha256$<digest>` line each (role `read` or `admin`, digest the SHA-256 of a random 32-byte token), sent as `Authorization: Bearer <name>.<token>`; `postwerk gen-api-token <name> <role>` prints both. Every check is one SHA-256 and one constant-time comparison, against a random dummy for an unknown name (D41). A line with a PBKDF2 hash is a configuration error. | F |
| H6 | `read`: `POST /v1/search` (id, `X-Postwerk-Ref`, `Message-ID`, address through K3; at most 20 mails) and `GET /v1/mails/{id}`, addresses masked (U12) and the `Message-ID` masked after its `@` (before it too if it looks like an address; D44), while a search takes the exact value; no list, no overview (`403`). `admin`: also `GET /v1/mails` (filters, pages) and `GET /v1/overview` (with each target's `state`, D19), addresses in full, searches up to 100 mails. Never content, subject or headers other than `Message-ID`. | F |
| H7 | Every authenticated request is an `api.request` event (`actor` the token name; endpoint, search kind, status and count as text), except a returned mail detail, which is `api.view` (`actor`, `mail_id`, like `ui.view`); `api.forbidden` for a role refusal. Missing, malformed and wrong tokens are counted: at most one `api.auth_failed` event per actor and minute, with `failures=<n>` as text, written when the minute ends and at shutdown (`actor` the token name only if it is in the file, otherwise empty; D42). Never the search term, a ref, a Message-ID or an address; a mail id only in `api.view`. A request is answered only once its event is written, otherwise `503`. | F |
| H8 | At most 60 requests per minute per token name and 600 for all callers together, counted before the token check; more get `429` with `Retry-After`. | F |
| H9 | JSON only: a `POST` needs `Content-Type: application/json` (UTF-8, else `415`), bodies at most 4 KiB (`413`), unknown fields refused, search terms only in the body. No CORS; requests with `Origin` or `Sec-Fetch-*` get `403`. Security headers and server timeouts on every response; optional `Host` allow-list `POSTWERK_API_HOSTS`. | F |
| H10 | `/readyz` fails while the store's last write failed for lack of space or storage (`SQLITE_FULL`, `IOERR`, `READONLY`, `CANTOPEN`; probed every 10 s until a write succeeds) or the volume is below its reserve (I18). It never fails because a target is down (D52). | F |
| H11 | Close (D53): an admin closes one delivery (mail, target): every recipient that is `queued`, `unknown` or `failed` without a resolution, with the resolution `confirmed` (status `handed_off`; refused while one of them is `queued`) or `given_up` (status `failed`), stored beside the status in `delivery_recipients.resolution`, and an optional note (at most 200 characters, one line of plain text, redacted). A closed recipient is final: raw expiry (S7), a claim, a close and a resend never touch it again. The same transaction removes the stored message if no recipient of the mail is `queued`, `sending`, `unknown` or `failed` without a resolution. Derived summaries (D17) read the status only. | F |
| H12 | Resend (D54): an admin queues the `failed` (not closed) and `unknown` recipients of one delivery again, due at once, while the stored message exists; never once it is removed, and retention is never extended for it (S7 applies unchanged). An `unknown` recipient needs the admin's explicit consent that the mail may arrive twice. Only to a configured target. The retry window stays the one counted from the mail's arrival. | F |
| H13 | Close and resend (D55) are for UI accounts and `admin` API tokens (`read`: `403`), at `POST /v1/mails/{id}/targets/{target}/close` and `/resend` with a JSON body ([api.md](api.md)) and the UI forms (U25). Both are refused (`409`) while any recipient of the delivery is `sending`, and checked in the transaction that makes the change, so a recipient is never in two attempts at once and a second resend finds nothing to queue. Each changed recipient gets one event, `delivery.closed` or `delivery.resend`, with `actor` and `via=ui\|api`, in that transaction; for the API it takes the place of `api.request`. | F |

## UI and admin listener

Details and reasons: [ui.md](ui.md), decisions D29, D35, D36 and D37.

| ID | Requirement | Phase |
|---|---|---|
| U1 | A separate admin listener `POSTWERK_ADMIN_LISTEN` (default `127.0.0.1:8081`) serves the UI; it is never the health listener (the query API has its own, H4). `off` or empty turns it off. | F |
| U2 | The admin listener must be a loopback address (`127.0.0.0/8`, `::1`, `localhost`). Another address is a configuration error unless `POSTWERK_ADMIN_ALLOW_NON_LOOPBACK=true` (for Docker port publishing); then a warning is logged at every start. | F |
| U3 | No Service port, Ingress or NetworkPolicy rule for the admin listener; people reach it with `kubectl port-forward` or an SSH tunnel. | F |
| U4 | `POSTWERK_UI_TOKENS_FILE` (optional, U23): `name:admin:<PBKDF2 hash>` per line; a `read` line is a configuration error. `postwerk gen-token <name>` prints a random 32-byte base64url token and its line. | F |
| U5 | Login by `POST` with name and password or token; every check costs one PBKDF2 at the same count for file accounts, database accounts and unknown names (as I12); at most `GOMAXPROCS` checks at once, 5 s wait for a slot, then a fixed "try again" reply (as I13). | F |
| U6 | Server-side sessions in memory: 32 random bytes, idle timeout 30 min, absolute 8 h, new id at login, deleted at logout. Cookie `HttpOnly; SameSite=Strict; Path=/`, `Secure` only over TLS. | F |
| U7 | Requests whose `Host` is not `localhost`, `127.0.0.1` or `[::1]` (any port) or a name in `POSTWERK_ADMIN_HOSTS` get `403` (DNS rebinding). | F |
| U8 | Every `POST` form carries a CSRF token (session-bound; on the login form bound to a short-lived cookie); missing or wrong → `403`. Cross-site `POST`s are refused by `Sec-Fetch-Site`/`Origin` as well. | F |
| U9 | Every response carries `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`, `X-Frame-Options: DENY`. No JavaScript. | F |
| U10 | Pages: overview (per target: up or down since when and why (D19), optional or not, last 24 h by recipient status, queued count, age of the oldest queued; recent `failed`/`unknown`/`partial` mails), mail list (filters state, target, time range; pagination), search, mail detail (record, envelope parameters, per target and recipient status, event timeline). | F |
| U11 | Search by mail id, `X-Postwerk-Ref`, `Message-ID` or recipient address, by `POST` only; addresses are found after pseudonymization through K3. The query never appears in a URL, log line or event. | F |
| U12 | One kind of account, admin (D35): addresses in full, "pseudonymized" after S8. | F |
| U13 | Events `ui.login`, `ui.login_failed`, `ui.login_throttled`, `ui.search`, `ui.view` and the account events name the account in a new `actor` field (on a failed login only if the name is an account); never the query, an address or a password. | F |
| U14 | No message content on any page. Mail records change only by an admin's close or resend (H11–H13, U25). | F |
| U15 | Read logic lives in `internal/query` on top of the store (SQL stays in the store), so the JSON API (H2) reuses it, masking included. | F |
| U16 | At most 30 searches per minute per account; more get `429`. | F |
| U17 | Accounts in the database (`ui_accounts`): name, PBKDF2 hash as for senders (D15), created and last changed with who did it; never the password. Pages to list and create accounts, set a password and delete. | F |
| U18 | Every account change needs the acting account's current password, is a `POST` with a CSRF token and writes its event (`ui.account_created`, `ui.password_set`, `ui.account_deleted`, `actor` and the account's name) in the same transaction. Every account can change its own password. | F |
| U19 | Passwords: 15 to 128 Unicode code points, no composition rules, not the account name, typed twice (NIST SP 800-63B-4, D36). | F |
| U20 | The last account in the database cannot be deleted. | F |
| U21 | Setting a password or deleting an account ends all its sessions at once. | F |
| U22 | Failed password checks are throttled in memory: per name 5 free, then 1 minute doubling to 15 minutes, forgotten after 24 h or a good login; per source address 20 free, forgotten after 1 h. Unknown names alike. A waiting login gets `429` without a check; `ui.login_throttled` when a wait starts. | F |
| U23 | The tokens file is start and break-glass access: optional, its accounts shown as "tokens file" and not editable, it wins on a name clash (warning at start). Without the file and without a database account, Postwerk does not start (D37). | F |
| U24 | Opening a mail's page writes `ui.view` (`actor`, `mail_id`) before the page is shown; the mail's timeline leaves out events with an `actor`, except an admin's `delivery.closed` and `delivery.resend`. | F |
| U25 | The mail page offers close and resend per target (H11–H12) as `POST` forms with the CSRF token and the acting account's current password (as U18), a confirmation text, and for an `unknown` recipient a required tick that the mail may arrive twice. An action that is not possible is shown disabled with its reason; a target whose recipients are all final shows none. | F |

## Operations and supply chain

| ID | Requirement | Phase |
|---|---|---|
| O1 | Configuration only through environment variables and secret files; invalid configuration stops the start with all errors listed. Keys only from files. Reference: [configuration.md](configuration.md). | F |
| O2 | Static binary, no cgo. Container from `scratch` with CA certificates only, runs as a non-root user, works with a read-only root file system. | F |
| O3 | Logs as JSON on stdout, never with message content or recipient addresses. | F |
| O4 | Direct dependencies kept to the SMTP library, its SASL package and the SQLite driver for the foundation. Every new dependency needs a reason in decisions.md. | F |
| O5 | CI: format check, vet, tests with the race detector, vulnerability scan, container build, Helm chart lint, render and schema check. | F |
| O6 | Helm chart `deploy/helm/postwerk`: one replica with `Recreate`, ClusterIP Service only (no Ingress, `LoadBalancer` or `NodePort`), PVC, existing Secrets mounted as files (`0440`, `fsGroup` 65532), non-root uid/gid 65532, read-only root file system, all capabilities dropped, seccomp `RuntimeDefault`, no service account token, no service links, `hostNetwork`, `hostPID` and `hostIPC` set to `false` explicitly, probes on `/healthz` and `/readyz`, grace period 120 s, resource requests and limits, every retention setting as a value with its recommended default. Guide: [kubernetes.md](kubernetes.md). | F |
| O7 | NetworkPolicy on by default: SMTP only from configured peers, HTTP only from configured sources (probes come from the node), egress only DNS and the target ports 587/465 (any address by default, written out as `0.0.0.0/0` and `::/0`; configurable CIDRs; a rule without peers is refused, D59). Optional cert-manager `Certificate` for STARTTLS. The UI listens on the pod's loopback, with the tokens file from an existing Secret (optional once accounts exist), and has no Service port or policy rule (U3). The query API is off by default; with `api.enabled` it gets a Service port, TLS by default and an ingress rule only from `api.allowFrom` (empty: nobody). | F |
| O8 | Release: a tag `v<semver>` builds `ghcr.io/eneuberg/postwerk:<semver>` for `linux/amd64` and `linux/arm64` (cross-compiled, no emulation), with an SBOM and BuildKit provenance, signs the index digest keyless with cosign (GitHub OIDC) and verifies the signature. No `latest` tag. The tag must match the chart's `appVersion`. Actions pinned by commit, job permissions `contents: read`, `packages: write`, `id-token: write` only. | F |
| O9 | The Helm chart defaults to `ghcr.io/eneuberg/postwerk` with the chart's `appVersion` as tag; `image.repository`, `image.tag` and `image.digest` override it. Signature check documented with the exact identity and issuer. | F |
