# Postwerk design

Status: foundation built. SMTP ingress, the store, per-target delivery with the retry policy, the event log with the
stdout output, retention and encryption at rest exist and are tested. Parts marked *(planned)* are not built yet; their
details may change. The code layout is in [architecture.md](architecture.md), every setting in
[configuration.md](configuration.md), and whether Postwerk fits a given setup in [scope.md](scope.md).

## 1. Purpose and scope

Postwerk is a lightweight, reliable central point and relay for outgoing mail inside a server or cluster. Applications
hand their finished mails to Postwerk. Postwerk stores each mail, delivers a copy to every configured target and records
every step, so every mail can be audited and tracked from one place.

- **In:** finished RFC 5322 messages over SMTP, from known senders (login required).
- **Out:** the same messages, unchanged except for Postwerk's own headers, to a list of targets: a sending provider such
  as Mailjet, a capture tool such as Mailpit, or any other SMTP server.
- **Beside:** an append-only event log, exported to observability tools, and *(planned)* a read-only query API with a
  small UI.

Postwerk does not read or change message content. It does not receive mail for mailboxes, sign messages, render
templates, build links, check link expiry or decide what an alert is.

Design goals, in order: never lose track of a mail, never send a mail twice by accident, stay small (one binary, one
file, no root, no special cluster permissions, no inbound port from the internet).

## 2. Components

| Component | Responsibility | Status | Library |
|---|---|---|---|
| Ingress | SMTP server, `AUTH PLAIN` per sender, size and recipient limits, STARTTLS | built | `github.com/emersion/go-smtp`, `github.com/emersion/go-sasl` |
| Store | SQLite in WAL mode, one connection, migrations embedded | built | `modernc.org/sqlite` (pure Go) |
| Delivery | one worker per target: due deliveries out, retry with backoff, recovery at start | built | `github.com/emersion/go-smtp` (client) |
| Retention | removes raw messages, pseudonymizes addresses, expires records and events | built | standard library |
| Reporter | event log → sinks, each with its own cursor | built for stdout; other sinks planned | standard library |
| Health | `/healthz`, `/readyz` on the internal HTTP listener | built | standard library |
| Provider status | after hand-off, ask the provider for the final status | planned | provider adapter (Mailjet first) |
| Query API and UI | read-only JSON API and HTML pages, admin-only resend | planned | standard library, `html/template` |

One process, one container, one data volume. [architecture.svg](architecture.svg) shows the full design, including the
planned parts (provider status, query API and UI, sinks other than stdout).

## 3. Targets

Delivery goes to a list of targets. Every accepted mail gets one delivery per target, and each delivery has its own
state, attempts, retries and status. A target that is slow or down does not hold up the others.

Typical setups:

| Setup | Targets |
|---|---|
| Production | `mailjet` |
| Production with a temporary capture copy | `mailjet`, `capture` (optional) |
| Development and tests | `mailpit` |
| Tests against a real provider | `mailjet` (with `ALLOW_RECIPIENTS`), `mailpit` |

Per target (`POSTWERK_TARGET_<NAME>_<SETTING>`; full list in [configuration.md](configuration.md)):

| Setting | Meaning | Default |
|---|---|---|
| `URL` | `smtps://host[:465]` (implicit TLS), `smtp://host[:587]` (STARTTLS required) or `smtp+plain://host[:25]` (cleartext, never with credentials) | required |
| `CREDENTIALS_FILE` | `username:password` for the target | none |
| `STATUS` | provider status adapter; only `mailjet` is accepted *(adapter planned)* | none |
| `OPTIONAL` | failures are reported as warnings instead of errors | `false` |
| `RETRY_WINDOW` | how long to keep retrying | global value, `24h` |
| `ON_UNKNOWN` | what to do with an unclear outcome, see section 6 | `check` if `STATUS` is set, else `hold` |
| `ALLOW_RECIPIENTS` | comma-separated address patterns such as `*@example.test`; a mail with any recipient outside the list is not delivered to this target | none (all allowed) |

TLS is the default towards every target. `smtp://` fails if the target does not offer STARTTLS; Postwerk does not fall
back to cleartext. Cleartext needs the explicit scheme `smtp+plain://`, which is meant for a local capture tool such as
`smtp+plain://mailpit:1025` and refuses credentials.

The recipient allow-list is an envelope check and stays content-blind. It is all or nothing per target, on purpose:
if one recipient is outside the list, every recipient of that target is `skipped` with a `delivery.not_allowed`
warning, and the mail is not sent there at all. Sending to the allowed part only would deliver a mail whose `To` and
`Cc` lines name people who never got it, which is exactly the confusion a test setup must avoid. It protects a test setup with a real provider
from mailing real people by accident.

Senders can keep a mail away from named targets with `X-Postwerk-Skip: <target>[, <target>]`, for example to keep mails
with credentials out of a capture tool. Unknown target names in the header are ignored and logged.

Envelope parameters: Postwerk stores and re-issues the envelope, so it keeps what the sender declared in `MAIL`
(`BODY=`, `SMTPUTF8`, `SIZE=`) and passes it on to every target that offers the extension (`SIZE=` with the exact
size of the message as Postwerk sends it). A message larger than a target's `SIZE` is not sent there
(`delivery.too_large`). A mail that needs
`SMTPUTF8` (a non-ASCII address or header) or `8BITMIME` (8-bit content) is never sent to a target that lacks it; that
delivery fails with `delivery.unsupported`. `DSN` and `REQUIRETLS` are not offered on ingress.

Headers: Postwerk removes every `X-Postwerk-*` header set by the sender before it stores the mail. It reads
`X-Postwerk-Ref` and `X-Postwerk-Skip` first. Towards every target it adds `X-Postwerk-Id: <mail id>`, and for targets
with `STATUS=mailjet` also `X-MJ-CustomID: <mail id>`. No other header is added or changed.

## 4. Lifecycle

A mail has one record and one delivery per target. The unit of tracking is the triple **(mail, target, recipient)**:
each recipient of each delivery has its own status, and that status is the single source of truth. The mail is
`accepted` once stored. Each recipient moves on its own:

```
queued ──▶ sending ──250──▶ handed_off ──(provider status, planned)──▶ delivered
  ▲          │                                                     ├──▶ bounced
  └─ retry ──┤ 4xx, 535, network error                             ├──▶ blocked
             ├─ 5xx, retry window over ──▶ failed                  └──▶ complained
             └─ no reply after the message was sent ──▶ unknown ──(section 6)

skipped: excluded by X-Postwerk-Skip, or a recipient is outside the target's allow-list
```

Built statuses: `queued`, `sending`, `handed_off`, `failed`, `unknown`, `skipped`, per (mail, target, recipient). The
provider statuses (per recipient, matching Mailjet's one message per recipient) and closing an `unknown` delivery by
hand are planned.

Scheduling and attempts stay per (mail, target): one SMTP transaction carries several recipients. But every result,
every retry decision and every event is per recipient. As in any SMTP relay (RFC 5321), the target answers each
`RCPT` on its own: the message goes to the accepted recipients, a recipient refused with `5xx` fails alone, a recipient
refused with `4xx` stays queued and only such recipients go into the next attempt. Events name the recipient by its
position in the envelope, never by its address.

The status of a delivery, and of a mail, is a **summary computed when reading**, never stored: `queued` while any
recipient is queued, `unknown` while any is unknown, `handed_off` when all were handed off, `failed` when none was,
and `partial` when some were handed off and the others failed. `partial` exists only as such a summary, for logs and
the later API.

- `accepted`: stored, `250` sent to the sender. The raw message is kept, encrypted, until it is no longer needed for
  delivery (section 7).
- `handed_off`: the target replied `250`. Without a provider status adapter this is the last state.
- `failed`, `unknown` and `target.auth_failed` raise an event with level `error` (`warn` for optional targets).
- On start, each worker recovers its recipients left in `sending`: back to `queued` if the message was not sent yet,
  through `ON_UNKNOWN` if it was. An unclear outcome only ever applies to the recipients of that attempt.
- The mail's overall state is derived from the recipients of its required targets: `done` when all are `handed_off`
  or later, `problem` as soon as one failed or is unknown, `pending` otherwise. It is not stored separately.

### What `250` means

A `250` reply from Postwerk (`250 2.0.0 queued as pw_…`) means Postwerk has stored the mail and is now responsible for
it. It does not mean the mail was delivered. The events, and later the status API and UI, say what happened afterwards.

Every `250` along the way has the same meaning: "I have it now." The target's `250` means the provider has it; only the
provider status says whether the recipient's server accepted it. Whether a person read it, or found it in the spam
folder, no step can tell.

Stored is not indestructible: if the data volume is lost before hand-off, the mails on it are lost. Normally a mail
waits only seconds before hand-off.

## 5. Retry policy

| Target reply | Meaning | Postwerk does |
|---|---|---|
| `250` after the message | accepted | `handed_off` |
| `5xx` to one `RCPT` | permanent for that recipient | that recipient `failed`; the others go on |
| `4xx` to one `RCPT` (and `552`, which RFC 5321 counts as "too many recipients") | temporary for that recipient | that recipient stays queued; only queued recipients go into the next attempt |
| `4xx` | temporary | retry with backoff |
| network error, TLS error, timeout before the message was sent | temporary | retry with backoff, event `target.unreachable` |
| `535` (authentication failed) | our credentials are wrong or rotated | **retry and raise an error at once**; the delivery stays queued |
| other `5xx` | permanent | `failed` |
| target lacks `SMTPUTF8` or `8BITMIME` that the mail needs (seen in `EHLO`) | permanent | `failed`, event `delivery.unsupported`, nothing sent |
| message larger than the target's `SIZE` (seen in `EHLO`) | permanent | `failed`, event `delivery.too_large`, nothing sent |
| no reply after the message was sent (timeout, crash) | unclear | `unknown`, see section 6 |
| retry window over | | `failed` |

Backoff: 30 s, 1 min, 2 min, 5 min, 10 min, 30 min, then hourly until the retry window (default 24 h) ends. The last
attempt is placed at the end of the window, so the whole window is used.

The `535` rule is deliberate. Some relays treat it as permanent and drop queued mail while the credentials are broken.
Postwerk treats a broken login as an outage on its own side.

"After the message was sent" is recorded per delivery (`data_sent_at`): it is set after the target's `354` reply and
before the first byte of the message. Details in [architecture.md](architecture.md#delivery-worker).

## 6. Unclear outcome

If the connection breaks after the message was sent, Postwerk cannot know whether the target has the mail. Resending
risks a duplicate, not resending risks a lost mail. The choice is per target (`ON_UNKNOWN`):

| Policy | Postwerk does | Fits |
|---|---|---|
| `hold` | stays `unknown`, raises an error, an admin decides (resend or close, planned) | targets without status lookup |
| `check` | *(planned)* looks the mail up at the provider by its tracking id; found → `handed_off`; still not found after a grace period (default 15 min) → resend once. Until the status adapter exists, `check` behaves like `hold`. | providers with a status adapter |
| `retry` | resends like a temporary error | capture tools and tests, where a duplicate does no harm |

The grace period exists because a provider may need a moment before a new mail shows up in its API. How long that is
for Mailjet has not been measured yet.

## 7. Store

One SQLite file with these tables (exact columns and migrations: [architecture.md](architecture.md#store)):

| Table | Holds |
|---|---|
| `mails` | id, received time, sender login, envelope sender, `Message-ID`, `ref`, size, and the raw message, encrypted at rest |
| `recipients` | one row per envelope recipient; kept apart so addresses can be pseudonymized or erased without touching anything else |
| `deliveries` | one row per mail and target: what one SMTP transaction shares (attempts, next attempt, `data_sent_at`, last reply code and redacted text) |
| `delivery_recipients` | one row per mail, target and recipient: its status (the single source of truth), last reply code and redacted text |
| `events` | the append-only event log (section 8) |
| `cursors` | one export position per sink |

- `events` is append-only. Triggers reject `UPDATE` and `DELETE`; only the retention job removes old rows.
- No message content and no recipient addresses in `events`. Every free text (SMTP replies, network errors) is stored
  with addresses replaced by `<redacted>`.
- The raw message is encrypted with AES-256-GCM before it is written. The key comes from a key file
  ([privacy-and-security.md](privacy-and-security.md#62-encryption-of-the-raw-message-at-rest)).
- Retention, with recommended defaults that operators can change: raw message removed when every recipient of every
  delivery is handed off or skipped, kept 72 h after a failure, never older than 7 days; recipient addresses replaced by a keyed hash
  after 30 days (lookups hash the query the same way); mail records and events removed after 90 days. Reasons for each
  period: [privacy-and-security.md section 3](privacy-and-security.md#3-lifecycle-and-retention).

SQLite settings: WAL, `synchronous=FULL`, foreign keys on, `secure_delete=ON`, one connection, busy timeout. The file is
created with mode `0600`. The store must sit on a local disk, not on a network file system.

## 8. Event model

Every state change is one event, written in the same transaction as the change. A result (retry, hand-off, failure,
unknown, skip) is one event per recipient and carries `"recipient"`, the position of the recipient in the envelope
(0, 1, …), never the address. All sinks receive the same shape:

```json
{
  "seq": 1042,
  "time": "2026-11-13T09:41:07.312Z",
  "level": "error",
  "kind": "target.auth_failed",
  "mail_id": "pw_01JCX4…",
  "target": "mailjet",
  "recipient": 0,
  "attempt": 3,
  "smtp": { "code": 535, "text": "5.7.8 authentication failed" },
  "sender": "auth-service",
  "instance": "prod"
}
```

Kinds written today:

| Kind | Level | When |
|---|---|---|
| `mail.accepted` | info | stored, `250` sent |
| `delivery.attempt` | info | a worker starts sending to a target (one per attempt) |
| `delivery.retry` | warn | temporary failure, next attempt scheduled |
| `delivery.handed_off` | info | the target replied `250` |
| `delivery.failed` | error (warn if optional) | permanent failure or retry window over |
| `delivery.unknown` | error (warn if optional) | unclear outcome under `hold` (or `check`, until the adapter exists) |
| `delivery.skipped` | info | excluded by `X-Postwerk-Skip` |
| `delivery.not_allowed` | warn | a recipient is outside the target's allow-list |
| `delivery.unsupported` | error (warn if optional) | the mail needs `SMTPUTF8` or `8BITMIME` and the target does not offer it; not sent |
| `delivery.too_large` | error (warn if optional) | the message is larger than the `SIZE` the target offers; not sent |
| `target.auth_failed` | error (warn if optional) | the target replied `535` |
| `target.unreachable` | warn | no reply before the message was sent: connection, TLS or protocol error |

Planned kinds: `mail.rejected`, `delivery.resent`, `delivery.closed`, `provider.delivered`, `provider.bounced`,
`provider.blocked`, `provider.complained`, `provider.stuck`, `queue.stale`. The retention job's own events are listed in
[architecture.md](architecture.md).

## 9. Sinks

A sink is one interface: take an event, export it, return an error or nothing. Each sink keeps its own cursor (`seq`) in
the store, so a sink that is down catches up later and nothing is lost.

| Sink | Format | Status |
|---|---|---|
| stdout | one JSON line per event | built, always on |
| OpenTelemetry | OTLP logs, plus a few metrics (queue size per target, oldest queued, attempts by result) | planned; on when an endpoint is set |
| Sentry protocol | error events grouped by `kind` and `target`; works with Sentry-compatible tools such as GlitchTip | planned; on when a DSN is set |
| Webhook | JSON `POST`, signed per the Standard Webhooks spec (HMAC-SHA256) | planned; on when a URL is set |

Each sink will have a filter (`level`, `kind`, `target`). The variables for the planned sinks are reserved: setting one
today stops the start with "not implemented yet".

## 10. Query API (planned)

Built today: `GET /healthz` (alive) and `GET /readyz` (store reachable, workers running) on the internal HTTP listener,
without a token. Everything else in this section is planned.

Internal listener, bearer tokens with scopes `read` and `admin`. Token use is itself logged as an event.

| Call | Returns | Scope |
|---|---|---|
| `GET /v1/mails/{id}` | state, deliveries per target (status, attempts, last reply, provider status), events | read |
| `GET /v1/mails?ref=…` / `?message_id=…` | the same, found by the sender's own reference or `Message-ID` | read |
| `GET /v1/mails?to=…&since=…&state=…&target=…` | list | read |
| `GET /v1/events?after=…` | event log from a cursor, for export and audit | read |
| `GET /v1/stats` | per target: queued, oldest queued, results of the last 24 h | read |
| `POST /v1/mails/{id}/resend?target=…` | new attempt for a `failed` or `unknown` delivery, while the raw message exists | admin |
| `POST /v1/mails/{id}/close?target=…` | mark an `unknown` delivery as done without resending | admin |

The API will never return message content. Searching by address is planned to move into a request body, so addresses
stay out of URLs and access logs ([privacy-and-security.md](privacy-and-security.md#82-privacy-changes-and-their-status)).

Senders can set `X-Postwerk-Ref: <their id>` to find a mail by their own reference later. A `ref` that contains `@` is
not stored, because a reference must be an opaque id, not an address. Postwerk also returns its id in the SMTP reply
(`250 2.0.0 queued as pw_…`).

## 11. UI (planned)

A small, read-only web UI on the internal listener: search, a mail with its deliveries and history, queue per target,
recent errors. Server-rendered HTML, no JavaScript build, no message content. Resend and close only with an admin token.
Not meant to be exposed to the internet.

## 12. Provider status (planned)

After hand-off, an adapter asks the provider what happened. Polling, not webhooks, so Postwerk needs no public endpoint.

Mailjet (first adapter): Postwerk sets `X-MJ-CustomID: <postwerk id>` (built) and looks up the message with
`GET /v3/REST/message?CustomID=…` (statuses include `queued`, `sent`, `deferred`, `softbounced`, `hardbounced`,
`blocked`, `spam`). Planned schedule: about 10 min, 1 h and 24 h after hand-off; stop at a final status. The same lookup
serves the `check` policy in section 6.

## 13. Security

- Ingress requires `AUTH PLAIN` before `MAIL`; sender names and PBKDF2-SHA256 password hashes come from a senders file.
  A connection is closed after 3 failed attempts, and after 30 s without a login. Every failed check costs the same
  (the strongest hash in the file), and at most one check per CPU runs at once (`454` when all are busy for 5 s).
  There is no blocking per IP address: in a cluster, pod addresses change and many senders share a node's address.
- `AUTH` is only offered over TLS (STARTTLS with a configured certificate). Cleartext `AUTH` needs an explicit setting
  (`POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true`), meant for a local machine or a network that encrypts traffic between
  nodes. Without a certificate and without that setting, Postwerk does not start.
- TLS towards targets by default; cleartext only with `smtp+plain://` and never with credentials (section 3).
- Limits: message size (default 15 MiB, the `SIZE` Mailjet offers), recipients per message (default 50), line length, read and write timeouts.
- The client only ever sees fixed reply texts; internal errors are logged, never sent.
- The HTTP listener is separate from ingress, defaults to `127.0.0.1:8080` and is meant for an internal network only.
- The container image is built from `scratch` (binary and CA certificates only, no shell) and runs as uid 65532 with a
  read-only root file system except for the data volume. No extra capabilities, no host network, no cluster API access.
- No content or recipient addresses in logs, events or error reports.

More in [privacy-and-security.md](privacy-and-security.md).

## 14. Configuration

Environment variables plus secret files and key files. Targets are named in `POSTWERK_TARGETS`; each name gets its own
variables. A short example with a provider and a temporary capture copy:

```
POSTWERK_SENDERS_FILE=/run/secrets/senders
POSTWERK_SMTP_TLS_CERT_FILE=/run/secrets/tls.crt
POSTWERK_SMTP_TLS_KEY_FILE=/run/secrets/tls.key

POSTWERK_TARGETS=mailjet,capture
POSTWERK_TARGET_MAILJET_URL=smtp://in-v3.mailjet.com:587
POSTWERK_TARGET_MAILJET_CREDENTIALS_FILE=/run/secrets/mailjet
POSTWERK_TARGET_CAPTURE_URL=smtp+plain://mailpit:1025
POSTWERK_TARGET_CAPTURE_OPTIONAL=true
POSTWERK_TARGET_CAPTURE_ON_UNKNOWN=retry
# plus the key files for encryption at rest and address hashing
```

Every variable, its default, the key files and the retention settings: [configuration.md](configuration.md). Unknown
`POSTWERK_*` variables are errors, and all configuration errors are reported together before the process exits.

## 15. Testing

Built:

- Every row of the retry policy is triggered on purpose against fake SMTP targets in the test suite.
- Two targets, one broken: the other is delivered on time, the broken one retries on its own.
- Crash during sending, restart, and proof that the recovery and each built `ON_UNKNOWN` policy behave as described.
- Leak test: every end-to-end test sends to a canary address and checks that it appears nowhere in logs, exported
  events, the events table or reply texts.
- `docker compose` example with Mailpit as target for local runs ([examples/compose.yaml](../examples/compose.yaml)).

Planned: a sink that is down catches up after it comes back (for the external sinks), tests against Mailpit's chaos
mode, and the `check` policy against a fake provider API.

## 16. Open points

- Target reply texts and rate limits of real providers: to be measured.
- Provider API rate limits for status lookups, and how long a new mail takes to appear there (grace period for `check`).
- Single instance: during a restart the ingress is unavailable for a few seconds. A Postgres store for two instances
  can be added later if needed.
