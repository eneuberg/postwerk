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
start. Unknown sender names cost the same time as known ones, so a login attempt does not reveal which names exist
(D26 makes this exact).

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

## D23 Envelope parameters are passed on, not dropped

Postwerk is store-and-forward: it ends the sender's SMTP transaction and starts its own towards each target. Whatever
the sender declared in `MAIL` would be lost unless Postwerk keeps it. It stores `BODY=`, `SMTPUTF8` and `SIZE=` with
the mail and passes them to every target that offers the extension; `SIZE=` carries the exact size of the message as
Postwerk sends it, which is more accurate than the sender's estimate. A mail that needs an extension the target does not
offer fails there with `delivery.unsupported` instead of being sent without it: a target that never agreed to UTF-8 or
8-bit data may mangle it, and a quietly broken address or subject is worse than a visible failure.

"Needs" is decided from the data, not only from the declaration. Go's `net/smtp` (and with it gomail) declares
`SMTPUTF8` whenever the server offers it, so after Postwerk offers it, practically every mail from such a client carries
it. A pure ASCII mail loses nothing without the parameter and is sent. `8BITMIME` is needed whenever the message holds
an 8-bit byte, declared or not. The SMTP library declares `BODY=8BITMIME` to every target that offers it; 7-bit content
is valid 8BITMIME, so this is harmless, and a declared `BODY=7BIT` is therefore not repeated as such.

`DSN` and `REQUIRETLS` are not offered: Postwerk cannot promise delivery status notifications or TLS on every hop for
every target, and offering an extension it cannot honour would be a silent lie. Mailjet's relay (checked
2026-11-15) offers `8BITMIME`, `SMTPUTF8`, `SIZE 15728640` and no `DSN`.

## D24 One status per (mail, target, recipient); delivery and mail status are derived

A target answers every `RCPT` on its own, and a provider such as Mailjet keeps one message per recipient (its Send API
returns one message id per recipient, a `/v3/REST/message` entry has one contact). Tracking a delivery as a whole
would have to merge different answers into one status, and a 4xx for one recipient would hold back or resend the mail
for all others. So the unit of tracking is the triple (mail, target, recipient), and its status is the single source
of truth. Scheduling and attempts stay per (mail, target), because one SMTP transaction carries several recipients;
every result, every retry decision and every event is per recipient. Events name the recipient by its position in the
envelope, never by its address, so they stay free of personal data.

Postwerk behaves like an RFC 5321 relay: `DATA` goes to the accepted recipients, a `5xx` at `RCPT` fails one
recipient, a `4xx` keeps it queued, and only queued recipients go into the next attempt, so nobody gets the mail
twice because somebody else was deferred. A `552` at `RCPT` counts as temporary, as RFC 5321 section 4.5.3.1.10
asks, because servers use it for "too many recipients". An unclear outcome after `DATA` applies only to the recipients
of that attempt; refusals at `RCPT` are stored with `data_sent_at`, so a crash cannot make them unclear.

The status of a delivery or a mail is computed when reading: `queued` while any recipient is queued, `unknown` while
any is unknown, `handed_off`, `failed`, or `partial` when some were handed off and the others failed. `partial` is a
summary for logs and the later API, never a stored state: a stored summary could disagree with the rows it
summarizes. Retention follows the recipients too: the message is kept while any recipient may still be sent.

The recipient allow-list stays all or nothing per target (D8 in requirements.md): a partial copy to a test target
would list people in `To` and `Cc` who never got it.

## D25 15 MiB default message size

`POSTWERK_MAX_MESSAGE_BYTES` defaults to 15728640 bytes, the `SIZE` Mailjet's relay offers (checked 2026-11-15), instead
of 25 MiB. Postwerk answers `250` once a mail is stored; a mail it accepts but no target can take would fail only
later, after the sender believes it sent it. With the provider's limit as the default, the sender gets the `552` at
once. The limit also bounds memory per mail (D27). The only gap is Postwerk's own headers (about 50 to 100 bytes): a
message within those bytes of the limit fails at the target with `delivery.too_large`, visibly, instead of being sent
and bounced. Towards each target, Postwerk checks the `SIZE` it offers before sending and passes the exact size as
`SIZE=`.

## D26 Login hardening without extra packages

Three cheap measures, no new dependency:

- **Equal cost.** Every password check that is not a cached login costs PBKDF2 with the strongest iteration count in
  the senders file. An unknown name is checked against a dummy hash with that count; a sender whose hash has fewer
  iterations (an older line, made with a lower setting) pays the difference with extra rounds; a mismatched
  authorization identity pays the full cost too. The time of a failed login therefore says nothing about which names
  exist or how strong their hashes are, so names cannot be enumerated by timing. The cache of successful logins (an
  HMAC under a random in-memory key) stays: only the right password can take that path, so it reveals nothing an
  attacker does not already have.
- **Limited concurrency.** PBKDF2 at 600,000 iterations takes a CPU core for a noticeable time. At most
  `runtime.GOMAXPROCS(0)` checks run at once; a login waits up to 5 s for a slot and then gets `454 4.7.0`
  (temporary, RFC 4954). A burst of logins, by mistake or on purpose, then slows logins, not delivery and storage.
  Cached logins need no slot, so a well-behaved application with a connection pool is not affected.
- **Login deadline.** A connection that has not logged in within 30 s is closed, so idle unauthenticated connections
  cannot hold the connection limit. The SMTP library closes it with its fixed `421 4.4.2` idle reply; reads before the
  login, including a STARTTLS handshake, simply get that deadline.

No per-IP blocking or rate limit: in Kubernetes, pod addresses change with every restart, and many senders can share
a node's address behind NAT. Blocking by address would hit the wrong clients and miss the attacker. The network
policy (D28) limits who can connect at all.

## D27 A global memory budget for messages in flight

Ingress keeps each message in memory until it is sealed and committed. With 100 connections and 15 MiB messages, a
burst could ask for 1.5 GiB plus copies, more than a small container has, and the out-of-memory killer would end the
process for every sender at once. A global budget (`POSTWERK_MAX_INFLIGHT_BYTES`, default 256 MiB) turns that into a
`451 4.3.1` for the senders over the limit, which every SMTP client retries. A `DATA` reserves the maximum message
size, or the declared `SIZE` when smaller, so typical small mails do not block each other; a message that turns out
larger than declared must grow its reservation or gets the `451`. Reserving at `DATA` (not at `MAIL` or `RCPT`)
keeps idle transactions from holding memory. The library sends `354` before the check, so the refused message still
crosses the network, but it is discarded without being buffered. 256 MiB fits 17 messages at the maximum size and
thousands of typical transactional mails.
