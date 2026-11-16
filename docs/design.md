# Postwerk design

Status: foundation built. SMTP ingress, the store, per-target delivery with the retry policy, the event log with the
stdout output and the Sentry-protocol sink, retention, encryption at rest, an internal UI and a read-only query API exist and are tested. Parts marked *(planned)* are not built yet; their
details may change. The code layout is in [architecture.md](architecture.md), every setting in
[configuration.md](configuration.md), and whether Postwerk fits a given setup in [scope.md](scope.md).

## 1. Purpose and scope

Postwerk is a lightweight, reliable central point and relay for outgoing mail inside a server or cluster. Applications
hand their finished mails to Postwerk. Postwerk stores each mail, delivers a copy to every configured target and records
every step, so every mail can be audited and tracked from one place.

- **In:** finished RFC 5322 messages over SMTP, from known senders (login required).
- **Out:** the same messages, unchanged except for Postwerk's own headers, to a list of targets: a sending provider such
  as Mailjet, a capture tool such as Mailpit, or any other SMTP server.
- **Beside:** an append-only event log, exported to observability tools, a small read-only UI and a read-only query
  API.

Postwerk does not read or change message content. It does not receive mail for mailboxes, sign messages, render
templates, build links, check link expiry or decide what an alert is.

Design goals, in order: never lose track of a mail, never send a mail twice by accident, stay small (one binary, one
file, no root, no special cluster permissions, no inbound port from the internet).

## 2. Components

| Component | Responsibility | Status | Library |
|---|---|---|---|
| Ingress | SMTP server, `AUTH PLAIN` per sender, size and recipient limits, STARTTLS | built | `github.com/emersion/go-smtp`, `github.com/emersion/go-sasl` |
| Store | SQLite in WAL mode, one connection, migrations embedded | built | `modernc.org/sqlite` (pure Go) |
| Delivery | one worker per target: due deliveries out, retry with backoff, recovery at start, target down and probes after a rejected login or an unreachable target | built | `github.com/emersion/go-smtp` (client) |
| Retention | removes raw messages, pseudonymizes addresses, expires records and events | built | standard library |
| Reporter | event log → sinks, each with its own cursor and loop | built for stdout and Sentry protocol; other sinks planned | standard library |
| Health | `/healthz`, `/readyz` on the internal HTTP listener | built | standard library |
| Provider status | after hand-off, ask the provider for the final status | planned | provider adapter (Mailjet first) |
| UI | HTML pages on the loopback admin listener: overview, mails, search, one mail, accounts | built | standard library, `html/template` |
| Query API | read-only JSON API on its own listener for other applications ([api.md](api.md)); admin-only resend and close later | built (read-only) | standard library |

One process, one container, one data volume. For Kubernetes there is a Helm chart with safe defaults
([kubernetes.md](kubernetes.md)). [architecture.svg](architecture.svg) shows the full design, including the
planned parts (provider status, the OpenTelemetry and webhook sinks).

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
| `OPTIONAL` | failures are reported as warnings instead of errors, and they do not change the mail's state (section 4) | `false` |
| `RETRY_WINDOW` | how long to keep retrying | global value, `24h` |
| `ON_UNKNOWN` | what to do with an unclear outcome, see section 6 | `check` if `STATUS` is set, else `hold` |
| `ALLOW_RECIPIENTS` | comma-separated address patterns such as `*@example.test`; a mail with any recipient outside the list is not delivered to this target | none (all allowed) |
| `MAILJET_TRACKING` | with `STATUS=mailjet` only: `off` switches Mailjet's open and click tracking off for every mail; `account` leaves it to the account's settings | `off` |
| `CA_FILE` | PEM bundle of the certificate authorities this target's certificate comes from; replaces the system roots for this target | none (system roots) |

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
with `STATUS=mailjet` also `X-MJ-CustomID: <mail id>` and, while `MAILJET_TRACKING=off` (the default),
`X-Mailjet-TrackOpen: 0` and `X-Mailjet-TrackClick: 0`. A Mailjet account with tracking on rewrites every link,
password reset links included, and an application cannot switch that off per mail through SMTP alone; so Postwerk
switches it off and removes any `X-MJ-CustomID`, `X-Mailjet-TrackOpen` or `X-Mailjet-TrackClick` the sender set
(D47). No other header is added or changed.

### Target health

A rejected login (`535`) or a target that cannot be reached (connecting, TLS, the greeting or `EHLO` fails) says
nothing about the mail; it is the target that is broken. On the first such failure the target goes **down** (D45):

- Its worker stops sending. Queued mails wait; new mails are accepted and queued as usual.
- A probe runs instead: connect, `EHLO`, `AUTH` if the target has credentials, `QUIT`. Never a mail. The first probe
  comes after `POSTWERK_PROBE_INTERVAL` (30 s), each failed probe doubles the wait up to `POSTWERK_PROBE_MAX_INTERVAL`
  (5 min). With wrong credentials the provider sees one login per probe, not one per queued mail, so it has no
  reason to lock the account.
- When a probe succeeds the target is **up** again and the queue resumes in its usual order.
- One `target.down` event (`error`, `warn` for an optional target) when it goes down, with the reason (`auth` or
  `unreachable`) and the redacted reply or error; one `target.recovered` event (`info`) when it comes back, with how
  long it was down. The mail whose attempt found the problem gets a `delivery.retry` (`warn`) like any temporary
  failure; the mails behind it get nothing until they are sent.
- The retry window still ends: a recipient queued at a down target past its window becomes `failed`
  (`delivery.failed`) without another attempt.
- The state lives in memory. After a restart every target starts up, and the first attempt finds out again (one
  more `target.down` if it is still broken). The UI and the API overview show each target as up or down since a time.

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
and `partial` when some were handed off and the others failed. `partial` exists only as such a summary, for logs, the
UI and the query API.

- `accepted`: stored, `250` sent to the sender. The raw message is kept, encrypted, until it is no longer needed for
  delivery (section 7).
- `handed_off`: the target replied `250`. Without a provider status adapter this is the last state.
- `failed` and `unknown` raise an event with level `error` (`warn` for optional targets), except a recipient address
  the target refuses for good, `delivery.rejected` (`warn`, section 5). A broken login or unreachable target raises
  one `target.down` for the target, not one event per mail (section 3).
- On start, each worker recovers its recipients left in `sending`: back to `queued` if the message was not sent yet,
  through `ON_UNKNOWN` if it was. An unclear outcome only ever applies to the recipients of that attempt.
- The mail's overall state is the same summary over the recipients of its **required** targets only; optional
  targets (a capture copy) are left out, so a mail that failed at the provider and reached Mailpit is `failed`, not
  `partial`, and a failed capture copy does not make a mail a problem. Each delivery keeps its own state, optional
  ones included. A mail that has deliveries to optional targets only is summarized over those. It is not stored
  separately; the list filters (`state=failed`) and the recent problems use the same rule.

### What `250` means

A `250` reply from Postwerk (`250 2.0.0 queued as pw_…`) means Postwerk has stored the mail and is now responsible for
it. It does not mean the mail was delivered. The events, the UI and the query API say what happened afterwards.

Every `250` along the way has the same meaning: "I have it now." The target's `250` means the provider has it; only the
provider status says whether the recipient's server accepted it. Whether a person read it, or found it in the spam
folder, no step can tell.

Stored is not indestructible: if the data volume is lost before hand-off, the mails on it are lost. Normally a mail
waits only seconds before hand-off.

## 5. Retry policy

| Target reply | Meaning | Postwerk does |
|---|---|---|
| `250` after the message | accepted | `handed_off` |
| `5xx` to one `RCPT` | permanent for that recipient | that recipient `failed`; the others go on; `delivery.rejected` (`warn`) if the reply refuses the address (below), else `delivery.failed` |
| `4xx` to one `RCPT` (and `552`, which RFC 5321 counts as "too many recipients") | temporary for that recipient | that recipient stays queued; only queued recipients go into the next attempt |
| `4xx` | temporary | retry with backoff |
| connecting, TLS, greeting or `EHLO` fails (any reply) | the target is unreachable | the target goes down (section 3): the mail stays queued, one `target.down` |
| network error or timeout later, before the message was sent | temporary | retry with backoff |
| `535` (authentication failed) | our credentials are wrong or rotated | the target goes down (section 3): **the mail stays queued, one error at once** |
| other `5xx` | permanent | `failed`; `delivery.rejected` for `550`, `551` or `553` that refuses the address (below) |
| target lacks `SMTPUTF8` or `8BITMIME` that the mail needs (seen in `EHLO`) | permanent | `failed`, event `delivery.unsupported`, nothing sent |
| message larger than the target's `SIZE` (seen in `EHLO`) | permanent | `failed`, event `delivery.too_large`, nothing sent |
| no reply after the message was sent (timeout, crash) | unclear | `unknown`, see section 6 |
| retry window over | | `failed` |

Backoff: 30 s, 1 min, 2 min, 5 min, 10 min, 30 min, then hourly until the retry window (default 24 h) ends. The last
attempt is placed at the end of the window, so the whole window is used.

The `535` rule is deliberate. Some relays treat it as permanent and drop queued mail while the credentials are broken.
Postwerk treats a broken login as an outage on its own side, and of the target, not of each mail (D45).

**Rejected recipients** (D46). A recipient address the target refuses for good is the sender's matter (a typo, a
closed mailbox), not the operator's. It is `failed` like any permanent failure, but its event is `delivery.rejected`
at `warn`, so an alert on `error` fires only for what the operator must act on. A reply counts as such a rejection when
its code is `550`, `551` or `553` and its enhanced status code is `5.1.x` (address) or `5.2.x` (mailbox); at `RCPT`
also a `550`, `551` or `553` without an enhanced code. A `550 5.7.1` (policy, relay denied, spam) or any `554` stays
`delivery.failed`. After the message, the rejection applies to the recipients of that attempt.

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
  "kind": "delivery.failed",
  "mail_id": "pw_01JCX4…",
  "target": "mailjet",
  "recipient": 0,
  "attempt": 3,
  "smtp": { "code": 554, "text": "5.7.1 relay access denied" },
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
| `delivery.rejected` | warn | the target refused the recipient's address for good (`550`/`551`/`553`, section 5); `failed` |
| `delivery.unknown` | error (warn if optional) | unclear outcome under `hold` (or `check`, until the adapter exists) |
| `delivery.skipped` | info | excluded by `X-Postwerk-Skip` |
| `delivery.not_allowed` | warn | a recipient is outside the target's allow-list |
| `delivery.unsupported` | error (warn if optional) | the mail needs `SMTPUTF8` or `8BITMIME` and the target does not offer it; not sent |
| `delivery.too_large` | error (warn if optional) | the message is larger than the `SIZE` the target offers; not sent |
| `target.down` | error (warn if optional) | the target rejected the login (`535`) or could not be reached; sending stops until a probe succeeds (section 3); no mail id, text `auth: <reply>` or `unreachable: <error>` |
| `target.recovered` | info | a probe succeeded and sending resumes; text `<reason>, down for <duration>` |
| `tls.reloaded` | info | a renewed STARTTLS certificate is served |
| `tls.reload_failed` | error | a changed certificate or key file did not load; the old certificate stays |
| `tls.expiring` | warn (error once expired) | the served certificate has less than 14 days left; once a day |
| `ui.login`, `ui.login_failed`, `ui.login_throttled`, `ui.search`, `ui.view`, `ui.account_created`, `ui.password_set`, `ui.account_deleted` | info (failures and throttling: warn) | someone used the UI; the event carries `"actor"` (the account) and `"text"` (where the account is kept, what was searched by, or the changed account's name), and `ui.view` the mail id; never a query, an address or a password ([ui.md](ui.md#4-audit-of-the-ui-itself)) |

Planned kinds: `mail.rejected`, `delivery.resent`, `delivery.closed`, `provider.delivered`, `provider.bounced`,
`provider.blocked`, `provider.complained`, `provider.stuck`, `queue.stale`. The retention job's own events are listed in
[architecture.md](architecture.md).

## 9. Sinks

A sink is one interface: take an event, export it, return an error or nothing. Each sink keeps its own cursor (`seq`) in
the store, so a sink that is down catches up later and nothing is lost. Each sink runs in its own loop, so a slow or
failing sink holds up no other sink and never delivery.

| Sink | Format | Status |
|---|---|---|
| stdout | one JSON line per event | built, always on |
| OpenTelemetry | OTLP logs, plus a few metrics (queue size per target, oldest queued, attempts by result) | planned; on when an endpoint is set |
| Sentry protocol | error events (warnings too, if chosen) grouped by `kind` and `target`, with `mail_id` as a searchable tag; works with Sentry-compatible tools such as GlitchTip | built; on when a DSN file is set ([configuration.md](configuration.md#error-reports-to-sentry-or-glitchtip), decisions D30, D31) |
| Webhook | JSON `POST`, signed per the Standard Webhooks spec (HMAC-SHA256) | planned; on when a URL is set |

The Sentry sink filters by level (`error`, or `warn` and above). Levels are chosen so that `error` means "the
operator must act": a target down, a mail failed for a reason on the relay's or the provider's side, an unclear
outcome, a decryption or retention failure. A refused recipient address (`delivery.rejected`), retries and failures at
optional targets are `warn`. A broken login or unreachable target is one `target.down` per outage, not one report per
mail; routing and grouping beyond that is the error tracker's job. Planned: a filter for every sink (`level`, `kind`,
`target`). The variables for the planned sinks are reserved: setting one today stops the start with "not
implemented yet".

The Sentry sink sends one envelope per event with plain HTTP, no SDK. The event holds the message
`<kind> at <target>: <code> <reply>`, the tags `kind`, `target`, `sender`, `instance`, `level` and `mail_id`, and the
attempt, recipient position and SMTP code as extra data; the environment is the instance. Its id is derived from the
event, so an export repeated after a crash is dropped by the receiver as a duplicate. It respects `429`,
`Retry-After` and `X-Sentry-Rate-Limits`, backs off while the server is down and keeps the events until then.

## 10. Query API

Built: a read-only JSON API for other applications in the cluster ([api.md](api.md), D32–D34). It has its own
listener, `POSTWERK_API_LISTEN`, off by default; unlike the UI it may bind the pod address, because other pods call
it. In the chart the NetworkPolicy lets in only the pods in `api.allowFrom`, and TLS is on. Its bearer tokens
`<name>.<token>` come from a file of their own, `POSTWERK_API_TOKENS_FILE`, never from the UI tokens file, and are
kept as SHA-256 (D41), with the roles `read` and `admin`. `GET /healthz` and `GET /readyz` stay on the internal HTTP
listener, without a token.

| Call | Returns | Role |
|---|---|---|
| `POST /v1/search` | the mails found by id, ref, `Message-ID` or address (the term in the body), as summaries without addresses; at most 20 for `read`, 100 for `admin` | read, admin |
| `GET /v1/mails/{id}` | one mail: deliveries per target with attempts and last reply, recipients, events; addresses and the `Message-ID` masked for `read` | read, admin |
| `GET /v1/mails?state=…&target=…&from=…&to=…&page=…` | the list, newest first, 50 per page; filters never hold an address | admin |
| `GET /v1/overview` | per target: recipients of the last 24 h by status, queued now, oldest queued; the recent problems | admin |

Every request is an event (`api.request`, or `api.view` for a returned mail), and is answered only once that event is
written; failed tokens are counted, one `api.auth_failed` per token name and minute (D42). The API never returns
message content, the subject or headers other than `Message-ID`. Searching by address takes a request body, as the
UI's search does, so addresses stay out of URLs and access logs
([privacy-and-security.md](privacy-and-security.md#82-privacy-changes-and-their-status)).

Planned, for `admin` tokens: `POST /v1/mails/{id}/resend?target=…` (a new attempt for a `failed` or `unknown`
delivery, while the raw message exists) and `POST /v1/mails/{id}/close?target=…` (mark an `unknown` delivery as done
without resending), D13.

Senders can set `X-Postwerk-Ref: <their id>` to find a mail by their own reference later. A `ref` that contains `@` is
not stored, because a reference must be an opaque id, not an address. Postwerk also returns its id in the SMTP reply
(`250 2.0.0 queued as pw_…`).

## 11. UI

Built: a small web UI ([ui.md](ui.md)), read-only for mail records. It runs on its own admin listener,
`127.0.0.1:8081` by default, loopback only, and is opened through `kubectl port-forward` or an SSH tunnel; it never
shares the health listener and is never on a Service or Ingress. Every account is an admin (D35). People log in with
a name and a password from an account kept in the database and managed on the UI's account pages, or, for the first
login and break-glass access, with a token from the UI tokens file (D37); they get an in-memory session. Pages: an
overview per target (last 24 hours by recipient status, queued now, oldest queued, recent problems), the mail list
with filters, a search by id, ref, `Message-ID` or address, one mail with its deliveries, recipients and events, and
the accounts. Addresses are shown in full; after pseudonymization nobody sees them. Server-rendered HTML, no
JavaScript, no message content; a Host check, CSRF tokens and a strict Content-Security-Policy guard against attacks
through the browser. Every login, search, opened mail and account change is an event.

Planned: resend and close (D13) as forms.

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
- Limits: memory for messages in flight (default 256 MiB, `451 4.3.1` when used up), message size (default 15 MiB, the
  `SIZE` Mailjet offers, minus Postwerk's own header lines), recipients per message (default 50), line length, read and write timeouts.
- The client only ever sees fixed reply texts; internal errors are logged, never sent.
- The HTTP listener is separate from ingress, defaults to `127.0.0.1:8080` and is meant for an internal network only.
- The UI has its own admin listener on loopback (`127.0.0.1:8081`), reached only through a tunnel, with a password login,
  sessions, a Host allow-list, CSRF tokens and strict headers ([ui.md](ui.md)).
- The query API, if turned on, has its own listener and bearer tokens, refuses browsers, is rate-limited and reachable
  only from the pods the NetworkPolicy allows ([api.md](api.md)).
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
POSTWERK_UI_TOKENS_FILE=/run/secrets/ui-tokens   # UI on 127.0.0.1:8081
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
  events, the events table or reply texts. The UI tests extend it to searches: the canary is never logged, never in an
  event and never echoed into a page; no page shows message content. The API tests check that a `read` token never
  sees it unmasked.
- `docker compose` example with Mailpit as target for local runs ([examples/compose.yaml](../examples/compose.yaml)).

- The Sentry sink against a fake endpoint: envelope shape, levels, rate limits, backoff, timeouts, a sink that is
  down catching up, and the leak test on every request body of the end-to-end tests. `SENTRY_LIVE_DSN=… go test
  ./internal/report -run TestSentryLive` sends real events to a running Sentry or GlitchTip.

Planned: tests against Mailpit's chaos mode, and the `check` policy against a fake provider API.

## 16. Open points

- Target reply texts and rate limits of real providers: to be measured.
- Provider API rate limits for status lookups, and how long a new mail takes to appear there (grace period for `check`).
- Single instance: during a restart the ingress is unavailable for a few seconds. A Postgres store for two instances
  can be added later if needed.
