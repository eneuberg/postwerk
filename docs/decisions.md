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

## D15 Sender passwords as PBKDF2-SHA256 from the standard library

The senders file holds `name:pbkdf2-sha256$<iterations>$<salt>$<key>`, never a plaintext password. PBKDF2-SHA256 is in
Go's standard library (`crypto/pbkdf2`), so it adds no dependency. Argon2id or bcrypt would need
`golang.org/x/crypto`. `postwerk hash-password` uses 600,000 iterations; a hash with fewer than 100,000 is refused at
start. Unknown sender names cost the same time as known ones, so a login attempt does not reveal which names exist.

## D16 `AUTH PLAIN` only

`PLAIN` works with stored hashes and every SMTP client supports it. `LOGIN` is an older, non-standard variant of the
same exchange and adds nothing. Mechanisms that need the plaintext password on the server (such as `CRAM-MD5`) would
rule out hashed storage. Because `PLAIN` sends the password in the clear inside the SMTP session, `AUTH` is offered
only over TLS unless cleartext is explicitly allowed (D18).

## D17 Go 1.26 as the minimum version

The pure-Go SQLite driver (`modernc.org/sqlite`) in the version used requires Go 1.26. Staying on a pure-Go driver
keeps the binary static and free of cgo, which matters more than supporting older Go releases.

## D18 TLS by default, cleartext only by explicit choice

Towards targets, `smtps://` uses implicit TLS and `smtp://` requires STARTTLS; there is no silent fallback to
cleartext. Cleartext needs the separate scheme `smtp+plain://`, and that scheme refuses credentials. On ingress, `AUTH`
needs a TLS certificate unless `POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true` is set. The insecure case is possible (a local
Mailpit, a cluster network that encrypts node traffic), but it must be written down in the configuration and can be
found by a review.

## D19 Unknown `POSTWERK_*` variables are errors

A misspelled variable such as `POSTWERK_TARGET_MAILJET_OPTINAL` would otherwise be ignored, and the setting would
silently keep its default. Postwerk refuses to start and names every unknown variable, together with all other
configuration errors. Variables for features that are not built yet are known names, but a value for them is refused.

## D20 Encryption at rest with AES-256-GCM and key files

The raw message is encrypted per row with AES-256-GCM, with the mail id as additional authenticated data, so a
ciphertext cannot be moved to another row. Each ciphertext carries a key id. New mails use the newest key; older keys
stay in the key file for decryption until no row needs them. There is no per-mail data key (envelope encryption):
raw messages live hours, not months, so rotation by key id is enough and keeps the scheme small.

Keys are read from files, not environment variables. Environment variables are visible to more places (process
listings, `docker inspect`, crash reports, child processes) and cannot hold several keys with ids in a readable way.
`postwerk gen-key` creates a new key. Address hashing uses its own key file, so one leak does not give both.

## D21 Retention periods are configurable, with recommended defaults

Defaults: the raw message is removed when every delivery is handed off or skipped, 72 h after a failure, and in any
case after 7 days; recipient addresses are replaced by a keyed hash after 30 days; mail records and events are removed
after 90 days. These are reasoned defaults for a typical support process, not legal requirements. The GDPR sets
principles (data minimisation, storage limitation), not fixed numbers. Operators with their own rules change the
values. The reasons for each period are in [privacy-and-security.md](privacy-and-security.md#3-lifecycle-and-retention).

## D22 Capture targets are for development and testing

A capture tool such as Mailpit stores full content, including password reset links and one-time codes. Postwerk
recommends it for development and tests and advises against it in production by default. Production audit and
tracing rely on Postwerk's own event log and, once built, its status lookup and error reporting, which keep no content.
