# Postwerk design

Status: draft. Everything here is a plan; details may change during implementation. Open points are listed at the end.

## 1. Scope

Postwerk sits between applications that send mail and one upstream sending provider.

- **In:** finished RFC 5322 messages over SMTP, from known senders (login required).
- **Out:** the same messages, unchanged except for one tracking header, to one upstream SMTP endpoint.
- **Beside:** an append-only event log, exported to observability tools, and a read-only query API with a small UI.

Postwerk does not read or change message content. It does not render templates, build links, check link expiry or
decide what an alert is.

## 2. Components

| Component | Responsibility | Library (planned) |
|---|---|---|
| Ingress | SMTP server, `AUTH` per sender, size and recipient limits, optional STARTTLS | `github.com/emersion/go-smtp` |
| Store | SQLite in WAL mode, one writer | `modernc.org/sqlite` (pure Go) |
| Delivery | due mails out to the upstream, retry with backoff | `github.com/emersion/go-smtp` (client) |
| Provider status | after hand-off, ask the provider for the final status | provider adapter (Mailjet first) |
| Reporter | event log → sinks, each with its own cursor | OpenTelemetry Go SDK, Sentry Go SDK |
| Query API | read-only JSON API, admin-only resend | Go standard library |
| UI | server-rendered HTML, no JavaScript build | Go `html/template` |
| Mirror | optional copy to a capture SMTP server | `github.com/emersion/go-smtp` (client) |

One process, one container, one data volume.

## 3. Lifecycle of a mail

```
accepted ──▶ queued ──▶ sending ──250──▶ handed_off ──(provider status)──▶ delivered
                ▲          │                                           ├──▶ bounced
                └─ retry ──┤ 4xx, 535, network error                   ├──▶ blocked
                           ├─ 5xx, retry window over ──▶ failed        └──▶ complained
                           └─ crash or timeout after DATA ──▶ unknown
```

- `accepted`: stored, `250` sent to the sender. The raw message stays in the store until hand-off.
- `handed_off`: the upstream replied `250`. Without a provider status adapter this is the last state.
- `failed`, `unknown`, `bounced`, `blocked`, `complained` raise an error event.
- `unknown` is never retried automatically: the upstream may already have the mail. An admin can resend.

## 4. Retry policy

| Upstream reply | Meaning | Postwerk does |
|---|---|---|
| `250` | accepted | `handed_off` |
| `4xx` | temporary | retry with backoff |
| network error, TLS error, timeout before `DATA` | temporary | retry with backoff |
| `535` (authentication failed) | our credentials are wrong or rotated | **retry and raise an error at once**; the mail stays queued |
| other `5xx` | permanent | `failed` |
| timeout or crash after `DATA` was sent | unknown | `unknown`, no automatic retry |

Backoff: 30 s, 1 min, 2 min, 5 min, 10 min, 30 min, then hourly until the retry window (default 24 h) ends.

The `535` rule is deliberate. Some relays treat it as permanent and delete queued mail while the credentials are broken.
Postwerk treats a broken login as an outage on its own side.

## 5. Store

Two tables in one SQLite file.

```
mails  (id, received_at, sender, envelope_from, envelope_to, message_id, ref, size,
        raw,            -- removed after hand-off
        status, attempts, next_attempt_at, last_reply, provider_id, provider_status, mirrored_at)

events (seq, time, mail_id, kind, level, attempt, smtp_code, smtp_text, provider_status, actor)
```

- `events` is append-only. Triggers reject `UPDATE` and `DELETE`; only the retention job may remove old rows.
- No message content and no recipient addresses in `events`.
- Retention: raw message until hand-off, recipient address replaced by a keyed hash after a configurable period
  (lookups hash the query the same way), events kept for a configurable period.

SQLite settings: WAL, `synchronous=FULL`, one writer goroutine, busy timeout. The store must sit on a local disk, not on a
network file system.

## 6. Event model

Every state change is one event. All sinks receive the same shape.

```json
{
  "seq": 1042,
  "time": "2026-11-13T09:41:07Z",
  "level": "error",
  "kind": "upstream.auth_failed",
  "mail_id": "pw_01JCX4…",
  "attempt": 3,
  "smtp": { "code": 535, "text": "5.7.8 authentication failed" },
  "upstream": "mailjet",
  "sender": "auth-service",
  "instance": "prod"
}
```

Kinds (first set): `mail.accepted`, `mail.rejected`, `mail.attempt`, `mail.retry`, `mail.handed_off`, `mail.failed`,
`mail.unknown`, `mail.resent`, `upstream.auth_failed`, `upstream.unreachable`, `provider.delivered`, `provider.bounced`,
`provider.blocked`, `provider.complained`, `provider.stuck`, `mirror.failed`, `queue.stale`, `retention.purged`.

## 7. Sinks

A sink is one interface: take an event, export it, return an error or nothing. Each sink keeps its own cursor (`seq`) in
the store, so a sink that is down catches up later and nothing is lost.

| Sink | Format | Default |
|---|---|---|
| stdout | one JSON line per event | on |
| OpenTelemetry | OTLP logs, plus a few metrics (queue size, oldest queued, attempts by result) | off; on when an endpoint is set |
| Sentry protocol | error events grouped by `kind`; works with Sentry-compatible tools such as GlitchTip | off; on when a DSN is set |
| Webhook | JSON `POST`, signed per the Standard Webhooks spec (HMAC-SHA256) | off; on when a URL is set |

Each sink has a filter (`level`, `kind`).

## 8. Query API

Internal listener, bearer tokens with scopes `read` and `admin`. Token use is itself logged as an event.

| Call | Returns | Scope |
|---|---|---|
| `GET /v1/mails/{id}` | status, attempts, last reply, provider status, events | read |
| `GET /v1/mails?ref=…` / `?message_id=…` | the same, found by the sender's own reference or `Message-ID` | read |
| `GET /v1/mails?to=…&since=…&status=…` | list | read |
| `GET /v1/events?after=…` | event log from a cursor, for export and audit | read |
| `GET /v1/stats` | queued, oldest queued, results of the last 24 h | read |
| `POST /v1/mails/{id}/resend` | new attempt for `failed` or `unknown`, while the raw message exists | admin |
| `GET /healthz`, `GET /readyz` | alive; store writable and delivery loop running | none |

The API never returns message content.

Senders can set `X-Postwerk-Ref: <their id>` to find a mail by their own reference later. Postwerk also returns its id in
the SMTP reply (`250 2.0.0 queued as pw_…`).

## 9. UI

A small, read-only web UI on the internal listener: search, status and history of a mail, queue overview, recent
errors. Server-rendered HTML, no JavaScript build, no message content. Resend only with an admin token. Not meant to be
exposed to the internet.

## 10. Provider status

After hand-off, an adapter asks the provider what happened. Polling, not webhooks, so Postwerk needs no public endpoint.

Mailjet (first adapter): Postwerk sets `X-MJ-CustomID: <postwerk id>` and looks up the message with
`GET /v3/REST/message?CustomID=…` (statuses include `queued`, `sent`, `deferred`, `softbounced`, `hardbounced`,
`blocked`, `spam`). Planned schedule: about 10 min, 1 h and 24 h after hand-off; stop at a final status.

## 11. Mirror

Optional: a copy of every mail to a capture tool such as Mailpit, on the first attempt, best effort, no retry. Off by
default. Senders can exclude a mail with `X-Postwerk-Mirror: no`, for example mails that contain credentials.

## 12. Security

- Ingress requires `AUTH`; credentials per sender from a secrets file. Optional STARTTLS.
- Limits: message size, recipients per message, connections per sender.
- The query API and UI listen separately from ingress and are meant for an internal network only.
- The container runs as non-root with a read-only root file system except for the data volume.
- No content or recipient addresses in logs, events or error reports.

## 13. Configuration

Environment variables plus a secrets file. Example:

```
POSTWERK_SMTP_LISTEN=:2525
POSTWERK_HTTP_LISTEN=127.0.0.1:8080        # API and UI, internal only
POSTWERK_DATA=/data/postwerk.db
POSTWERK_SENDERS_FILE=/run/secrets/senders
POSTWERK_UPSTREAM=smtp://in-v3.mailjet.com:587
POSTWERK_UPSTREAM_CREDENTIALS_FILE=/run/secrets/upstream
POSTWERK_PROVIDER_STATUS=mailjet           # empty: no status lookups
POSTWERK_MIRROR=                           # e.g. smtp://mailpit:1025
POSTWERK_RETRY_WINDOW=24h
POSTWERK_OTEL_ENDPOINT=                    # empty: off
POSTWERK_SENTRY_DSN=                       # empty: off
POSTWERK_WEBHOOK_URL=                      # empty: off
```

## 14. Testing

- Every row of the retry policy is triggered on purpose against a fake upstream (Mailpit's chaos mode can return chosen
  SMTP errors).
- Crash during `DATA`, restart, and proof that no mail is sent twice.
- A sink that is down catches up after it comes back.
- `docker compose` example with Mailpit as upstream for local runs.

## 15. Open points

- Upstream reply texts and rate limits of real providers: to be measured.
- Provider API rate limits for status lookups.
- Single instance: during a restart the ingress is unavailable for a few seconds. A Postgres store for two instances
  can be added later if needed.
