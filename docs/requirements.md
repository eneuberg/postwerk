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
| I5 | Limits: message size, recipients per message, line length, read/write timeouts, concurrent connections (`POSTWERK_MAX_CONNECTIONS`, default 100; a connection over the limit gets a fixed `421` and is closed). | F |
| I6 | `250` is sent only after the mail and its deliveries are committed with `synchronous=FULL`. The reply names the id: `250 2.0.0 queued as pw_…`. | F |
| I7 | If storing fails, reply `451` (temporary) and keep nothing half-written. Internal error texts never reach the client. | F |
| I8 | `X-Postwerk-*` headers set by the sender are read (`Ref`, `Skip`) and removed before storage. *(safe default, proposed; scope gap 7)* | F |
| I9 | An invalid `X-Postwerk-Ref` (contains `@`, is longer than 256 bytes or not printable ASCII) is not stored; the mail is still accepted, without a ref. The header is removed either way. | F |

## Store

| ID | Requirement | Phase |
|---|---|---|
| S1 | One SQLite file, WAL, `synchronous=FULL`, foreign keys on, one connection (single writer). | F |
| S2 | Tables `mails`, `recipients`, `deliveries`, `events`; schema versioned with `user_version`, migrations embedded. | F |
| S3 | `events` is append-only, enforced by triggers: no update ever; a delete only for events older than the retention cutoff (S10). | F |
| S4 | State change and its event are written in the same transaction. | F |
| S5 | No message content and no recipient address in `events`. SMTP reply texts are stored with addresses redacted. | F |
| S6 | Retention job: runs at start (before any delivery) and then every 10 minutes; each run that changed something writes one `retention.purged` event with counts only. | F |
| S7 | Raw message removal: once every delivery is `handed_off` or `skipped`, after `POSTWERK_RAW_KEEP_AFTER_HANDOFF` (default 0: in the same transaction that makes the last delivery final); once every delivery is final with one `failed` or `unknown`, after `POSTWERK_RAW_KEEP_AFTER_FAILURE` (72 h); in any state after `POSTWERK_RAW_MAX_AGE` (7 d). A delivery still `queued` or `unknown` at removal becomes `failed` with `retention.raw_expired` (`error`, `warn` for optional targets). Every retry window must be shorter than the maximum age. | F |
| S8 | Pseudonymization: after `POSTWERK_PSEUDONYMIZE_AFTER` (30 d), recipient addresses and the envelope sender are replaced by keyed hashes (K1), once no delivery may still be sent. | F |
| S9 | Record retention: `mails`, `recipients` and `deliveries` rows are deleted after `POSTWERK_RECORD_RETENTION` (90 d), except mails with a delivery still `queued`, `sending` or `unknown`. | F |
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
| D1 | One delivery per mail and target, each with its own status, attempts and next attempt time. | F |
| D2 | One worker per target; a slow or broken target never delays another. | F |
| D3 | Retry policy as in design.md section 5: `4xx` and network errors retry; `535` retries and raises an error at once; other `5xx` fail. | F |
| D4 | Backoff 30 s, 1 min, 2 min, 5 min, 10 min, 30 min, then hourly, until the retry window ends; then `failed`. | F |
| D5 | The moment before the message body is sent is recorded (`data_sent_at`). A connection error after that is an unclear outcome, handled by the target's `ON_UNKNOWN` policy (`hold`, `retry`; `check` needs a status adapter). | F |
| D6 | On start, deliveries left in `sending` are recovered: before `data_sent_at` → queued again; after → unclear outcome. | F |
| D7 | TLS towards targets: `smtps://` (implicit TLS) or `smtp://` (STARTTLS required). Cleartext only with the explicit scheme `smtp+plain://`, and never with credentials. *(safe default, proposed; scope gap 4)* | F |
| D8 | Per-target recipient allow-list, all or nothing per target: if one recipient is outside the list, the whole delivery to that target is skipped (`delivery.not_allowed`, warning). Nothing is rejected at `RCPT`; other targets still get the mail. *(safe default, proposed; scope gap 5)* | F |
| D9 | `X-Postwerk-Skip` excludes named targets for one mail. | F |
| D10 | Postwerk adds `X-Postwerk-Id` and, for Mailjet targets, `X-MJ-CustomID`; for Mailjet targets any `X-MJ-CustomID` set by the sender is removed first, so tracking is unambiguous. | F |
| D11 | Graceful shutdown: stop accepting, let in-flight sends finish within a deadline, then close the store. One attempt is capped at 90 s (dial 15 s, command 30 s, reply after the message 60 s); the default deadline `POSTWERK_SHUTDOWN_TIMEOUT` is 100 s, longer than the cap; the orchestrator's grace period must be longer still (120 s). *(safe default, proposed; scope gap 3)* | F |
| D12 | Rate and connection limits per target. | L |

## Status, audit, reporting

| ID | Requirement | Phase |
|---|---|---|
| A1 | Every state change is an event with `seq`, time, mail id, target, kind, level. | F |
| A2 | Reporter reads events by cursor per sink; a sink that fails retries later and catches up. | F |
| A3 | stdout sink: one JSON line per event, always on. | F |
| A4 | Sinks for OpenTelemetry, Sentry protocol, signed webhook. | L |
| A5 | Provider status adapter (Mailjet), and the `check` policy for unclear outcomes. | L |

## HTTP

| ID | Requirement | Phase |
|---|---|---|
| H1 | Separate HTTP listener, default `127.0.0.1:8080`. `/healthz` (alive) and `/readyz` (store reachable, workers running). | F |
| H2 | Read-only query API with bearer tokens (`read`, `admin`), token use logged; resend and close for admins. | L |
| H3 | Small read-only UI, server-rendered, no content. | L |

## Operations and supply chain

| ID | Requirement | Phase |
|---|---|---|
| O1 | Configuration only through environment variables and secret files; invalid configuration stops the start with all errors listed. Keys only from files. Reference: [configuration.md](configuration.md). | F |
| O2 | Static binary, no cgo. Container from `scratch` with CA certificates only, runs as a non-root user, works with a read-only root file system. | F |
| O3 | Logs as JSON on stdout, never with message content or recipient addresses. | F |
| O4 | Direct dependencies kept to the SMTP library, its SASL package and the SQLite driver for the foundation. Every new dependency needs a reason in decisions.md. | F |
| O5 | CI: format check, vet, tests with the race detector, vulnerability scan, container build. | F |
