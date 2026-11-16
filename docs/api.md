# Query API

A small JSON API for other applications inside the cluster, such as a support tool: "what happened to the mail for
this address, this id or this ref?". One can look up what happened to a given mail; nobody can read all mails through
it with a `read` token. An `admin` token can also close or resend a delivery (section 3, "Close and resend"); nothing
else changes through the API. It never returns message content. Requirements: H2, H4–H13 in
[requirements.md](requirements.md#http); reasons: decisions D32–D34 and D53–D55 in [decisions.md](decisions.md).

## 1. Turning it on

The API has its own listener, off by default:

| Variable | Default | What it does |
|---|---|---|
| `POSTWERK_API_LISTEN` | `off` | Address, for example `:8082`. Unlike the UI it may bind the pod address, because another pod calls it. |
| `POSTWERK_API_TOKENS_FILE` | required while on | Bearer tokens, one `name:role:hash` line per calling application. |
| `POSTWERK_API_HOSTS` | none (any `Host`) | If set, the only `Host` names the API answers to (any port); others get `403`. |
| `POSTWERK_API_TLS_CERT_FILE`, `POSTWERK_API_TLS_KEY_FILE` | none | TLS on the API listener; the pair is reloaded when the files change. Without TLS on a non-loopback address, a warning is logged at every start. |

Details: [configuration.md](configuration.md#query-api). In Kubernetes: `api.enabled=true` in the Helm chart
([kubernetes.md](kubernetes.md#51-a-client-of-the-query-api)).

**Tokens.** `postwerk gen-api-token <name> <read|admin>` prints two lines: the bearer value `<name>.<token>` for the
calling application, and the line for `POSTWERK_API_TOKENS_FILE`, `<name>:<role>:sha256$<digest>`: the SHA-256 of
the token, never the token itself (D41). Use one token per calling application, so the `actor` of every event names
it. The file is read once at start. A line with a `pbkdf2-sha256$…` hash, from a version before D41, stops the start
with a message that says to make a new token.

```sh
$ postwerk gen-api-token support-tool read
support-tool.Xb3k…43 characters…
support-tool:read:sha256$…43 characters…
```

Every request sends it as `Authorization: Bearer support-tool.Xb3k…`.

## 2. Roles

| | `read` | `admin` |
|---|---|---|
| `POST /v1/search` | yes, at most 20 mails | yes, at most 100 mails |
| `GET /v1/mails/{id}` | yes, addresses and the `Message-ID` masked (`j•••@e•••.com`, `<4711@s•••.com>`) | yes, in full |
| `GET /v1/mails` (list with filters) | no (`403`) | yes |
| `GET /v1/overview` (per target) | no (`403`) | yes |
| `POST /v1/mails/{id}/targets/{target}/close` | no (`403`) | yes |
| `POST /v1/mails/{id}/targets/{target}/resend` | no (`403`) | yes |

A `read` token can only answer "what happened to this mail?": every search is an exact match on one id, ref,
Message-ID or address, and there is no listing. After pseudonymization nobody sees an address (`"state":
"pseudonymized"`), but a search by address still finds the mail through the keyed hash.

## 3. Endpoints

All paths start with `/v1/`. Responses are `application/json`; times are RFC 3339 in UTC; a time that is not set is
left out. Statuses are the derived ones of the UI: `queued`, `sending`, `unknown`, `handed_off`, `failed`, `partial`,
`skipped`.

### Search: `POST /v1/search`

The search term goes in the body, never in the URL, so it stays out of access logs.

```http
POST /v1/search HTTP/1.1
Host: postwerk.mail.svc:8082
Authorization: Bearer support-tool.Xb3k…
Content-Type: application/json

{"by": "address", "query": "jane@example.com"}
```

`by` is `id`, `ref` (`X-Postwerk-Ref`), `message_id` (with or without angle brackets), `address` (recipient or
envelope sender) or `any` (the default; the address only if the query contains `@`). The query is at most 998 bytes.

```json
{
  "mails": [
    {
      "id": "pw_01JD3W8K2M4N6P8Q0R2S4T6V8X",
      "received_at": "2026-11-15T11:00:00Z",
      "sender": "shop",
      "size": 2345,
      "recipients": 2,
      "status": "partial",
      "targets": [{"target": "mailjet", "status": "partial"}]
    }
  ],
  "limit": 20,
  "truncated": false
}
```

`truncated` is `true` when more mails matched than `limit`. A search result holds no address.

### One mail: `GET /v1/mails/{id}`

The id is not personal data, so it may be in the URL. Example for a `read` token:

```json
{
  "id": "pw_01JD3W8K2M4N6P8Q0R2S4T6V8X",
  "received_at": "2026-11-15T11:00:00Z",
  "sender": "shop",
  "size": 2345,
  "ref": "order-4711",
  "message_id": "<4711@s•••.com>",
  "envelope_from": {"state": "masked", "address": "n•••@e•••.com"},
  "params": {"smtputf8": false},
  "stored": false,
  "raw_purged_at": "2026-11-15T11:00:02Z",
  "status": "partial",
  "recipients": [
    {"position": 0, "state": "masked", "address": "j•••@e•••.com"},
    {"position": 1, "state": "masked", "address": "m•••@e•••.org"}
  ],
  "deliveries": [
    {
      "target": "mailjet",
      "status": "partial",
      "attempts": 1,
      "last_code": 250,
      "last_text": "2.0.0 OK",
      "recipients": [
        {"position": 0, "status": "handed_off", "last_code": 250, "last_text": "2.0.0 OK"},
        {"position": 1, "status": "failed", "last_code": 550, "last_text": "5.1.1 <redacted>: mailbox unavailable"}
      ]
    }
  ],
  "events": [
    {"seq": 1, "time": "2026-11-15T11:00:00Z", "kind": "mail.accepted", "level": "info"},
    {"seq": 2, "time": "2026-11-15T11:00:01Z", "kind": "delivery.attempt", "level": "info", "target": "mailjet", "attempt": 1},
    {"seq": 3, "time": "2026-11-15T11:00:02Z", "kind": "delivery.handed_off", "level": "info", "target": "mailjet", "recipient": 0, "attempt": 1, "code": 250, "text": "2.0.0 OK"},
    {"seq": 4, "time": "2026-11-15T11:00:02Z", "kind": "delivery.rejected", "level": "warn", "target": "mailjet", "recipient": 1, "attempt": 1, "code": 550, "text": "5.1.1 <redacted>: mailbox unavailable"}
  ],
  "events_truncated": false
}
```

An address `state` is `full` (admin), `masked` (read), `pseudonymized` (no address) or `null_sender` (`MAIL FROM:<>`).
A recipient an admin closed has `"resolution": "confirmed"` (its status is `handed_off`) or `"given_up"` (status
`failed`); the field is left out otherwise. The events include the admin's `delivery.closed` and `delivery.resend`; for
an `admin` token they carry `"actor"`, the account or token that acted.
For a `read` token the `message_id` is masked too, because many libraries put a host name, and some an address, into
it: the part after the last `@` like an address's domain, and the part before it as well if it holds an `@` or an
encoded address (`jane=example.com`); angle brackets stay. A search by `message_id` still takes the exact value
(D44).
A mail's `status` (here and in search results and lists) is derived from the recipients of its required targets
only; a delivery to an optional target keeps its own `status` in `deliveries` but does not change the mail's
(design.md section 4). So `state=failed` lists a mail that failed at the provider even if its capture copy arrived.
Recipients are numbered by their position in the envelope, from 0, as in the events. SMTP texts are stored redacted.
At most 500 events are returned. An unknown id gets `404`. Every detail returned is an `api.view` event with the
token name and the mail id (section 5).

### List: `GET /v1/mails` (admin)

Filters as URL parameters, each at most once: `state`, `target`, `from` and `to` (days, `YYYY-MM-DD`, UTC), `page`
(from 1, 50 mails per page, newest first). Filters never hold an address.

```json
{"page": 1, "has_next": false, "mails": [{"id": "pw_01JD3W8K2M4N6P8Q0R2S4T6V8X", "…": "as in search"}]}
```

### Overview: `GET /v1/overview` (admin)

Per target: whether it is optional, whether its worker can send to it, recipients of mails received in the last 24
hours by status, mails queued now, the oldest queued; and the ten most recent `failed`, `unknown` or `partial` mails
(by their required targets).

```json
{
  "now": "2026-11-15T12:00:00Z",
  "since": "2026-11-14T12:00:00Z",
  "targets": [
    {
      "target": "mailjet",
      "configured": true,
      "optional": false,
      "state": "down",
      "state_since": "2026-11-15T11:57:12Z",
      "down_reason": "auth",
      "recent": {"failed": 1, "handed_off": 1, "queued": 0, "sending": 0, "skipped": 0, "unknown": 0},
      "recent_total": 2,
      "queued": 1,
      "oldest_queued_at": "2026-11-15T11:58:00Z",
      "oldest_queued_age_seconds": 120
    }
  ],
  "problems": [{"id": "pw_01JD3W8K2M4N6P8Q0R2S4T6V8X", "…": "as in search"}]
}
```

`state` is `up` or `down` for a configured target (D45); a target that has deliveries in the store but is no longer
configured has no `state`. `down` means the target rejected the login (`down_reason` `auth`) or could not be reached
(`unreachable`) and no mail goes out to it until a probe succeeds; `state_since` is when it went down, or when it was
last seen up again (or Postwerk started). The state is held in memory, so after a restart every target is `up` until
the first attempt says otherwise.

### Close and resend: `POST /v1/mails/{id}/targets/{target}/close`, `…/resend` (admin)

An admin's decision on one delivery (the mail at one target), for what Postwerk cannot finish alone, typically an
`unknown` delivery after a connection broke during `DATA` (D53–D55). Both act on every open recipient of that delivery
at once, are refused while a worker is sending it (`409 in_progress`, try again a moment later), and write one event
per recipient with the token name as `actor`, in the same transaction as the change.

**Close** takes the recipients that are `queued`, `unknown` or `failed` (and not closed yet):

```http
POST /v1/mails/pw_01JD3W8K2M4N6P8Q0R2S4T6V8X/targets/mailjet/close HTTP/1.1
Authorization: Bearer ops-tool.Xb3k…
Content-Type: application/json

{"resolution": "confirmed", "note": "found in the provider's message log"}
```

`resolution` is `confirmed` (checked at the target, it has the mail; the recipients become `handed_off`) or
`given_up` (they become `failed`). `confirmed` is refused while a recipient to close is `queued`
(`409 confirm_queued`): it was never handed to the target. `note` is optional, at most 200 characters of plain text on
one line; addresses in it are replaced by `<redacted>` before it is stored, in the event only. A closed recipient is
final: retention no longer turns an `unknown` one into `failed`, and nothing sends it again. If no other recipient of
the mail may still need the stored message, it is removed in the same transaction.

**Resend** takes the recipients that are `failed` (not closed) or `unknown`, queues them again and sends at once, with
the retries left in the retry window (counted from the mail's arrival):

```json
{"accept_duplicate": true}
```

`accept_duplicate` (default `false`) must be `true` if one of them is `unknown`: the target may already have the mail,
and a resend may deliver it twice (`409 duplicate_risk` otherwise). A resend needs the stored message. Postwerk keeps
it 72 h after a failure and 7 days at most (`POSTWERK_RAW_KEEP_AFTER_FAILURE`, `POSTWERK_RAW_MAX_AGE`) and never longer
for a resend; once it is gone: `409 message_removed`. The body is required, `{}` at least.

Both answer `200` with the positions changed and the delivery as in the mail's detail:

```json
{
  "id": "pw_01JD3W8K2M4N6P8Q0R2S4T6V8X",
  "target": "mailjet",
  "recipients": [1],
  "delivery": {
    "target": "mailjet", "status": "handed_off", "attempts": 1, "last_code": 250, "last_text": "2.0.0 OK",
    "recipients": [
      {"position": 0, "status": "handed_off", "last_code": 250, "last_text": "2.0.0 OK"},
      {"position": 1, "status": "handed_off", "resolution": "confirmed"}
    ]
  }
}
```

| Status | `error` | When |
|---|---|---|
| 400 | `bad_request` | unknown `resolution`, a note too long or not plain text, an invalid body |
| 404 | `not_found` | no such mail, or the mail has no delivery to this target |
| 409 | `in_progress` | a worker is sending this delivery right now |
| 409 | `nothing_to_close`, `nothing_to_resend` | no recipient in a state the action takes (a second resend finds the recipients queued) |
| 409 | `confirm_queued` | `confirmed` with a queued recipient |
| 409 | `duplicate_risk` | resend of an `unknown` recipient without `"accept_duplicate": true` |
| 409 | `message_removed` | resend after the stored message was removed |
| 409 | `target_not_configured` | resend to a target that is no longer configured (close it instead) |

### Errors

Every error is `{"error": "<code>", "message": "<text>"}`:

| Status | `error` | When |
|---|---|---|
| 400 | `bad_request` | invalid JSON, unknown field, more than one value, invalid `by` or filter, empty query, URL parameters on a `POST`, a body on a `GET` |
| 401 | `unauthorized` | missing, malformed or wrong token; with `WWW-Authenticate: Bearer realm="postwerk"` |
| 403 | `forbidden` | the role may not use the endpoint |
| 403 | `host_not_allowed`, `browser_refused` | `Host` not in `POSTWERK_API_HOSTS`; a request with `Origin` or `Sec-Fetch-*` headers |
| 404 | `not_found` | no such endpoint, mail or delivery |
| 405 | `method_not_allowed` | wrong method (with `Allow`) |
| 413 | `too_large` | body over 4 KiB |
| 409 | see above | a close or resend that is not possible now |
| 415 | `unsupported_media_type` | a `POST` without `Content-Type: application/json` (UTF-8) |
| 429 | `rate_limited` | over a rate limit; with `Retry-After` in seconds |
| 503 | `unavailable` | the request could not be written to the event log; with `Retry-After` |

## 4. Protection

- **Tokens.** A token is 32 random bytes, so it cannot be guessed, and the file holds only its SHA-256. Every check
  is one SHA-256 and one constant-time comparison, for unknown names too (against a random dummy), so wrong tokens
  cost no noticeable CPU and the time of a check does not tell which names exist (D41).
- **Rate limits** (fixed): 60 requests per minute per token name, and 600 per minute for all callers together,
  counted before the token check, so a flood of wrong tokens is bounded too. Over a limit: `429` with `Retry-After`.
- **No browsers.** No CORS headers, so a browser cannot call the API from a page; `OPTIONS` gets `405`; requests with
  `Origin` or `Sec-Fetch-*` headers get `403`.
- **Strict requests.** JSON only, at most 4 KiB, unknown fields refused. Server timeouts: 5 s for the headers, 10 s to
  read, 20 s to write, 60 s idle; headers at most 8 KiB.
- **Headers on every response:** `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'; base-uri
  'none'`, `X-Content-Type-Options: nosniff`, `Cache-Control: no-store`, `Referrer-Policy: no-referrer`,
  `X-Frame-Options: DENY`.
- **Network.** Postwerk is never reachable from outside the cluster. In Kubernetes the NetworkPolicy lets in only the
  pods in `api.allowFrom`; with an empty list nobody gets in. TLS is on in the chart by default.

**Never returned:** message content, subject, headers other than `Message-ID`, anything from the stored message.

## 5. Audit

| Kind | Level | Carries |
|---|---|---|
| `api.request` | info | `actor`: the token name; `text`: endpoint, search kind, HTTP status and number of mails, for example `search by=address status=200 results=1` |
| `api.view` | info | `actor`: the token name, `mail_id`: the mail whose detail was returned; no text. Written instead of `api.request` for a successful `GET /v1/mails/{id}`, like the UI's `ui.view` |
| `api.forbidden` | warn | `actor`: the token name; `text`: the endpoint |
| `delivery.closed`, `delivery.resend` | info | `actor`: the token name, `mail_id`, `target` and the recipient's position; `text`: `resolution=… was=… via=api` and the note (close), `was=… via=api` (resend). Written instead of `api.request` for a close or resend that succeeded, in the same transaction as the change; a refused one is an `api.request` with its status |
| `api.auth_failed` | warn | `actor`: the name only if it is in the tokens file, otherwise empty; `text`: `failures=<n>`, the missing, malformed or wrong tokens of that actor in one minute. At most one per actor and minute (D42) |

Never the search term, a ref, a Message-ID or an address; a mail id only in `api.view`, for a mail that was returned
(a lookup of an unknown id is an `api.request` with status `404` and no id). So the event log answers "who looked at
this mail?" without becoming a list of what was searched for. A request is answered only after its event is
written; if that fails, the client gets `503` and no data. Failed authentications are not single events: the first
one of an actor starts a minute, the ones after it are counted, and one `api.auth_failed` with the count is written
when the minute ends or Postwerk stops. So a flood of wrong tokens writes at most one event per token name, plus one
for the rest, per minute, and the same to Sentry. Requests over a rate limit write no event. Events caused
by a token or a person (`api.view`, `ui.view`) are not part of a mail's own timeline in the detail response; a close
or resend is, because it changes the mail's state.

## 6. Code

`internal/api` holds the handler, the checks and the JSON shapes, on top of `internal/query` (the same read logic and
masking as the UI) and `internal/resolve` (the same close and resend as the UI). `postwerk gen-api-token` is in `cmd/postwerk`; the listener and its TLS (`internal/tlsreload`) are
set up there too.
