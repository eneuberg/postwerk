# Code architecture

How the code is laid out. The *what* and *why* are in [design.md](design.md) and [requirements.md](requirements.md);
this page is the map for people reading or changing the code.

## Packages

```
cmd/postwerk          main: parse config, open store, start ingress, workers, reporter, retention, HTTP; handle signals
internal/config       environment + secret files → validated Config (O1); reference in configuration.md
internal/id           mail ids: "pw_" + 26 chars, time-ordered, crypto random
internal/message      header handling: read Ref/Skip/Message-ID, strip X-Postwerk-*, prepend own headers (I8, D10)
internal/redact       remove mail addresses from free text such as SMTP replies (S5)
internal/auth         sender password hashes and verification (I2)
internal/keys         key file format (<key-id>:<base64>), active key + old keys, gen-key (E5)
internal/seal         AES-256-GCM sealing of the stored message, keyed by the key id in each blob (E1–E4)
internal/addrhash     address normalization and HMAC-SHA256 keyed hashes, lookups over all keys (K1–K3)
internal/store        SQLite: open, migrate, all reads and writes, retention steps; the only package that speaks SQL (S1–S12)
internal/ingress      SMTP server session: AUTH, limits, seal, store-then-250 (I1–I9)
internal/delivery     one worker per target: claim, open the seal, send, classify per recipient, schedule, recover (D1–D16)
internal/retention    runs the store's retention at start and every 10 minutes, logs counts (S6)
internal/report       event cursor loop and sinks (A2, A3)
internal/httpapi      health endpoints now; query API and UI later (H1)
internal/testutil     helpers for tests only (self-signed certificates, polling, buffers); never imported by code
```

Dependency rule: `store` depends on nothing internal except `redact`, so every free text is redacted where it is
written. The store holds no keys: messages reach it already sealed, and the retention step that pseudonymizes takes
the hash function as a parameter. `ingress`, `delivery`, `retention` and `report` depend on `store` and the small helper
packages (`config` for the `Target` type, `auth`, `id`, `message`, `redact`, `seal`), never on each other. `seal` and
`addrhash` depend only on `keys`. Only `cmd/postwerk` wires them together. This keeps every piece testable alone and
makes the data flow visible: everything meets in the store.

## Data flow

```
sender ──SMTP──▶ ingress ──seal(raw)── tx: mail + recipients + deliveries (+ status per recipient) + events ──▶ store
                                                                                        │
     target ◀──SMTP── delivery worker (per target) ◀── claim due, open(raw) ────────────┤
                            └── tx: status + event per recipient (+ raw removed at last hand-off) ▶│
                                                                                        │
                  retention (start, every 10 min) ── tx batches: purge raw, ───────────▶│
                                                     pseudonymize, delete rows/events,  │
                                                     retention.purged; WAL checkpoint   │
                                                                                        │
     stdout ◀── report (cursor per sink) ◀── events after cursor ───────────────────────┘
```

Ingress wakes the workers through a non-blocking signal after each commit; workers also poll on a timer, so a missed
signal only delays, never loses a mail. The store calls a hook after every commit that added events; the reporter uses
it the same way.

## Store

One `*sql.DB` with a single open connection. All access is serialized; at transactional volumes this is simpler and
safer than juggling readers and a writer, and it avoids `SQLITE_BUSY` entirely. Pragmas per connection: `journal_mode=WAL`,
`synchronous=FULL`, `foreign_keys=ON`, `busy_timeout`.

Tables (version 4):

| Table | Holds | Notes |
|---|---|---|
| `mails` | id, received time, sender login, envelope from, Message-ID, ref, size (of the plaintext), raw message as a sealed blob, `raw_purged_at`, `pseudonymized_at`, the declared MAIL parameters `body`, `smtputf8`, `declared_size` | raw is set to NULL by retention (S7); envelope from becomes a keyed hash (S8) |
| `recipients` | mail id, position, address | separate so addresses can be pseudonymized or erased without touching anything else; an address becomes `<key-id>:<hex>` (S8) |
| `deliveries` | mail id, target, attempts, next attempt, `data_sent_at`, last code and redacted text of the attempt, updated time | what one SMTP transaction shares; primary key (mail id, target); next attempt is set while a recipient is queued; partial index on (target, next attempt) |
| `delivery_recipients` | mail id, target, position, **status**, last code and redacted text, updated time | the unit of tracking: one row per (mail, target, recipient); its status is the single source of truth |
| `events` | seq, time, mail id, target, recipient position, kind, level, attempt, code, redacted text, sender login | append-only by trigger; no addresses; `seq` is `AUTOINCREMENT`, so it is never reused |
| `cursors` | sink name, last seq | one row per sink |
| `event_retention` | one row: `cutoff` (Unix ms) | events older than the cutoff may be deleted; triggers refuse lowering it, deleting the row or inserting another (S10) |

Migrations: `0001_init.sql` creates the tables above except `event_retention`, with a trigger that refuses every update
and delete on `events`. `0002_retention.sql` adds `raw_purged_at`, `pseudonymized_at`, the `event_retention` row with
its triggers, indexes for retention (`mails.received_at`, partial indexes for rows that still hold raw or plain
addresses, `events.time`), and replaces the delete trigger on `events` by one that allows only deletes below the
cutoff (a missing row counts as cutoff 0). The update trigger stays. Databases written by builds before version 2
stored the raw message without a blob header; they are not supported (there was no release). `0003_envelope.sql` adds
the MAIL parameters as the sender declared them: `body` (`7BIT`, `8BITMIME` or NULL), `smtputf8` (0 or 1) and
`declared_size` (NULL if not given). `0004_recipient_status.sql` adds `delivery_recipients`, fills it with each
delivery's status for every recipient, drops `deliveries.status` (and the unused provider columns; provider status will
be kept per recipient) and adds `events.recipient`.

All tables are `STRICT`. The schema lives in `internal/store/migrations/NNNN_name.sql`, embedded in the binary; a
migration runs in its own transaction together with `PRAGMA user_version = N`. A database with a newer version than the
binary knows is refused. Besides the pragmas above, the store sets `secure_delete=ON` (deleted content is overwritten;
`POSTWERK_SECURE_DELETE=off` turns it off) and `temp_store=MEMORY` (no temporary files, so a read-only root file system is enough), and checks after opening that
WAL, `synchronous=FULL`, foreign keys and the configured `secure_delete` are really in effect. The database file is created with mode
`0600`.

**Status.** The unit of tracking is the triple (mail, target, recipient). Its status is stored in
`delivery_recipients` and is the only stored status: `queued`, `sending`, `handed_off`, `failed`, `unknown`,
`skipped` (later, provider statuses such as delivered or bounced per recipient, which matches Mailjet's one message
per recipient). Scheduling and attempts stay per (mail, target), because one SMTP transaction carries several
recipients. The status of a delivery, or of a mail over all its targets, is a summary computed when reading
(`store.Summarize`), never stored:

| Recipients | Summary |
|---|---|
| any `sending` | `sending` |
| any `queued` | `queued` |
| any `unknown` | `unknown` |
| all `skipped` | `skipped` |
| all others `handed_off` | `handed_off` |
| none `handed_off` | `failed` |
| some `handed_off`, the others `failed` | `partial` |

Event kinds. Every result is an event per recipient: it carries the recipient's position, never its address.
`mail.accepted`, `delivery.attempt` and `retention.purged` carry none.

| Kind | Level | Written when |
|---|---|---|
| `mail.accepted` | info | a mail is stored |
| `delivery.attempt` | info | a worker claims a delivery (one per attempt, no recipient) |
| `delivery.retry` | warn | a temporary failure for this recipient (`4xx` at `RCPT` or for the attempt); queued again |
| `delivery.handed_off` | info | the target answered `250` after the message; one per recipient of the attempt |
| `delivery.failed` | error (warn: optional target) | a permanent failure for this recipient (`5xx` at `RCPT` or after the message), or the retry window is over |
| `delivery.unknown` | error (warn: optional) | no reply after the message was sent, policy `hold` or `check`; for the recipients of that attempt |
| `delivery.skipped` | info | the sender named the target in `X-Postwerk-Skip`; one per recipient |
| `delivery.not_allowed` | warn | a recipient is outside the target's allow-list; the whole target is skipped, one event per recipient |
| `delivery.unsupported` | error (warn: optional) | the mail (or this recipient's address) needs `SMTPUTF8` or `8BITMIME` and the target does not offer it; nothing was sent |
| `delivery.too_large` | error (warn: optional) | the message as Postwerk would send it is larger than the `SIZE` the target offers; nothing was sent |
| `target.auth_failed` | error (warn: optional) | the target answered `535`; queued again |
| `target.unreachable` | warn | no reply before the message was sent (connection, TLS or protocol error) |
| `retention.raw_expired` | error (warn: optional) | the stored message was removed while this recipient was `queued` or `unknown`; it is now `failed` |
| `retention.purged` | info | a retention run changed something; text holds counts only (`raw_purged=… raw_expired=… pseudonymized=… records_deleted=… events_deleted=…`) |
| `store.decrypt_failed` | error | the stored message could not be decrypted (unknown key id, failed authentication); the delivery is `failed`, nothing was sent |

The overall state of a delivery or a mail is derived from its recipients; it is not stored twice.

Times are stored as Unix milliseconds (UTC).

## Delivery worker

```
loop:
  claim the next due delivery of this target  (tx: queued recipients → sending, attempts+1, event delivery.attempt)
  connect (TLS per scheme), EHLO, check extensions, AUTH, MAIL (+ parameters)
  RCPT per recipient; collect the refused ones (none accepted → no DATA)
  DATA; target answers 354 → record data_sent_at and the refused recipients   (tx)
  own headers + stored message, final dot
  read the reply → classify → next status per recipient   (tx: status + event per recipient, next attempt)
  nothing due → wait for signal, the next due time, or the poll interval
```

Classification. RFC 5321 relay behaviour: the target answers each `RCPT`, `DATA` goes to the accepted recipients,
and every result is per recipient.

| Outcome | Applies to | Before `data_sent_at` | After `data_sent_at` |
|---|---|---|---|
| reply `250` after the message | the accepted recipients of the attempt | — | `handed_off` |
| `5xx` to `RCPT` | that recipient | `failed` | — |
| `4xx` to `RCPT`, or `552` (too many recipients, RFC 5321 4.5.3.1.10) | that recipient | queued, backoff; only queued recipients go into the next attempt | — |
| reply `535` | every recipient of the attempt | queued, backoff, event `target.auth_failed` (error) | same |
| reply `4xx` to `MAIL`, `DATA` or after the message | the recipients of the attempt not refused at `RCPT` | queued, backoff | queued, backoff (the server answered, so the outcome is clear) |
| reply `5xx` to `MAIL`, `DATA` or after the message | the same | `failed` | `failed` |
| no reply (network error, timeout, crash) | the same | queued, backoff | unclear: `hold` → `unknown`; `retry` → queued |
| every recipient refused at `RCPT` | — | no `DATA`; each recipient has its own result | — |
| retry window over | the recipients that would be queued | `failed` | `failed` |

The derived status of the delivery follows from the recipients (table above): `queued` while any recipient is queued,
`handed_off` if all were handed off, `partial` if some were handed off and the others failed, `failed` if none was
handed off. The recipients refused at `RCPT` are written in the same transaction as `data_sent_at`, so a crash during
the message makes the outcome unclear only for the recipients that were accepted.

`data_sent_at` is written after the target's `354` reply to `DATA` and before the first byte of the message. A failure
of the `DATA` command itself is therefore still "before": the target cannot have accepted a message it never received.
If writing `data_sent_at` fails, the message is not sent at all.

A reply code outside 2xx/4xx/5xx counts as "no reply". `check` behaves like `hold` until the status adapter exists
(phase L); its event says that the check is pending. The last attempt is moved to the end of the retry window, so the
whole window is used. Levels: `failed`, `unknown` and `target.auth_failed` are `error`, or `warn` for an optional
target; retries are `warn`.

Recovery at start (D6): recipients left in `sending` move to `queued` (no `data_sent_at`) or through the target's
`ON_UNKNOWN` policy (with `data_sent_at`). `cmd/postwerk` runs this for every target (`Manager.Recover`) before the
first retention run, so that retention sees the recovered state and a mail past `RAW_MAX_AGE` is expired, not sent;
each worker repeats it when it starts, in case the first try failed. Deliveries for targets that are no longer
configured are left as they are and logged at start.

A shutdown never interrupts a send in progress; the worker finishes it and then stops. The send runs on a context
without cancellation and is bounded by the SMTP timeouts: dial 15 s, each command 30 s, the reply after the message
60 s, and a cap of 90 s for the whole attempt including the message body.

Shutdown in `cmd/postwerk`, all under one deadline (`POSTWERK_SHUTDOWN_TIMEOUT`, default 100 s): the workers stop
claiming at once and finish their current send (at most 90 s); at the same time the SMTP listener closes and open
sessions get at most half of the deadline, then they are closed (mails accepted meanwhile stay queued for the next
start). Then the remaining events are exported, HTTP stops and the store closes. Because the deadline is longer than
the attempt cap, a send in progress always ends normally; a shorter configured deadline logs a warning. The
orchestrator's grace period must be longer than the deadline (120 s).

**Envelope parameters.** Postwerk re-issues `MAIL` and `RCPT` itself, so it passes on what the sender declared
(`store.Params`). After `EHLO` the worker checks the target's extensions before it logs in:

| Mail | Target offers it | Target lacks it |
|---|---|---|
| declared `SMTPUTF8`, non-ASCII in the envelope or the header section | `SMTPUTF8` passed on | `failed`, `delivery.unsupported`, nothing sent |
| declared `SMTPUTF8`, all ASCII | `SMTPUTF8` passed on | sent without it (nothing is lost) |
| any byte outside 7-bit ASCII in the message | `BODY=8BITMIME` | `failed`, `delivery.unsupported`, nothing sent |
| 7-bit message | `BODY=8BITMIME` (set by the SMTP library for every target that offers it; 7-bit content is valid 8BITMIME) | sent without `BODY=` |

**Size.** The worker computes the exact size it will send (its own headers plus the stored message, without the
sender's `X-MJ-CustomID` for Mailjet targets) and sends it as `SIZE=` to a target that offers `SIZE`. If the target
names a limit and the message is larger, the delivery fails with `delivery.too_large` before `AUTH` and `MAIL`; like
`delivery.unsupported`, this is final.

Many clients declare `SMTPUTF8` whenever a server offers it (Go's `net/smtp` does), which is why a pure ASCII mail
does not need it. `delivery.unsupported` is final: the target's capabilities do not change between attempts.

Before a send, the worker opens the sealed message. If retention removed it, the claim returns no message and the
delivery fails with `retention.raw_expired`; if it cannot be decrypted, it fails with `store.decrypt_failed`. In both
cases nothing is sent. For a Mailjet target, any `X-MJ-CustomID` the sender set is removed before Postwerk adds its own.

## Encryption at rest

`internal/seal` encrypts the message after header processing and before it is stored. A blob is
`0x01 | len(key id) | key id | nonce (12) | ciphertext + tag`, AES-256-GCM directly with the active store key, with the
blob header and the mail id as additional authenticated data. With encryption off the blob is `0x00 | message`; such a
blob is refused while encryption is on, so a damaged version byte can never turn ciphertext into a "message" that is
sent.

Why no per-mail data key (envelope encryption, as privacy-and-security.md section 6.2 suggests): a wrapped data key
helps when a key-encryption key must be rotated without rewriting large amounts of long-lived data, or when keys sit in
a KMS that should only see small payloads. Neither applies. Stored messages live minutes to days (`RAW_MAX_AGE`, 7 d),
the key id in every blob already lets new mails use a new key while old ones are read with the old key, and an old key
can be removed once the maximum age has passed. A second layer would add code and a second failure mode without
protecting anything more. Random 96-bit nonces are safe for far more messages than one key will ever see.

Keys come from files only (`internal/keys`), read once at start. A `Key` prints as its id, never its bytes.

## Retention

`internal/retention` calls `store.Retain` at start, after the recovery of interrupted deliveries and before the workers
start, and then every 10 minutes. The steps run
in order, each in transactions of at most 200 rows so ingress and workers never wait long for the single connection:

All steps look at the recipient statuses: the message stays while any recipient of any target is `queued`,
`sending` or `unknown` (within the limits below).

1. Remove the message of mails whose recipients are all `handed_off`/`skipped`, `RAW_KEEP_AFTER_HANDOFF` after the last
   change. With the default 0 this already happened in `Finish` (or at `Accept`, when every delivery is skipped).
2. Remove the message of mails whose recipients are all final with one `failed`/`unknown`, `RAW_KEEP_AFTER_FAILURE`
   after the last change; `unknown` becomes `failed` with `retention.raw_expired`.
3. Remove the message of mails older than `RAW_MAX_AGE`, skipping mails with a recipient in `sending`; `queued` and
   `unknown` recipients become `failed`, each with `retention.raw_expired`.
4. Replace recipient addresses and the envelope sender by keyed hashes (`addrhash.Hasher.Hash`) for mails older than
   `PSEUDONYMIZE_AFTER` that have no message and no recipient that may still be sent.
5. Delete mails older than `RECORD_RETENTION` with no recipient in `queued`, `sending` or `unknown`; recipients,
   deliveries and their recipient statuses go with them (`ON DELETE CASCADE`). Events stay.
6. Raise the event cutoff to `now − EVENT_RETENTION` (never lower) and delete events below it.
7. One `retention.purged` event with the counts (`raw_expired` counts recipients), if anything changed; then `PRAGMA wal_checkpoint(TRUNCATE)`.

Lookups by address (`store.MailIDsByAddress`) take the normalized query and its hashes under every key
(`addrhash.Hasher.Lookup`), so they find plain and pseudonymized rows alike. The query API (H2) and erasure requests
will use it.

## Errors and logging

- The SMTP client on the other side only ever sees fixed reply texts chosen by Postwerk. Every session method
  returns a fixed `*smtp.SMTPError`; the SMTP library would otherwise send the text of any other error to the client.
- Every free text that is stored or exported (SMTP replies, network errors) goes through `redact`: addresses become
  `<redacted>`, control characters are removed, the length is capped at 300 bytes.
- Logs are JSON (`log/slog`) and carry ids, targets, codes and redacted texts. They never carry content or addresses.
- Configuration errors are collected and printed together; the process exits with status 2.

## Testing

- Unit tests per package.
- End-to-end tests start Postwerk's ingress and workers in-process against fake SMTP targets built with the same SMTP
  library, scripted to answer `250`, `4xx`, `535`, `5xx` or to drop the connection after DATA.
- Recovery tests write a store in a crashed state and start the workers on it.
- Every end-to-end test sends to a canary address and checks at the end that it appears nowhere in logs, exported
  events, the events table or delivery reply texts (leak test). The end-to-end harness runs with encryption on. One
  test also runs retention with tiny periods and then searches the database file and the WAL file byte by byte: the
  message body is never there in plaintext, and after pseudonymization neither are the recipient or the envelope
  sender; only hashes remain, and the mail is still found by address.
- TLS tests use a self-signed certificate made in the test; the delivery package takes the trusted roots through an
  unexported option, so there is no "skip verification" setting.
