# Postwerk design

Status: draft. Everything here is a plan; details may change during implementation. Open points are listed at the end.
What Postwerk should and should not do, and whether it fits a given setup: [scope.md](scope.md).

## 1. Purpose and scope

Postwerk is a lightweight, reliable central point and relay for outgoing mail inside a server or cluster. Applications
hand their finished mails to Postwerk. Postwerk stores each mail, delivers a copy to every configured target and records
every step, so every mail can be audited and tracked from one place.

- **In:** finished RFC 5322 messages over SMTP, from known senders (login required).
- **Out:** the same messages, unchanged except for tracking headers, to a list of targets: a sending provider such as
  Mailjet, a capture tool such as Mailpit, or any other SMTP server.
- **Beside:** an append-only event log, exported to observability tools, and a read-only query API with a small UI.

Postwerk does not read or change message content. It does not receive mail for mailboxes, sign messages, render
templates, build links, check link expiry or decide what an alert is.

Design goals, in order: never lose track of a mail, never send a mail twice by accident, stay small (one binary, one
file, no root, no special cluster permissions, no inbound port from the internet).

## 2. Components

| Component | Responsibility | Writes to the store | Reads from the store | Library (planned) |
|---|---|---|---|---|
| Ingress | SMTP server, `AUTH` per sender, size and recipient limits, optional STARTTLS | new mails | — | `github.com/emersion/go-smtp` |
| Store | SQLite in WAL mode, one writer | — | — | `modernc.org/sqlite` (pure Go) |
| Delivery | due deliveries out to their target, retry with backoff | attempt results | due deliveries | `github.com/emersion/go-smtp` (client) |
| Provider status | after hand-off, ask the provider for the final status | provider status | handed-off deliveries | provider adapter (Mailjet first) |
| Reporter | event log → sinks, each with its own cursor | sink cursors | events | OpenTelemetry Go SDK, Sentry Go SDK |
| Query API and UI | read-only JSON API and HTML pages, admin-only resend | resend requests | mails, deliveries, events | Go standard library, `html/template` |

One process, one container, one data volume. See [architecture.svg](architecture.svg).

## 3. Targets

Delivery goes to a list of targets. Every accepted mail gets one delivery per target, and each delivery has its own
state, attempts, retries and status. A target that is slow or down does not hold up the others.

Typical setups:

| Setup | Targets |
|---|---|
| Production | `mailjet` |
| Production with a temporary capture copy | `mailjet`, `capture` (optional) |
| Development and tests | `mailpit` |
| Tests against a real provider | `mailjet`, `mailpit` |

Per target:

| Setting | Meaning | Default |
|---|---|---|
| `URL` | `smtp://` or `smtps://` address | required |
| `CREDENTIALS_FILE` | login for the target | none |
| `STATUS` | provider status adapter, e.g. `mailjet` | none |
| `OPTIONAL` | failures are reported as warnings instead of errors | `false` |
| `RETRY_WINDOW` | how long to keep retrying | global value |
| `ON_UNKNOWN` | what to do with an unclear outcome, see section 6 | `check` if `STATUS` is set, else `hold` |

Senders can keep a mail away from named targets with `X-Postwerk-Skip: <target>[, <target>]`, for example to keep mails
with credentials out of a capture tool. Unknown target names in the header are ignored and logged.

The header `X-Postwerk-Skip` is removed before delivery. Postwerk adds `X-Postwerk-Id: <mail id>` and, for providers
that support it, its own tracking header (Mailjet: `X-MJ-CustomID`).

## 4. Lifecycle

A mail has one record and one delivery per target. The mail is `accepted` once stored. Each delivery moves on its own:

```
queued ──▶ sending ──250──▶ handed_off ──(provider status)──▶ delivered
  ▲          │                                           ├──▶ bounced
  └─ retry ──┤ 4xx, 535, network error                   ├──▶ blocked
             ├─ 5xx, retry window over ──▶ failed        └──▶ complained
             └─ crash or timeout after DATA ──▶ unknown ──(section 6)

skipped: the sender excluded this target with X-Postwerk-Skip
```

- `accepted`: stored, `250` sent to the sender. The raw message stays in the store until every delivery is final.
- `handed_off`: the target replied `250`. Without a provider status adapter this is the last state.
- `failed`, `unknown`, `bounced`, `blocked`, `complained` raise an event with level `error` (`warn` for optional targets).
- The mail's overall state is derived from its required targets: `done` when all are `handed_off` or later and none
  failed, `problem` as soon as one failed, bounced or is unknown, `pending` otherwise.

### What `250` means

A `250` reply from Postwerk means Postwerk has stored the mail and is now responsible for it. It does not mean the mail
was delivered. The status API and UI say what happened afterwards.

Every `250` along the way has the same meaning: "I have it now." The target's `250` means the provider has it; only the
provider status says whether the recipient's server accepted it. Whether a person read it, or found it in the spam
folder, no step can tell.

Stored is not indestructible: if the data volume is lost before hand-off, the mails on it are lost. Normally a mail
waits only seconds before hand-off.

## 5. Retry policy

| Target reply | Meaning | Postwerk does |
|---|---|---|
| `250` | accepted | `handed_off` |
| `4xx` | temporary | retry with backoff |
| network error, TLS error, timeout before `DATA` | temporary | retry with backoff |
| `535` (authentication failed) | our credentials are wrong or rotated | **retry and raise an error at once**; the delivery stays queued |
| other `5xx` | permanent | `failed` |
| timeout or crash after `DATA` was sent | unclear | `unknown`, see section 6 |

Backoff: 30 s, 1 min, 2 min, 5 min, 10 min, 30 min, then hourly until the retry window (default 24 h) ends.

The `535` rule is deliberate. Some relays treat it as permanent and drop queued mail while the credentials are broken.
Postwerk treats a broken login as an outage on its own side.

## 6. Unclear outcome

If the connection breaks after `DATA` was sent, Postwerk cannot know whether the target has the mail. Resending risks
a duplicate, not resending risks a lost mail. The choice is per target (`ON_UNKNOWN`):

| Policy | Postwerk does | Fits |
|---|---|---|
| `hold` | stays `unknown`, raises an error, an admin decides (resend or close) | targets without status lookup |
| `check` | looks the mail up at the provider by its tracking id; found → `handed_off`; still not found after a grace period (default 15 min) → resend once | providers with a status adapter |
| `retry` | resends like a temporary error | capture tools and tests, where a duplicate does no harm |

The grace period exists because a provider may need a moment before a new mail shows up in its API. How long that is
for Mailjet has not been measured yet.

## 7. Store

Three tables in one SQLite file.

```
mails      (id, received_at, sender, envelope_from, envelope_to, message_id, ref, size,
            raw,              -- removed when every delivery is final
            state)

deliveries (mail_id, target, status, attempts, next_attempt_at, last_reply,
            provider_id, provider_status, updated_at)

events     (seq, time, mail_id, target, kind, level, attempt, smtp_code, smtp_text, provider_status, actor)
```

- `events` is append-only. Triggers reject `UPDATE` and `DELETE`; only the retention job may remove old rows.
- No message content and no recipient addresses in `events`.
- Retention: raw message until every delivery is final, recipient address replaced by a keyed hash after a configurable
  period (lookups hash the query the same way), events kept for a configurable period.

SQLite settings: WAL, `synchronous=FULL`, one writer goroutine, busy timeout. The store must sit on a local disk, not on a
network file system.

## 8. Event model

Every state change is one event. All sinks receive the same shape.

```json
{
  "seq": 1042,
  "time": "2026-11-13T09:41:07Z",
  "level": "error",
  "kind": "target.auth_failed",
  "mail_id": "pw_01JCX4…",
  "target": "mailjet",
  "attempt": 3,
  "smtp": { "code": 535, "text": "5.7.8 authentication failed" },
  "sender": "auth-service",
  "instance": "prod"
}
```

Kinds (first set): `mail.accepted`, `mail.rejected`, `delivery.attempt`, `delivery.retry`, `delivery.handed_off`,
`delivery.failed`, `delivery.unknown`, `delivery.skipped`, `delivery.resent`, `delivery.closed`, `target.auth_failed`,
`target.unreachable`, `provider.delivered`, `provider.bounced`, `provider.blocked`, `provider.complained`,
`provider.stuck`, `queue.stale`, `retention.purged`.

## 9. Sinks

A sink is one interface: take an event, export it, return an error or nothing. Each sink keeps its own cursor (`seq`) in
the store, so a sink that is down catches up later and nothing is lost.

| Sink | Format | Default |
|---|---|---|
| stdout | one JSON line per event | on |
| OpenTelemetry | OTLP logs, plus a few metrics (queue size per target, oldest queued, attempts by result) | off; on when an endpoint is set |
| Sentry protocol | error events grouped by `kind` and `target`; works with Sentry-compatible tools such as GlitchTip | off; on when a DSN is set |
| Webhook | JSON `POST`, signed per the Standard Webhooks spec (HMAC-SHA256) | off; on when a URL is set |

Each sink has a filter (`level`, `kind`, `target`).

## 10. Query API

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
| `GET /healthz`, `GET /readyz` | alive; store writable and delivery loop running | none |

The API never returns message content.

Senders can set `X-Postwerk-Ref: <their id>` to find a mail by their own reference later. Postwerk also returns its id in
the SMTP reply (`250 2.0.0 queued as pw_…`).

## 11. UI

A small, read-only web UI on the internal listener: search, a mail with its deliveries and history, queue per target,
recent errors. Server-rendered HTML, no JavaScript build, no message content. Resend and close only with an admin token.
Not meant to be exposed to the internet.

## 12. Provider status

After hand-off, an adapter asks the provider what happened. Polling, not webhooks, so Postwerk needs no public endpoint.

Mailjet (first adapter): Postwerk sets `X-MJ-CustomID: <postwerk id>` and looks up the message with
`GET /v3/REST/message?CustomID=…` (statuses include `queued`, `sent`, `deferred`, `softbounced`, `hardbounced`,
`blocked`, `spam`). Planned schedule: about 10 min, 1 h and 24 h after hand-off; stop at a final status. The same lookup
serves the `check` policy in section 6.

## 13. Security

- Ingress requires `AUTH`; credentials per sender from a secrets file. Optional STARTTLS.
- Limits: message size, recipients per message, connections per sender.
- The query API and UI listen separately from ingress and are meant for an internal network only.
- The container runs as non-root with a read-only root file system except for the data volume. No extra capabilities,
  no host network, no cluster API access.
- No content or recipient addresses in logs, events or error reports.

## 14. Configuration

Environment variables plus secrets files. Targets are named in `POSTWERK_TARGETS`; each name gets its own variables.

```
POSTWERK_SMTP_LISTEN=:2525
POSTWERK_HTTP_LISTEN=127.0.0.1:8080          # API and UI, internal only
POSTWERK_DATA=/data/postwerk.db
POSTWERK_SENDERS_FILE=/run/secrets/senders
POSTWERK_RETRY_WINDOW=24h

POSTWERK_TARGETS=mailjet,capture
POSTWERK_TARGET_MAILJET_URL=smtp://in-v3.mailjet.com:587
POSTWERK_TARGET_MAILJET_CREDENTIALS_FILE=/run/secrets/mailjet
POSTWERK_TARGET_MAILJET_STATUS=mailjet
POSTWERK_TARGET_CAPTURE_URL=smtp://mailpit:1025
POSTWERK_TARGET_CAPTURE_OPTIONAL=true
POSTWERK_TARGET_CAPTURE_ON_UNKNOWN=retry

POSTWERK_OTEL_ENDPOINT=                      # empty: off
POSTWERK_SENTRY_DSN=                         # empty: off
POSTWERK_WEBHOOK_URL=                        # empty: off
```

## 15. Testing

- Every row of the retry policy is triggered on purpose against a fake target (Mailpit's chaos mode can return chosen
  SMTP errors).
- Two targets, one broken: the other is delivered on time, the broken one retries on its own.
- Crash during `DATA`, restart, and proof that each `ON_UNKNOWN` policy behaves as described.
- A sink that is down catches up after it comes back.
- `docker compose` example with Mailpit as target for local runs.

## 16. Open points

- Target reply texts and rate limits of real providers: to be measured.
- Provider API rate limits for status lookups, and how long a new mail takes to appear there (grace period for `check`).
- Single instance: during a restart the ingress is unavailable for a few seconds. A Postgres store for two instances
  can be added later if needed.
- Retention defaults.
