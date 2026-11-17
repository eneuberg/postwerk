# Code architecture

How the code is laid out. The *what* and *why* are in [design.md](design.md) and [requirements.md](requirements.md);
this page is the map for people reading or changing the code.

## Packages

```
cmd/postwerk          main: parse config, open HTTP, open store (data lock), start ingress, workers, reporter, retention, admin; handle signals
internal/config       environment + secret files → validated Config (O1); reference in configuration.md
internal/id           mail ids: "pw_" + 26 chars, time-ordered, crypto random
internal/message      header handling: read Ref/Skip/Message-ID, strip X-Postwerk-*, prepend own headers (I8, D10)
internal/redact       remove mail addresses from free text such as SMTP replies (S5)
internal/auth         sender password hashes, UI tokens (name:admin:hash), UI password rules and the equal-cost checker (I2, I12, U4, U19)
internal/keys         key file format (<key-id>:<base64>), active key + old keys, gen-key (E5)
internal/seal         AES-256-GCM sealing of the stored message, keyed by the key id in each blob (E1–E4)
internal/addrhash     address normalization and HMAC-SHA256 keyed hashes, lookups over all keys (K1–K3)
internal/store        SQLite: data lock, open, migrate, all reads and writes, retention steps; the only package that speaks SQL (S1–S12, D61)
internal/ingress      SMTP server session: AUTH, limits, seal, store-then-250 (I1–I9)
internal/tlsreload    TLS certificate from files, re-read every minute, validated before it is served (I16)
internal/delivery     one worker per target: claim, open the seal, send, classify per recipient, schedule, recover (D1–D16)
internal/retention    runs the store's retention at start and every 10 minutes, logs counts (S6)
internal/report       event cursor loop per sink, the stdout and Sentry-protocol sinks (A2, A3, A6–A9)
internal/httpapi      health endpoints on the health listener (H1)
internal/watch        Postwerk's own signals: free space of the data volume, age of each target's queue (H10, A10, A11)
internal/fold         counts repeated occurrences, one record per key and window (the API's and ingress's refusal events)
internal/query        read logic for the UI and the JSON API: overview, lists, search, detail, address masking (U10–U12, U15)
internal/resolve      the admin actions on a delivery for the UI and the JSON API: close and resend, their input and availability (H11–H13)
internal/ui           the internal web UI on the admin listener: login and throttling, sessions, CSRF, Host check, pages, accounts, close and resend forms (H3, U1–U25)
internal/api          the JSON query API on its own listener: bearer tokens, limits, events, JSON, close and resend (H2, H4–H13)
internal/testutil     helpers for tests only (self-signed certificates, polling, buffers); never imported by code
```

Dependency rule: `store` depends on nothing internal except `redact`, so every free text is redacted where it is
written. The store holds no keys: messages reach it already sealed, and the retention step that pseudonymizes takes
the hash function as a parameter. `ingress`, `delivery`, `retention` and `report` depend on `store` and the small helper
packages (`config` for the `Target` and `SentryDSN` types, `auth`, `id`, `message`, `redact`, `seal`, `fold`), never on each other. `watch` depends on `store` only; ingress asks it for space through a one-method interface. `seal` and
`addrhash` depend only on `keys`. `query` reads through `store` (and `addrhash` for lookups); `resolve` writes
through `store` and reads `query`'s delivery detail; `ui` depends on `query`, `resolve`, `auth` and the store's types,
never on `ingress` or `delivery` (a resend wakes the workers through a function `cmd/postwerk` passes in); so does `api`, and `api` never depends on `ui`. Only `cmd/postwerk` wires them together. This keeps every piece testable alone and
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
     stdout ◀── report (cursor and loop per sink) ◀── events after cursor ──────────────┤
     Sentry ◀──HTTPS──┘                                                                 │
                                                                                        │
  person ──tunnel──▶ admin listener (127.0.0.1:8081) ── ui ── query ── reads ──────────▶│
                                                         ├── tx: ui.* events, ui_accounts ────▶│
                                                         └── resolve ── tx: close / resend ───▶│
                                                                  (+ events, raw removed at close; wake workers)
  other pod ──▶ API listener (POSTWERK_API_LISTEN) ── api ── query ── reads ───────────▶│
                                                       ├── tx: api.request / api.view events ▶│
                                                       └── resolve ── tx: close / resend ─────▶│
  watch (disk every 10 s and at MAIL/RCPT/DATA, queues every minute) ── statfs, queue ages ─▶│
                                                       └── tx: disk.* / queue.* events ────────▶┘
```

Ingress wakes the workers through a non-blocking signal after each commit; workers also poll on a timer, so a missed
signal only delays, never loses a mail. The store calls a hook after every commit that added events; the reporter uses
it the same way.

## Store

One `*sql.DB` with a single open connection. All access is serialized; at transactional volumes this is simpler and
safer than juggling readers and a writer, and it avoids `SQLITE_BUSY` entirely. Pragmas per connection: `journal_mode=WAL`,
`synchronous=FULL`, `foreign_keys=ON`, `busy_timeout`.

**One process (D61).** The single connection serializes one process; nothing in SQLite stops a second one from
running its own workers on the same file. `store.Open` therefore first takes `flock(LOCK_EX|LOCK_NB)` on
`<database>.lock` (`lock_unix.go`, through `golang.org/x/sys/unix`), and only then creates or opens the database. While
another open file holds the lock, in this process or another, it retries every 250 ms up to `Options.LockWait` and
calls `Options.OnLockWait` at once and then every 5 s, then returns `store.ErrLocked`; a cancelled context ends the
wait. The holder writes `host=… pid=… since=…` into the file, read only for those messages. `Close` closes the
database, then the lock file, which releases the lock; the kernel releases it too when the process ends, so there is
never a stale lock. Where `flock` is not built (`lock_other.go`, Windows and others), `store.LockSupported` is false,
Open does not wait and `cmd/postwerk` logs a warning. `cmd/postwerk` opens the health listener before the store:
`/healthz` answers during the wait, and `/readyz` fails until the store, the workers and the other listeners run.

Tables (version 7):

| Table | Holds | Notes |
|---|---|---|
| `mails` | id, received time, sender login, envelope from, Message-ID, ref, size (of the plaintext), raw message as a sealed blob, `raw_purged_at`, `pseudonymized_at`, the declared MAIL parameters `body`, `smtputf8`, `declared_size` | raw is set to NULL by retention (S7); envelope from becomes a keyed hash (S8) |
| `recipients` | mail id, position, address | separate so addresses can be pseudonymized or erased without touching anything else; an address becomes `<key-id>:<hex>` (S8) |
| `deliveries` | mail id, target, attempts, next attempt, `data_sent_at`, last code and redacted text of the attempt, updated time | what one SMTP transaction shares; primary key (mail id, target); next attempt is set while a recipient is queued; partial index on (target, next attempt) |
| `delivery_recipients` | mail id, target, position, **status**, last code and redacted text, updated time, `resolution` | the unit of tracking: one row per (mail, target, recipient); its status is the single source of truth; `resolution` (`confirmed`, `given_up` or NULL) is set when an admin closed it |
| `events` | seq, time, mail id, target, recipient position, kind, level, attempt, code, redacted text, sender login, actor (account name of a UI event) | append-only by trigger; no addresses; `seq` is `AUTOINCREMENT`, so it is never reused |
| `cursors` | sink name, last seq | one row per sink |
| `ui_accounts` | name, PBKDF2 hash, created and updated time and by whom | UI accounts managed in the panel (U17); never a password |
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
be kept per recipient) and adds `events.recipient`. `0005_ui.sql` adds partial indexes on `mails.ref` and
`mails.message_id` for lookups, and `events.actor`. `0006_ui_accounts.sql` adds `ui_accounts`. `0007_resolution.sql`
adds `delivery_recipients.resolution` with a check that it matches the status (`confirmed` only with `handed_off`,
`given_up` only with `failed`).

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

**Closed by an admin** (D53). Closing does not add a status. A recipient closed as `confirmed` becomes `handed_off`,
one closed as `given_up` becomes `failed`, and `resolution` records that a person decided it, not a reply from the
target. Every summary above, the list filters and the overview counts read the status only, so they stay as they are:
a confirmed `unknown` counts as handed off, a given-up one as failed. The UI and the API show the resolution next to
the status; who closed it, when and why is in its `delivery.closed` event. A closed recipient is final: retention's
raw expiry only touches `queued` and `unknown`, a claim only `queued`, and neither close nor resend takes a recipient
that has a resolution. The rules are `store.CloseCheck` and `store.ResendCheck`; the store applies them inside the
transaction that makes the change, and `internal/resolve` uses the same functions to show what is possible.

| Action | Takes the recipients of one delivery that are | Refused while | Result |
|---|---|---|---|
| close `confirmed` | `unknown`, or `failed` without a resolution | any is `sending`; any to close is `queued` | `handed_off` + `confirmed`; next attempt cleared; raw removed if nothing else needs it |
| close `given_up` | `queued`, `unknown`, or `failed` without a resolution | any is `sending` | `failed` + `given_up`; the same |
| resend | `unknown` (with the admin's consent to a duplicate), or `failed` without a resolution | any is `sending`; the message is gone | `queued`, due at once; the workers are woken |

"Nothing else needs it" means no recipient of the mail, at any target, is `queued`, `sending`, `unknown` or `failed`
without a resolution. `Finish` removes the message at the last hand-off when the others are handed off, skipped or
closed.

Event kinds. Every result is an event per recipient: it carries the recipient's position, never its address.
`mail.accepted`, `delivery.attempt` and `retention.purged` carry none; `target.down` and `target.recovered` carry no
mail id either.

| Kind | Level | Written when |
|---|---|---|
| `mail.accepted` | info | a mail is stored |
| `delivery.attempt` | info | a worker claims a delivery (one per attempt, no recipient) |
| `delivery.retry` | warn | a temporary failure for this recipient (`4xx` at `RCPT` or for the attempt, `535`, the target unreachable); queued again |
| `delivery.handed_off` | info | the target answered `250` after the message; one per recipient of the attempt |
| `delivery.failed` | error (warn: optional target) | a permanent failure for this recipient (`5xx` at `RCPT` or after the message), or the retry window is over (also while the target is down) |
| `delivery.rejected` | warn | the target refused this recipient's address for good: `550`, `551` or `553` with `5.1.x`/`5.2.x`, or without an enhanced code at `RCPT` (D46); `failed` |
| `delivery.unknown` | error (warn: optional) | no reply after the message was sent, policy `hold` or `check`; for the recipients of that attempt |
| `delivery.skipped` | info | the sender named the target in `X-Postwerk-Skip`; one per recipient |
| `delivery.not_allowed` | warn | a recipient is outside the target's allow-list; the whole target is skipped, one event per recipient |
| `delivery.unsupported` | error (warn: optional) | the mail (or this recipient's address) needs `SMTPUTF8` or `8BITMIME` and the target does not offer it; nothing was sent |
| `delivery.too_large` | error (warn: optional) | the message as Postwerk would send it is larger than the `SIZE` the target offers; nothing was sent |
| `target.down` | error (warn: optional) | the target answered `535` (text `auth: <reply>`) or connecting, TLS, the greeting or `EHLO` failed (`unreachable: <error>`); its worker stops sending and probes (D45). One per outage; for `unreachable` only once `POSTWERK_TARGET_DOWN_AFTER_PROBES` probes (default 1) failed too, and then with the last probe's error (D58) |
| `target.recovered` | info | a probe of a down target succeeded; text `<reason>, down for <duration>` |
| `retention.raw_expired` | error (warn: optional) | the stored message was removed while this recipient was `queued` or `unknown`; it is now `failed` |
| `retention.purged` | info | a retention run changed something; text holds counts only (`raw_purged=… raw_expired=… pseudonymized=… records_deleted=… events_deleted=…`) |
| `store.decrypt_failed` | error | the stored message could not be decrypted (unknown key id, failed authentication); the delivery is `failed`, nothing was sent |
| `tls.reloaded` | info | a changed certificate and key pair loaded and is served from the next handshake; text names the listener, subject and expiry |
| `tls.reload_failed` | error | a changed pair did not load (unreadable, no match, expired), also after a second read 2 s later; the old one stays. Once per content |
| `tls.expiring` | warn; error below 7 days and once expired | the served certificate has less than 14 days left; at most once a day, and at once when it turns into an error; the time left is rounded down |
| `delivery.closed` | info | an admin closed this recipient; `actor` is the account or API token, text `resolution=<confirmed\|given_up> was=<status> via=<ui\|api>` and `: <note>` if given (redacted) |
| `delivery.resend` | info | an admin queued this recipient again; `actor`, text `was=<status> via=<ui\|api>` |
| `ui.login` | info | someone logged in to the UI; `actor` is the account, text `file` or `database` |
| `ui.login_failed` | warn | a failed password check; `actor` only if the name is an account; text empty for a login, `confirm` for the password asked for a change |
| `ui.login_throttled` | warn | failed checks started a wait; `actor` as above, text `account` or `source` |
| `ui.search` | info | a UI search; `actor` is the account, text the search kind (`id`, `ref`, `message_id`, `address`, `any`), never the query |
| `ui.view` | info | someone opened the mail's page; `actor` is the account; left out of the mail's timeline |
| `ui.account_created`, `ui.password_set`, `ui.account_deleted` | info | an account change; `actor` made it, text is the account's name |
| `api.request` | info | a query API request; `actor` is the token name, text the endpoint, search kind, HTTP status and number of mails (`search by=address status=200 results=1`), never the query |
| `api.forbidden` | warn | an API token asked for an endpoint its role may not use; text the endpoint |
| `api.view` | info | a mail's detail was returned through the API; `actor` is the token name, `mail_id` the mail; no text |
| `api.auth_failed` | warn | missing, malformed or wrong API tokens, counted: at most one per actor and minute, text `failures=<n>`; `actor` only if the name is in the tokens file |
| `ingress.auth_failed` | warn | failed SMTP `AUTH` attempts (wrong password, unknown name, malformed `PLAIN`), counted: at most one per sender and minute, text `failures=<n>`; `sender` only if the name is in the senders file, otherwise empty |
| `ingress.rejected` | warn | refused commands of a logged-in sender, counted per sender, reason and minute: text `reason=<r> rejected=<n>`, `r` one of `too_large` (`552`), `bad_address` (`553 5.1.3`), `needs_smtputf8` (`553 5.6.7`), `too_many_recipients` (`452 4.5.3`), `line_too_long` (`500`). A `SIZE=` above the limit at `MAIL` is answered by the SMTP library itself and not counted |
| `queue.stale` | error (warn: optional) | the oldest mail with a recipient `queued` or `sending` at this target is older than `POSTWERK_QUEUE_STALE_AFTER`; `target` set, text `queued=<mails> oldest=<age> after=<limit>`. Once per change, again after `POSTWERK_QUEUE_STALE_REPEAT` if set |
| `queue.recovered` | info | no mail at this target is older than that any more; same text |
| `disk.low` | error | free space of the data volume below `POSTWERK_MIN_FREE_BYTES`; text `free=… total=… reserve=…` |
| `disk.ok` | info | free space at least the reserve plus a quarter again |

The overall state of a delivery or a mail is derived from its recipients; it is not stored twice.

On stdout and in the Sentry sink, an event with a reply code carries `"smtp": {"code", "text"}`
(`report.FromSMTPReply`); every other event carries its text as `"text"`.

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
| `5xx` to `RCPT` | that recipient | `failed` (`delivery.rejected` if it refuses the address, D46) | — |
| `4xx` to `RCPT`, or `552` (too many recipients, RFC 5321 4.5.3.1.10) | that recipient | queued, backoff; only queued recipients go into the next attempt | — |
| connecting, TLS, greeting or `EHLO` fails | every recipient of the attempt | queued, backoff; the target goes down (`target.down`) | — |
| reply `535` | every recipient of the attempt | queued, backoff; the target goes down (`target.down`) | queued, backoff |
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
whole window is used. Levels: `failed`, `unknown` and `target.down` are `error`, or `warn` for an optional
target; retries and `delivery.rejected` are `warn`.

**Target health** (D45, `internal/delivery/health.go`). Each worker keeps its target's state in memory: up, or down
since a time with a reason. `process` marks the target down when the attempt failed while connecting (dial, TLS,
greeting, `EHLO`, whatever the reply) or with a `535` before the message; the attempt's recipients are queued as for a
temporary failure, then `target.down` is appended: at once for `535`, for an unreachable target after
`DownAfterProbes` failed probes (D58; a target back before that writes neither `target.down` nor `target.recovered`). While down, the worker claims nothing. Each loop step first fails
the queued recipients whose retry window is over (`store.ExpireQueued`, `delivery.failed`, at most 200 deliveries
per transaction), then sleeps until the next probe (wake signals are ignored) or runs it: `connect` and `AUTH`
through the same code as a send, then `QUIT`. A failed probe doubles the wait up to the maximum; a good one sets the
target up, moves the next attempt of every queued delivery of the target to now (`store.MakeDue`) and appends
`target.recovered`. The worker calls `MakeDue` once at start as well, after the recovery of interrupted deliveries. `Manager.Health` returns the states for the UI and the API
(`query.Options.Health`, wired in `cmd/postwerk`); `query` does not import `delivery`.

**Mailjet tracking and CA files.** For a `STATUS=mailjet` target with `MAILJET_TRACKING=off`, `headersFor` adds
`X-Mailjet-TrackOpen: 0` and `X-Mailjet-TrackClick: 0` before `X-MJ-CustomID` (`X-Postwerk-Id` is always last,
D60), and `stripFor` removes the sender's
fields of those names; `MaxAddedBytes` and `message.Size` count them like the other added lines (D47). A target with
`CA_FILE` has its own `x509.CertPool` (`config.Target.RootCAs`), used instead of the system roots in `tlsConfig`
(D48).

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
start); the API listener gets the same half, then its remaining connections are closed. net/http counts a connection
that has not sent a request yet as busy for five seconds, and an HTTP client's spare keep-alive connection is one, so
without that bound it could use up the deadline. Ingress and the API then write their folded refusal events. Then the
remaining events are exported, HTTP stops and the store closes, which releases the data lock. Because the deadline is longer than
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
sender's `X-MJ-CustomID` and tracking fields for Mailjet targets, a bare LF counted as CRLF; `message.Size`) and sends it as `SIZE=` to a
target that offers `SIZE`. Ingress accepts `POSTWERK_MAX_MESSAGE_BYTES` minus `delivery.MaxAddedBytes`, the length of
those headers for the configured targets, so a mail it accepts fits a target with that limit (I17). If the target
names a limit and the message is larger, the delivery fails with `delivery.too_large` before `AUTH` and `MAIL`; like
`delivery.unsupported`, this is final.

Many clients declare `SMTPUTF8` whenever a server offers it (Go's `net/smtp` does), which is why a pure ASCII mail
does not need it. `delivery.unsupported` is final: the target's capabilities do not change between attempts.

Before a send, the worker opens the sealed message. If retention removed it, the claim returns no message and the
delivery fails with `retention.raw_expired`; if it cannot be decrypted, it fails with `store.decrypt_failed`. In both
cases nothing is sent. For a Mailjet target, any `X-MJ-CustomID` the sender set is removed before Postwerk adds its own,
and with tracking off also the sender's `X-Mailjet-TrackOpen` and `X-Mailjet-TrackClick`.

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
`sending` or `unknown` (within the limits below). A recipient an admin closed is `handed_off` or `failed` and never
changes again; an admin's close removes the message itself when nothing else needs it, and a resend needs the message
and never keeps it longer (D54).

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
(`addrhash.Hasher.Lookup`), so they find plain and pseudonymized rows alike. The UI's search uses it through
`internal/query`, for the UI and the query API (H2); erasure requests will too.

## Query layer and UI

`internal/query` holds the read logic; the SQL behind it is in the store (`ListMails`, `TargetStatsSince`,
`MailIDsByRef`, `MailIDsByMessageID`, `MailIDsByAddress`, `MailEvents`, `Mail`, `Deliveries`). `ListMails` filters by
the derived state in SQL: it counts each mail's recipients per status and applies the same rule as `store.Summarize`
(a test checks both agree for every combination), so pages stay correct with a state filter. A mail's state counts
the recipients of its required targets only: `MailFilter.Optional` (from `query.Options.Optional`) leaves the
optional targets out of the join unless the mail has no other delivery, and `store.MailStatus` does the same for one
mail's detail (a randomized test compares both). `MailEvents` returns a
mail's history: the relay's events and the admin's `delivery.closed` and `delivery.resend`, not who looked at it. The
query layer decides
what a reader may see: `query.Full` (the UI, and API tokens with role `admin`) returns addresses, `query.Masked` (API
tokens with role `read`, the zero value) returns `j•••@e•••.com` and masks the `Message-ID` (`query.MaskMessageID`),
and a pseudonymized mail returns no address at all. No function returns the stored message.

`internal/ui` is one `http.Handler` on the admin listener (`ui.NewServer` sets the timeouts). An outer layer sets the
security headers on every response, refuses unknown `Host` headers and bounds the body; Go's
`http.CrossOriginProtection` refuses cross-site `POST`s; every page needs a session, and every `POST` the session's
CSRF token. Sessions are a map in memory. Templates (`html/template`) and the stylesheet are embedded; the UI has no
JavaScript. Login checks run through `auth.Tokens`, at most `GOMAXPROCS` at once. A login or search is allowed only
after its event is written, so nothing happens in the UI that the audit log misses. Details: [ui.md](ui.md).

## Signals

`internal/watch` holds the checks that are about Postwerk itself, not about one mail (design.md section 9.1). Each
starts from the newest of its events in the event log (`store.LastOf`), so a restart neither repeats a `disk.low` or
`queue.stale` nor loses the recovery.

- **Disk.** `statfs` on the directory of the database file (Linux, macOS, FreeBSD; elsewhere the check logs that it
  is off). Ingress calls `Allow(size)` at `MAIL`, at `RCPT` and after `DATA` with the message's size, so a burst
  cannot outrun the 10 s interval; one `statfs` costs a system call. Low below `POSTWERK_MIN_FREE_BYTES`, ok again at
  the reserve plus a quarter. The event is written by the check's own loop, never in a session; a failed write is
  tried again on the next round.
- **Why a reserve, and why 64 MiB.** SQLite needs room to write after the volume is "full" for Postwerk: the
  `disk.low` event, the export cursors, and the deliveries of mails already accepted. With `secure_delete` the
  removal of a delivered message overwrites its pages, which goes through the WAL first, so one hand-off can grow the
  WAL by about the message's size until the next checkpoint (the WAL file is reused, and retention truncates it every
  10 minutes). The reserve therefore covers a few of the largest messages: 64 MiB is four of 15 MiB plus the small
  writes. Ingress refuses new mail first, because it is the only writer that can be told to come back later.
- **Store writability.** `store.tx` and `SetCursor` remember a write that failed with `SQLITE_FULL`, `SQLITE_IOERR`,
  `SQLITE_READONLY` or `SQLITE_CANTOPEN` (a file system remounted read-only after errors gives the last ones) until
  a write succeeds (`Store.Writable`). Meanwhile `/readyz` fails, and the disk check writes one page (`Store.Probe`,
  `user_version` set to itself) every round until it goes through. This needs no event: when the store cannot be
  written, no event can be.
- **Queue.** Every minute, `store.Queues` (mails with a recipient `queued` or `sending`, per target, and the oldest
  received time; it uses the index on `delivery_recipients.status`).

## Errors and logging

- Ingress holds a whole message in memory between `DATA` and the commit. A global budget
  (`POSTWERK_MAX_INFLIGHT_BYTES`) bounds that: `DATA` reserves the maximum size (or the declared `SIZE`) before reading,
  and gets `451 4.3.1` when the budget is used up. With `DATA`, the SMTP library sends `354` before Postwerk's
  `Data` runs, so a refused message still crosses the network but is discarded without being buffered. The budget
  counts message bytes; processing and sealing copy a message a few times, so actual memory is a small multiple.
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
- Target health: a target answering `535` with four mails queued gets the first attempt and then only probes (the
  test counts the logins), writes one `target.down` and one Sentry report, and after the fix one
  `target.recovered`, and every mail goes out; an unreachable target, a target with an untrusted certificate and one
  without STARTTLS go down the same way; the retry window still ends while a target is down.
- Every end-to-end test sends to a canary address and checks at the end that it appears nowhere in logs, exported
  events, the events table or delivery reply texts (leak test). The end-to-end harness runs with encryption on. One
  test also runs retention with tiny periods and then searches the database file and the WAL file byte by byte: the
  message body is never there in plaintext, and after pseudonymization neither are the recipient or the envelope
  sender; only hashes remain, and the mail is still found by address.
- UI tests drive the handler with `httptest`: Host check, security headers on every response, CSRF, login (one token
  check per attempt, whatever was typed), concurrency limit, session expiry, logout, full addresses for every
  account, pagination, a crawl of every reachable page that must never show the seeded message body, and the canary
  leak test for searches (the canary is never in logs, events or the exported stream, and never echoed into a page). `UI_DEMO_ADDR=127.0.0.1:18081 go test ./internal/ui -run TestDemo` serves made-up data for
  design work and screenshots.
- API tests drive the handler with `httptest`: role separation (a `read` token cannot list and sees masked
  addresses and a masked `Message-ID`, `admin` sees them in full), the search bound for `read`, auth failures and
  their folded events, both rate limits, method, content type, body size and browser checks, the Host allow-list, security headers, and a
  leak test (no content, subject or other header in any response; the query never in an event, log or the exported
  stream). `cmd/postwerk` runs the whole program with the API over TLS.
- Close and resend are tested in the store (every transition, refusals that change nothing, the note redacted, the
  message removed at the last close and kept while another delivery needs it, a closed `unknown` untouched by raw
  expiry, no resend after it, the timeline), in the UI and the API (roles, CSRF, the password asked again, 409s with
  their reasons, events with the actor) and end to end with a running worker: eight resends at once queue the mail
  once, and while the worker holds the delivery every close and resend is refused, so the target gets exactly one
  more copy.
- The end-to-end harness also runs the Sentry sink against a fake endpoint, and the leak check searches every
  envelope it received for the canary addresses and the message content. `internal/report` tests the Sentry sink on
  its own: envelope and event fields, level filter, rate limits, backoff, timeout, redirects, and catching up after
  an outage.
- TLS tests use a self-signed certificate made in the test; the delivery package takes the trusted roots through an
  unexported option, so there is no "skip verification" setting.
