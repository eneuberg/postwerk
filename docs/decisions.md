# Design decisions

Short records of what was decided and why. Newest at the bottom.

## D1 Go

The hardest part to get right is the SMTP server. Go has a maintained library for both sides
(`emersion/go-smtp`, MIT). In Rust, sending is well covered (`lettre`), but the server-side crates are small and have
not been updated in years, so most of the protocol would have to be written by hand. Go also gives a single static
binary.

## D2 SMTP is the only ingress

Every framework, auth service and shop plugin can send SMTP, so any application can use Postwerk without new code.
Mails arrive finished, which keeps Postwerk content-blind. An HTTP ingress would need its own message format and would
make Postwerk assemble mails.

## D3 Content-blind

Postwerk forwards the message unchanged, apart from one tracking header. It never renders templates or builds links.
Security-critical content (password reset links, one-time tokens) stays with the system that creates it.

## D4 Store first, at most once

`250` is sent only after the message is stored. After `DATA` has been sent upstream, an unclear outcome becomes
`unknown` and is not retried automatically. Losing track is reported; sending twice is avoided.

## D5 `535` holds the queue

A rejected upstream login is our own configuration error, not a property of the mail. Postwerk keeps retrying within
the retry window and raises an error immediately, instead of failing the mail.

## D6 SQLite, one instance

One file on a local volume, no database server, works anywhere. WAL mode with `synchronous=FULL` makes every accepted
mail durable before `250`. Throughput far exceeds typical transactional volumes. Trade-off: one instance only, so the
ingress is unavailable for a few seconds during restarts. A Postgres store can be added later for high availability.

## D7 Event log as the single source for sinks

Every state change is an event in an append-only table. Sinks read it with their own cursor, so no sink needs its own
queue and nothing is lost while a sink is down.

## D8 OpenTelemetry as the standard export, Sentry protocol for errors

OpenTelemetry (OTLP) is the vendor-neutral interface for logs and metrics. Error trackers such as GlitchTip speak the
Sentry protocol, so that stays as its own sink. stdout JSON is always on. Sinks are off until configured.

## D9 Status by polling the provider

Polling the provider's API needs no public endpoint and also returns states that webhooks may not report (for example
deferred). Webhooks may be added later.

## D10 Internal UI, read-only

A small server-rendered UI for status and history, on a listener meant for internal networks only. It never shows
message content.

## D11 MIT license

## D12 A list of targets instead of one upstream and a mirror

Delivery goes to every configured target, and each target is tracked on its own. This covers production (a provider),
tests (a capture tool such as Mailpit) and both at once with one mechanism, instead of a main path plus a best-effort
mirror. A broken target does not block the others. Senders can skip named targets per mail with a header.

## D13 Unclear outcome is a per-target policy

After `DATA`, a broken connection leaves it open whether the target has the mail. `hold` waits for an admin, `check`
asks the provider and resends only if the mail is not found there, `retry` resends at once. The default is `check` where
a status adapter exists and `hold` otherwise, so a duplicate is only risked where it is checked first or does no harm.

## D14 `250` means stored, not delivered

Postwerk replies `250` once the mail is stored, like every SMTP relay. Delivery is reported later through the status API,
the UI and events. This is stated in the README so that senders do not read their own "sent" flag as "delivered".
