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
the retry window and raises an error immediately, instead of failing the mail. D45 moves the error and the retries to the
target: one error per outage, and probes instead of a login per mail.

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

A small server-rendered UI for status and history. It never shows message content. How it is reached and protected
is D29.

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
once. The limit also bounds memory per mail (D27). Postwerk's own header lines are taken off what ingress accepts
(D40). Towards each target, Postwerk checks the `SIZE` it offers before sending and passes the exact size as
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

## D28 Helm chart defaults: one replica, cluster-internal, deny by default

The chart encodes what the design needs, so a default install is a safe one:

- **One replica, `Recreate`.** One SQLite file has one writer (D6). A rolling update would start a second pod on the
  same volume; `Recreate` stops the old pod first, at the cost of a few seconds without SMTP, which senders retry.
- **ClusterIP only.** Postwerk relays mail in the name of the organization; reachable from the internet it would be an
  open door for anyone with one leaked sender password. The chart has no Ingress, `LoadBalancer` or `NodePort`
  switch on purpose. People reach the health port through a VPN or `kubectl port-forward`.
- **NetworkPolicy on.** SMTP only from the pods that send, HTTP only from named sources, egress only DNS and the
  submission ports 587 and 465. The target ports are open to any address by default because providers change their
  addresses without notice; a CIDR list is there for providers that publish ranges. Probes need no rule: Kubernetes
  NetworkPolicy does not block traffic from a pod's own node. Without per-IP rules in Postwerk (D26), the policy is
  the place that decides who may connect.
- **Secrets as files from existing Secrets.** Keys never pass through Helm values (which end up in release secrets
  and shell histories) or environment variables (D20). Mode `0440` with `fsGroup: 65532` makes them readable by
  Postwerk only.
- **No token, no service links.** Postwerk never calls the Kubernetes API, so the pod gets no service account token.
  Service links are off because they inject `POSTWERK_*`-looking variables that Postwerk refuses (D19).
- **Hardened pod.** Non-root uid 65532, read-only root file system, all capabilities dropped, seccomp
  `RuntimeDefault`; the image has no shell to begin with.
- **TLS by default, cert-manager optional.** STARTTLS from an internal CA, because common clients (Go's `net/smtp`)
  refuse to send a password without it. A renewed certificate is picked up without a restart (D38).

## D29 The UI on a loopback admin listener, reached through a tunnel, with a login anyway

The UI shows addresses and delivery history, so it must not be reachable by anyone who happens to be on the cluster
network. Options were a Service port with a NetworkPolicy, an Ingress with an identity proxy, or a loopback listener
reached through a tunnel. The tunnel wins: it adds nothing to the attack surface, needs no identity provider, and
reuses access control that already exists and is already audited.

- **Its own listener, `127.0.0.1:8081`.** The health listener must accept the kubelet's probes, so in containers it
  binds to `:8080`; sharing it would put the UI on the pod network. A separate loopback listener is reachable only
  from inside the pod's network namespace. `kubectl port-forward` ends there, so access is decided by Kubernetes RBAC
  (`pods/portforward`) and travels inside the API server's TLS connection. On a host, `ssh -L` does the same.
- **Docker needs an exception.** Docker's port publishing forwards to the container's interface, not to its loopback,
  so a loopback-only listener inside a container cannot be published at all. Instead of dropping the loopback rule,
  `POSTWERK_ADMIN_ALLOW_NON_LOOPBACK=true` allows another address explicitly, with a warning at every start; compose
  publishes the port on the host's `127.0.0.1` only. Kubernetes does not need it.
- **A login anyway.** A forwarded port on `localhost` is open to every process on the operator's machine, and inside
  the pod to every other container. Tokens use the same PBKDF2 hashes and login hardening as senders (D15, D26), so
  there is one scheme to review. The form asks for name and token: with the name, a login costs one PBKDF2 check;
  with the token alone, it would have to be checked against every line.
- **Sessions in memory.** One instance (D6), so nothing needs sharing; a restart logs everyone out, which is
  acceptable for an admin tool. Cookies are `HttpOnly` and `SameSite=Strict`, `Secure` only over TLS: the admin
  listener has no TLS of its own, and browsers treat `http://localhost` as a secure context.
- **Browser attacks.** A web page can make the operator's browser talk to `localhost`. A Host allow-list stops DNS
  rebinding; it accepts the loopback names with any port, because the port of a tunnel is the operator's choice
  and plays no part in the attack. CSRF tokens on every form, Go's cross-origin check, a strict
  Content-Security-Policy and no JavaScript at all cover the rest.
- **Addresses stay out of URLs.** Search is a `POST` whose results come back in the same response, so an address never
  reaches a URL, the browser history or an access log. Events record who searched and by what kind of key, never the
  query (privacy-and-security.md, changes 8 and 10).
- **Masking in the query layer.** `read` sees `j•••@e•••.com`, `admin` the full address. The masking happens where the
  data is read (`internal/query`), not in the templates, so the later JSON API cannot forget it. The UI later dropped
  the `read` role (D35); the masking stays for the API.

## D30 The Sentry protocol by hand, without an SDK

Sentry-compatible tools (Sentry itself, GlitchTip) are where many teams already get their alerts, so error events go
there directly (D8). An SDK would bring many indirect dependencies (O4, [scope.md gap 15](scope.md#7-gaps-between-the-ideal-and-the-current-design))
and defaults that collect more than Postwerk wants to send: breadcrumbs, request data, the host name, local
variables. What Postwerk needs is small: one `POST` of one envelope per event, with a key in a header.

- **Standard library only.** `report.Sentry` builds the envelope and the event itself from the event record, so there
  is no field that could carry an address or content (privacy-and-security.md, change 12). The payload was checked
  against the Sentry developer documentation (envelopes, authentication, event payloads, rate limiting) and against
  GlitchTip's ingest code, and sent to GlitchTip 7.0.0 in a local test.
- **One Sentry event per Postwerk event, grouped by kind and target.** The fingerprint is
  `["postwerk", kind, target]`, so "`delivery.failed` at `mailjet`" is one issue however many mails fail, and a new
  kind of trouble is a new issue that alerts. Reply texts differ per mail and would split issues if they were part of
  the grouping.
- **`mail_id` as a tag.** Tags are searchable in Sentry and GlitchTip, so an alert leads to the mail, and the mail to
  the UI (`ui_path` in the extra data). `sender` and `target` are tags for the same reason.
- **The DSN from a file.** Sentry calls the key public, but whoever has it can post events to the project and flood
  the alerts. It is handled like every other secret (D20): from a file, never from a variable, never logged.
- **`https` only, unless allowed.** `POSTWERK_SENTRY_ALLOW_HTTP=true` exists for local tests and a GlitchTip inside
  a cluster whose network is encrypted, as cleartext SMTP needs its own explicit setting (D18).

## D31 At least once to Sentry, never in the way of delivery

The Sentry SDKs drop events when the server is down or rate limits them. Postwerk already has every event in its log
and a cursor per sink (D7), so the Sentry sink keeps them instead and sends them later.

- **Deterministic event ids.** The id is a UUID made from instance, seq, time, mail id and kind. An event exported
  twice (a crash between export and cursor update) has the same id, and Sentry and GlitchTip keep one. Time and mail
  id are part of it so that a new database, which starts at seq 1 again under the same instance name, does not
  collide with events already sent.
- **Wait as asked.** `429` with `Retry-After`, and `X-Sentry-Rate-Limits` on any response, pause the sink as long as
  the server says (60 s for a bare `429`, at most an hour). Network errors, `5xx` and a refused DSN back off from 5 s
  to 5 minutes. A refused DSN is kept and retried like a rejected SMTP login (D5): it is our configuration error.
- **Skip what can never succeed.** An event refused with `400`, `413` or `422` is logged and skipped; sending it
  again would stop the sink for good.
- **Every sink on its own.** The reporter runs each sink in its own loop, so a slow Sentry holds up neither stdout nor
  other sinks, and delivery only ever sends a non-blocking wake signal. One request takes at most 10 s; while the sink
  waits, it makes no requests at all. The final export at shutdown is capped at 10 s, so closing the store always
  has time left.
- **Events below the level** move the cursor at once, so the sink never re-reads them.

## D32 The query API on its own listener, reachable from the cluster network

H2 first said "on the admin listener". The admin listener is loopback only (D29): people reach it through a tunnel,
and nothing in the cluster can. The API's callers are other pods, such as a support tool, so it needs an address
those pods can reach. Moving the admin listener to the pod address would put the UI on the cluster network as well.

- **A separate listener, `POSTWERK_API_LISTEN`, off by default.** It may bind the pod address. Only the API is on it,
  and only operators who need it turn it on; the UI stays loopback only.
- **Who may connect is the NetworkPolicy's job.** In the chart the API gets a Service port only when enabled, and an
  ingress rule only from the pods in `api.allowFrom`; an empty list lets nobody in. As for SMTP, Postwerk itself has no
  per-IP rules (D26).
- **TLS, on by default in the chart.** Bearer tokens and full addresses would otherwise cross the pod network in
  cleartext. The API has its own certificate variables, so it does not depend on STARTTLS being on; the chart uses
  the SMTP certificate by default, whose names are already the Service names. The pair is reloaded like the SMTP one.
  Outside the chart, a non-loopback API without TLS logs a warning at every start.
- **No browsers.** The callers are services. No CORS headers, so no web page can call it; requests that carry
  `Origin` or `Sec-Fetch-*` headers are refused. That also removes the reason for the UI's DNS-rebinding defence;
  the `Host` allow-list is optional, and the chart fills it with the Service names.

## D33 Bearer tokens `<name>.<token>`, from a file of their own

- **First: same format and checks as the UI tokens.** One `name:role:hash` line per calling application, PBKDF2 at
  the strongest iteration count for every name, at most one check per CPU, and a cache of tokens that passed. D41
  replaced this with a SHA-256 of the token: the tokens are random, so PBKDF2 protected nothing and let wrong tokens
  burn CPU.
- **The name travels with the token.** With the name, a token is checked against one line; with the token alone it
  would have to be checked against every line. The token is base64url and has no dot, so the last dot separates the
  two (names may contain dots). `postwerk gen-api-token` prints the whole value.
- **A file of its own, `POSTWERK_API_TOKENS_FILE`.** People and applications get different credentials, and a
  token for a service never works for the UI or the other way round. The UI's own login can change without
  touching the API.

## D34 Lookups for `read`, lists only for `admin`, and every request in the event log

The purpose is "what happened to the mail for this address, id or ref", not a copy of the mail history.

- **`read` can only look up.** Search by exact id, ref, Message-ID or address (no prefixes, no wildcards), at most 20
  mails per search, and one mail's detail, with addresses masked as in the UI. There is no listing and no overview,
  so a read token cannot page through all mails; the rate limit bounds guessing. `admin` gets lists, the overview and
  full addresses, for an operator's own tooling.
- **Search terms only in a `POST` body**, as in the UI, so they stay out of URLs and access logs. A mail id may be in
  the URL: it is random and not personal data. List filters never hold an address.
- **Every request is an event, answered only after the event is written.** `api.request` names the token, the
  endpoint, the search kind, the status and the number of mails, never the term; that is enough to see who looked
  up how much, without the log itself becoming a list of addresses. A role refusal is a `warn` event; failed tokens
  are counted and written as one `warn` event per token name and minute (D42). Requests over a rate limit write no
  event, so a flood cannot fill the log; the global limit counts requests before the token check, so a flood of wrong
  tokens is bounded too.
- **A returned detail names its mail.** The owner decided that opening a mail is an audit event, in the UI
  (`ui.view`) and the API alike: `api.view` carries the token name and the mail id, so "who looked at this mail?"
  has an answer. The id is random and not personal data; search terms still never reach the log. An unknown id
  gets no id in its event, so a mistyped or guessed id leaves no trace of the guess.
- **Limits are fixed for now**: 60 requests per minute per token and 600 in all. They can become settings once real
  use shows the need.

## D35 One kind of UI account: admin, and every opened mail is an event

The UI first had two roles, `read` with masked addresses and `admin`. It now has one: every account sees all pages and
every address in full, and may manage accounts.

- **Who reaches the UI.** Only people who can port-forward into Postwerk's pod or log in to its host (D29). They can
  already read the logs, the secrets or the volume, so a second, weaker role inside the UI protects little. Giving
  support staff that cluster access just to look up a mail would be the wrong trade.
- **Support belongs on the API.** Looking up what happened to one mail, without browsing all mails, is a job for a
  small tool on top of the JSON API (H2), which keeps its own `read` and `admin` tokens and the masking in
  `internal/query` (`query.Mask`). One role keeps the UI's code and its tests small.
- **The tokens file** keeps its format, `name:role:hash`, so old files and the API's parsing stay the same, but the UI
  accepts `admin` lines only. A `read` line is a configuration error rather than silently becoming admin.
  `postwerk gen-token <name>` makes admin lines; `gen-token <name> admin` still works.
- **Opened mails are events.** With one role, the audit is the counterweight: opening a mail's page writes `ui.view`
  with the account and the mail id before the page is shown, like searches (privacy-and-security.md, change 8). The
  mail's own timeline leaves out events with an actor, so viewing it does not change it.

## D36 Passwords for UI accounts, kept in the database and set in the UI

Generating tokens into a file and restarting for every new person is clumsy, and nobody can choose or change their
own secret. Accounts with passwords now live in the SQLite file (`ui_accounts`, migration 0006) and are managed on
the UI's account pages.

- **Same hash as senders** (D15): PBKDF2-SHA256, 600,000 iterations, from the standard library; no new dependency
  (O4). Never the password itself, in the table, a log or an event.
- **Password rules from NIST SP 800-63B-4** (August 2025, section 3.1.1.2, <https://doi.org/10.6028/NIST.SP.800-63B-4>):
  passwords used as a single factor "SHALL" have at least 15 characters; verifiers "SHALL NOT impose other composition
  rules" and "SHOULD permit a maximum password length of at least 64 characters"; each Unicode code point counts as
  one character. Postwerk takes 15 to 128 code points and refuses the account name, the context-specific word the
  guideline names. Not built: a check against a list of breached passwords (it needs a shipped list or a network call)
  and Unicode normalization (it needs `golang.org/x/text`); a password typed in another normalization form fails.
- **Re-entering your own password for every change**, so a session left open on a laptop cannot add an account or take
  one over. The check counts towards the same throttle as logins.
- **Changes end sessions.** Setting a password or deleting an account ends all of its sessions at once, so an old
  password or a removed person loses access immediately. The last account in the database cannot be deleted.
- **Throttling, not lockout.** NIST asks for rate limiting (section 3.2.2: no more than 100 consecutive failures on
  one account). A hard lockout would let anyone who reaches the login lock out every admin by name. Postwerk slows
  down instead, in memory: 5 free failures per name, then waits of 1 minute doubling to 15, about 100 tries a day; 20
  per source address, since everybody behind `kubectl port-forward` is `127.0.0.1`. Unknown names are throttled the
  same way and every check costs the same (D26), so neither the time nor the throttle tells which names exist. A
  restart forgets the counts, which an attacker cannot cause from the login page.

## D37 The tokens file stays as start and break-glass access

A fresh install has no accounts in its database, so the first admin has to come from somewhere. Options:

- **A `postwerk set-password` command** run with `kubectl exec deploy/postwerk -- /postwerk …`. Rejected: it is a
  second process writing the SQLite file while the server runs (D6 assumes one writer), it cannot end the server's
  in-memory sessions, it needs exec rights in the pod, and the password would travel through a terminal and maybe a
  shell history.
- **A setup page that lets the first visitor create an admin.** Rejected: whoever reaches the port first wins,
  including another container in the pod.
- **The existing tokens file.** Chosen. It exists already, as a Secret in the Helm chart, and needs nothing new. Log in
  once with a token, create accounts with passwords, then remove the line or the file, or keep it stored safely to get
  back in if every password is lost. Its accounts are shown as "tokens file" and cannot be changed in the UI. On a
  name in both places the file wins, with a warning at start.

With the admin listener on and neither a tokens file nor an account in the database, Postwerk does not start, with a
message that says what to do; a UI nobody can log in to would be a silent misconfiguration (O1).

## D38 TLS certificates reload from their files, checked every minute

cert-manager renews a certificate long before it expires, and Kubernetes swaps the files of a Secret volume through a
symlink. A process that reads the certificate only at start would keep serving the old one until it expires; then
in-cluster clients, which need TLS for `AUTH PLAIN`, could no longer send. Options were a restart on every renewal (an
extra controller or a cron job, and a short outage), file system notifications, or polling.

- **Polling, by content, every 60 s.** Notifications on a symlink swap are easy to miss (the watched path itself does
  not change), and the modification time of the target can stay the same. Reading two small files once a minute
  costs nothing and catches every way of replacing them. Kubernetes takes up to about a minute to update the volume,
  so faster polling would gain little.
- **Validate before swapping.** A new pair must parse, the key must match and the certificate must still be valid.
  Otherwise the old certificate stays: a broken renewal must not take TLS down while the old certificate still works.
  A pair read in the middle of a replacement is read once more after 2 s before it counts as failed.
- **Report, not spam.** `tls.reload_failed` once per content, `tls.reloaded` per reload, `tls.expiring` once a day
  under 14 days. cert-manager renews 15 days ahead, so the warning means the renewal did not arrive.
- **One package for every listener.** `internal/tlsreload` has no Postwerk dependencies; the STARTTLS ingress uses it,
  and any later TLS listener uses the same code. At start an invalid pair still stops Postwerk (O1).

## D39 Signed images on GHCR, no `latest`

Operators need an image they can pull and check, without building it themselves. The release workflow runs on a tag
`v<semver>` only; nothing is published from a branch.

- **GHCR, keyless cosign.** The image lives next to the code, and the workflow's own `GITHUB_TOKEN` pushes it. cosign
  signs with a short-lived certificate for the workflow's OIDC identity, so there is no signing key to store, rotate or
  leak. A verifier checks issuer and identity (`release.yml@refs/tags/v…`), which ties the image to this repository
  and that tag. The signature covers the index digest, and with it both platforms, the SBOM and the provenance.
- **SBOM and provenance from BuildKit**, attached to the image index. No extra action is needed for them, and the
  scanner image is pinned by digest. The workflow uses four actions (checkout, setup-go, setup-buildx, the cosign
  installer), each pinned by commit; login, build and signing are plain commands. The tests run in a job of their
  own with read access only; the job that pushes and signs starts after it and runs no project code but the build.
- **Two platforms, no emulation.** The build stage runs on the build machine and cross-compiles (`GOOS`, `GOARCH`);
  the final stage is `scratch`, so `linux/arm64` costs one more `go build`.
- **No `latest`.** A moving tag hides which version runs, and `IfNotPresent` never pulls the new one. Users pin a
  version, better a digest. The chart takes its tag from `appVersion`, and the workflow refuses a tag that does not
  match it, so chart and image cannot drift.
- **Publishing stays a manual step.** A release starts only when the maintainer pushes a tag.

## D40 The size limit is the target's, Postwerk's header lines are taken off exactly

Postwerk adds `X-Postwerk-Id` to every message and `X-MJ-CustomID` for Mailjet. With ingress and target at the same
15 MiB, a message just under the limit got `250` and then failed at the target with `delivery.too_large`: the sender
believed it was sent. Options were a fixed margin (1 KiB, a default of 15727616), a separate setting for the margin,
or computing it.

- **Computed, not a margin.** The added lines have a fixed length, because every mail id has 29 characters: 46 bytes
  each. Ingress accepts the configured value minus the largest sum over the configured targets, 92 bytes with a
  Mailjet target. Nothing to tune, and nothing is given away.
- **One setting, in the provider's terms.** `POSTWERK_MAX_MESSAGE_BYTES` is the largest message a target receives,
  so operators copy the provider's limit (the `SIZE` in its `EHLO`, or its documentation) and are done. A separate
  margin setting would make them do the arithmetic. Anyone who wants more room sets a lower value.
- **Counted as sent.** The SMTP client turns every bare LF into CRLF, so a message with LF line ends grows on the
  way out. Ingress and the `SIZE=` towards targets count a bare LF as two bytes, which keeps the check exact for those
  messages too. Dots added for transparency do not count (RFC 1870).
- The SMTP library's `DATA` reader refuses a message of exactly the offered `SIZE`; the largest one through `DATA` is
  one byte less. That errs on the safe side, so Postwerk leaves it.

## D41 API tokens as SHA-256, not PBKDF2

The API first hashed its tokens like the UI tokens, with PBKDF2 at 600,000 iterations (D33). A slow hash makes a
guessable secret, a password, expensive to brute-force from a leaked file. An API token is not guessable: it is 32
bytes from the operating system's random source, made by `postwerk gen-api-token`. Trying 2^256 values is impossible
with any hash, so PBKDF2 added no protection. It did add cost: every wrong token, up to the global limit of 600 a
minute, took a CPU core for a noticeable time, and with a 1-CPU limit that slowed SMTP logins and delivery.

- **Plain SHA-256 of the token, from the standard library.** The line is `name:role:sha256$<digest>`. A keyed hash
  (HMAC) would only matter if the digest let someone find the token, which it does not for a random 256-bit value;
  its key would be one more secret to create, store, rotate and lose, and losing it would invalidate every token.
  SHA-256 is what services that issue random tokens commonly store, and it needs no new dependency (O4).
- **Equal steps for every check.** One SHA-256 of what was sent and one constant-time comparison with 32 bytes: the
  name's digest, or a random dummy for an unknown name. The time does not tell which names exist.
- **No cache and no check slots.** Both existed only to bound PBKDF2; with one SHA-256 per request there is nothing
  to save. The rate limits (D34) stay.
- **Old lines are refused.** Postwerk has had no release yet, so a `pbkdf2-sha256$…` line in the API tokens file
  stops the start with a message that says to make a new token. Accepting both schemes would keep the slow path, and
  with it the reason for this change. UI tokens and UI passwords are typed or pasted by people and stay on PBKDF2
  (D36, D37); the UI tokens file keeps its format.

## D42 Failed API authentications are counted, one event per token name and minute

Every failed authentication was an `api.auth_failed` event, a malformed header too. At the global limit of 600
requests a minute that is about 860,000 events a day: enough to fill the volume over time, and each a `warn` event,
so each also went to Sentry.

- **One event per actor and minute, with the count.** The first failure of an actor starts a window of one minute;
  the failures after it only count; when the window ends, one `api.auth_failed` event is written with
  `failures=<n>` as text. At shutdown the open windows are written at once, before the final export. Someone
  watching still sees which token fails and how often, within a minute.
- **The actor stays as before.** The token name if it is in the file, otherwise empty: unknown names, malformed
  headers and missing tokens share one count. So the counts held in memory are bounded by the lines in the file
  plus one, whatever an attacker sends, and a token pasted into the name part still never reaches the log.
- **Not a sampling.** Every failure is counted; nothing is dropped. The `401` itself does not wait for the event.

## D43 Build inputs pinned by digest, releases only from `main`

D39 pinned the actions by commit and the SBOM scanner by digest, but other inputs of the release could still change
under the same name: the Dockerfile frontend (`# syntax=docker/dockerfile:1`) and the BuildKit image that
`setup-buildx` starts (`moby/buildkit:buildx-stable-1`) both run with the build, and a tag pushed on any branch
would have been released.

- **Everything by digest.** The frontend (`docker/dockerfile:1.27.1@sha256:…`, the version `1` pointed to), the Go
  build image (`golang:1.27@sha256:…`) and BuildKit (`moby/buildkit:v0.34.2@sha256:…`, the version
  `buildx-stable-1` pointed to, through `driver-opts`) are fixed, like the scanner and the CI tool images. The final
  stage is `scratch`, which is no image and needs no pin. The digests were read from the registry
  (`docker buildx imagetools inspect <image>:<tag>`), not copied from elsewhere.
- **Dependabot keeps them current**, weekly, and only with releases at least a week old, so a hijacked release has
  time to be noticed before a pull request proposes it. It covers the actions, the Dockerfile's `FROM` line and the Go
  modules. It does not read the `# syntax=` line, `driver-opts` or images in workflow variables; those (the frontend,
  BuildKit, helm, kubeconform, the SBOM scanner) are updated by hand with the command above. Renovate would cover them
  too, but needs an app with write access to the repository; Dependabot is part of GitHub and needs only its file.
- **A release tag must be on `main`.** The release workflow checks that the tagged commit is an ancestor of
  `origin/main` before the tests run, and stops otherwise. A tag on a branch nobody reviewed can no longer publish
  a signed image. The check runs in the job without write rights, so nothing is pushed or signed before it passes.

## D44 The Message-ID is masked for `read` API tokens

A `read` token saw addresses masked but the `Message-ID` in full (D34). Many mail libraries build it from the
sending host's name (`<…@app-7f9c.internal.example>`), and some put the recipient's address or a user id into it, so
in full it can tell a support tool more than masked addresses do.

- **Masked like an address.** The part after the last `@` keeps its first character and its top-level domain
  (`<4711@s•••.com>`). The part before it stays, because it is mostly a random id that support staff compare with
  the copy a recipient forwards, unless it looks like an address: it holds an `@`, or an address folded in the way
  bounce and tracking ids do (`jane=example.com`, `jane%example.com`). Then it is masked to its first character.
  Angle brackets stay, so the value still reads as a `Message-ID`.
- **Search unchanged.** A search by `message_id` matches the exact stored value; a caller who has the value from a
  recipient finds the mail, and the response does not hand it back in full.
- **In the query layer**, next to the address masking (`query.MaskMessageID`), so every reader with masked access
  gets it. `admin` tokens and the UI, whose accounts are all admins (D35), see it in full.

## D45 A rejected login or an unreachable target takes the target down, not each mail

D5 kept the mails queued on a `535`, but every queued mail still tried its own login: with four mails queued and a
wrong password, an end-to-end check saw 7 failed logins in about 90 s, 7 `target.auth_failed` error events and 7
reports in the error tracker. A provider that locks an account after repeated failed logins would have locked it,
and the alert said the same thing seven times.

- **The target has a state, up or down.** A `535`, or a failure while connecting (dial, TLS, greeting, `EHLO`,
  whatever the reply), is about the target, so it takes the target down at once. Its worker then sends nothing. A
  probe (connect, `EHLO`, `AUTH`, `QUIT`; never a mail) runs after 30 s and doubles to at most 5 min, both
  configurable. The first probe that works brings the target up and the queue resumes. The provider sees at most one
  login per probe.
- **One event per outage.** `target.down` (`error`, `warn` for an optional target) with the reason `auth` or
  `unreachable` and the redacted reply, and `target.recovered` (`info`) with how long it lasted. The two old kinds,
  `target.auth_failed` and `target.unreachable`, are gone, not kept beside them: one name per fact keeps the event
  schema and the alert rules simple. The mail whose attempt found the problem gets an ordinary `delivery.retry`
  (`warn`); the others get nothing until they are sent. There was no release, so no consumer of the old kinds needs
  a transition; old rows stay in the event log until retention removes them.
- **Down on the first failure, also for `unreachable`.** Waiting for a second failure would mean one more login with
  a wrong password, and for a network blip it saves only one event pair. A blip now gives `target.down` and, 30 s
  later, `target.recovered`; an error tracker can group or mute that pair, which is its job, not Postwerk's.
- **On contact, everything queued is due.** When a probe succeeds, and when a worker starts, every queued delivery
  of the target gets its next attempt now. A live test against Mailjet showed a mail waiting the rest of its backoff
  after the credentials were fixed and Postwerk restarted, up to an hour after a long outage. Telling the mails that
  waited for the outage apart from those in backoff for a `4xx` would need a stored reason; bringing all of them
  forward costs at most one early attempt each and is one rule for both cases.
- **The retry window still ends.** While down, the worker claims nothing, so it fails the queued recipients whose
  window is over itself (`delivery.failed`). Otherwise a mail could wait for days and go out after the target came
  back, which the retry window exists to prevent.
- **In memory only.** Storing the state would need a table, a migration and rules for a state left over from an
  older configuration. After a restart every target starts up and the first attempt finds out again, at the cost of
  one more `target.down` per restart while it is still broken. Readiness does not depend on it: a pod that is not
  ready takes no mail, which would turn an outage of the provider into one of Postwerk.
- The UI and the API overview show each target as up or down since a time, from the delivery workers through the
  query layer (`query.Options.Health`), so `query` still does not import `delivery`.

## D46 A refused recipient address is `delivery.rejected`, a warning

A typo in an address or a closed mailbox was `delivery.failed` at `error`, the same as a relay that refuses Postwerk
or a provider that rejects every mail. With alerts on `error`, a user mistyping their address paged the operator, who
can do nothing about it.

- **Own kind, level `warn`.** The recipient stays `failed`, so the mail's state, lists and the API are unchanged.
  Only the event differs, and with it the alert. `delivery.failed` keeps everything the operator must act on: other
  permanent replies, the end of the retry window, failures at `MAIL` or after the message that are not about an
  address.
- **Kept simple and narrow.** A rejection is a `550`, `551` or `553` whose enhanced status code says the address or
  the mailbox is wrong (`5.1.x`, `5.2.x`), or, at `RCPT`, such a code without an enhanced code. A `550 5.7.1` (relay
  denied, policy, spam) and any `554` stay `delivery.failed`: they are often the operator's configuration. After the
  message, a qualifying reply applies to the recipients of that attempt. Parsing the reply text for which recipient
  it names was not done: texts differ between providers and would need redacted addresses to be matched.
- Delivery status after hand-off and bounce suppression stay with the provider; this only names what the target said
  in the SMTP session.

## D47 Mailjet tracking is off by default

A Mailjet account with open or click tracking on rewrites every link through its tracking domain and adds a pixel,
password reset links included. Applications such as an auth service send through SMTP and cannot switch that off per
mail. Mailjet reads `X-Mailjet-TrackOpen` and `X-Mailjet-TrackClick` (`0` off, `1` on) on each SMTP message, over the
account's setting (dev.mailjet.com, "SMTP relay", custom headers).

- **`POSTWERK_TARGET_<NAME>_MAILJET_TRACKING`, default `off`, only with `STATUS=mailjet`.** `off` adds both headers
  with `0` and removes any the sender set, so no sender turns tracking back on by accident and the outcome does not
  depend on how the account is configured. `account` adds and removes nothing; the account's settings and the
  sender's headers apply. There is no `on`: turning tracking on is a marketing choice for the account or the sender.
- **Counted in the size margin**, like `X-MJ-CustomID` (D40): the two lines have a fixed length of 49 bytes, so
  ingress offers 49 bytes less for a Mailjet target with tracking off.
- This changes headers, which D3 keeps to Postwerk's own tracking lines; it changes no content and no link. It lets
  Postwerk keep the promise that links reach the recipient as the application wrote them.

## D48 A CA file per target replaces the system roots for that target

Only Go's `SSL_CERT_FILE` let Postwerk trust a relay with a private CA. It is undocumented in Postwerk, applies to
every target and to the Sentry sink, and replaces the system roots, so a bundle without the public roots breaks the
public provider.

- **`POSTWERK_TARGET_<NAME>_CA_FILE`**, a PEM bundle, read and checked at start: every PEM block must be a
  certificate that parses, at least one must be there, and `smtp+plain` with a CA file is an error (it would have no
  effect). It is not a secret, so it gets no permission warning.
- **Replaces, does not add.** A target behind a private CA should trust that CA, not every public one as well: adding
  would let any public CA vouch for a host name that is meant to be internal. The other targets and the Sentry sink
  keep the system roots. Anyone who wants both builds a bundle that holds both.
- The chart mounts it from a ConfigMap (`targets[].caConfigMap`).

## D49 Mail that waits too long is a signal of its own, measured by age

A provider outage, rotated credentials, a rate limit or a `4xx` that keeps coming back all end the same way for the
person waiting for a password reset: the mail does not arrive. `target.down` (D45) covers a target Postwerk cannot
log in to or reach; a rate limit or a `4xx` that keeps coming back left only `delivery.retry` (`warn`), which the
default Sentry filter does not see.

- **Age, not reachability.** Every minute, per target, the age of the oldest mail with a recipient `queued` or
  `sending`. Over `POSTWERK_QUEUE_STALE_AFTER`: `queue.stale` (error, warn for an optional target) with the count
  and the age; back under: `queue.recovered` (info). It does not care why a mail waits, so it also catches causes
  nobody wrote a check for. Whether a target is reachable is reported on the delivery side (D45–D48); the two stay
  independent, so either can change without the other.
- **15 minutes by default.** Longer than the first retries (30 s, 1, 2, 5 and 10 minutes), so a short hiccup passes
  without a page; about as long as a password reset link or a login code is useful, so it fires while the mail still
  matters.
- **One event per change.** No repeats by default: the issue stays open in the tool until someone resolves it.
  `POSTWERK_QUEUE_STALE_REPEAT` writes it again at an interval for tools that only notify on new events; repeating
  and escalating are the tool's job (D57).
- **Restarts keep the state.** The check starts from the newest `queue.*` event of each target, so a restart neither
  repeats `queue.stale` nor loses `queue.recovered`. The lookup at start has no index on the kind and reads the
  events from the newest back; an index would need a migration for a query that runs once per start.

## D50 A free-space reserve: refuse new mail before the volume is full

On a full data volume Postwerk answered `250` until a write failed, then `451 4.3.0`; `/readyz` stayed `200`, and the
only trace was an error log line, because no event could be written any more.

- **A reserve, checked with `statfs`.** Below `POSTWERK_MIN_FREE_BYTES` (64 MiB) ingress answers `452 4.3.1` at
  `MAIL` and `RCPT`, and after `DATA` when the message would take the volume below it, before anything is stored.
  Senders treat `452` as temporary and retry. `/readyz` fails, so Kubernetes stops sending new connections. Ingress
  asks on every command (one system call), so a burst cannot outrun the 10 s interval.
- **Written while it still fits.** `disk.low` (error) goes out while the reserve has room for it and for the export
  cursor. The reserve also covers the mails already accepted: delivering them writes state and events, and removing
  a delivered message overwrites its pages through the WAL (`secure_delete`), up to its size until the next
  checkpoint. Four messages of the default 15 MiB fit in 64 MiB; a reserve under twice the message size logs a
  warning. Bytes, not a percentage: what has to fit depends on the message size, not on the volume.
- **Hysteresis.** `disk.ok` (info) only at the reserve plus a quarter, so a volume at the line does not flap.
- **The store's own word counts too.** A write that fails with `SQLITE_FULL`, `IOERR`, `READONLY` or `CANTOPEN`
  (quotas, a file system remounted read-only) makes `/readyz` fail until a write succeeds; the check probes with a
  one-page write. That case has no event, because none can be written: the log line and the failing probe say it.
- **Other systems.** `statfs` is read on Linux, macOS and FreeBSD. Elsewhere the check logs that it is off, and the
  store's own write errors still make `/readyz` fail.

## D51 Refused SMTP logins and commands are counted, one event per sender, reason and minute

A wrong SMTP password (`535`), a message too large (`552`) or an address Postwerk cannot pass on (`553`) left only a
log line. These are the sending application's problems, but Postwerk is the only place that sees them, and an
application that swallows the error loses mail silently.

- **The pattern of D42.** The folding of `api.auth_failed` moved into `internal/fold` and serves both: the first
  refusal opens a one-minute window, the following ones count, the window's end writes one event with the count.
  At shutdown the open windows are written before the final export.
- **Two kinds.** `ingress.auth_failed` (`failures=<n>`) for failed `AUTH`, and `ingress.rejected`
  (`reason=<r> rejected=<n>`) for refused commands of a logged-in sender, with the reason as a fixed word:
  `too_large`, `bad_address`, `needs_smtputf8`, `too_many_recipients`, `line_too_long`. Both `warn`: the application
  got the error itself, and a wrong password from an attacker is no reason to wake anyone. With
  `POSTWERK_SENTRY_MIN_LEVEL=warn` they reach the application's team.
- **No password, no address.** The sender is named only if the login name is in the senders file, otherwise it is
  empty, so a password typed into the name field never reaches an event, and the open windows stay bounded by the
  file. Recipients and envelope senders are never in the event.
- **What is not counted.** A `SIZE=` above the limit at `MAIL` is answered by the SMTP library before Postwerk sees
  the command; a too large message sent without `SIZE=` is counted. The recipient limit moved from the library to
  Postwerk (same reply, `452 4.5.3`) so that it is counted.

## D52 `/readyz` means "can take and keep mail"; what needs a person reaches the default filter

- **Not ready** while the store cannot be written, the volume is below its reserve (D50) or the delivery workers are
  not running. A pod that would refuse every mail should not get new connections.
- **A target that is down does not count.** Neither probe fails for it: a restart cannot fix a provider, it would
  interrupt sends to the other targets, and Kubernetes restarting a pod in a loop while the queue fills helps nobody.
  The mails wait, and `queue.stale` (D49) says so.
- **Certificate expiry as an error a week ahead.** `tls.expiring` was a warning until expiry, so with the default
  `POSTWERK_SENTRY_MIN_LEVEL=error` nobody saw it in time. It stays a warning from 14 days (cert-manager renews 15
  days ahead, so the warning means the renewal did not arrive) and becomes an error below 7 days, at once when it
  crosses, then daily. The time left is rounded down ("9 days 23 hours", hours below two days), so it never promises
  more than there is.
- **`smtp` only for SMTP replies.** An event carries `"smtp": {code, text}` only when it has a reply code; every other
  text (a certificate, retention counts, a network error, a count of refusals) is `"text"`. Fixed before 1.0, because
  the event fields are covered by semantic versioning from then on (D56).
## D53 An admin closes a delivery; the resolution is stored beside the status

After a crash during `DATA` a delivery under `hold` is `unknown`, and the target probably has the mail. Without a way
to settle it, retention turned it into `failed` with `retention.raw_expired` after 72 h, so the final record said
"failed" for a mail that was most likely delivered, and nobody could correct it.

- **Close with a resolution.** An admin (UI account or `admin` API token) closes a delivery with `confirmed` (checked
  at the target, it has the mail) or `given_up` (nobody will send it), and an optional note of at most 200
  characters, redacted like every text. It takes every recipient of the delivery that is `queued`, `unknown` or
  `failed` and not closed yet, including one that raw expiry already turned into `failed`, which is exactly the case
  above. `confirmed` is refused while a recipient is `queued`: it never reached the target.
- **No new status.** `confirmed` makes the recipients `handed_off`, `given_up` makes them `failed`, and a new column
  `delivery_recipients.resolution` (migration 0007) records that a person decided it. The alternative, a status
  `closed`, would have needed a place in `store.Summarize`, the SQL filter, the overview counts and every reader, and
  still would not say whether the mail arrived. With the resolution beside the status, every summary stays as it is:
  a confirmed delivery counts as handed off, a given-up one as failed. A check in the schema keeps the two consistent.
  Who closed it, when and why is in the `delivery.closed` event, not in the row.
- **Final.** A closed recipient is never touched again: raw expiry only fails `queued` and `unknown` recipients, a
  claim takes only `queued`, and neither close nor resend takes a recipient with a resolution. A mistaken close is
  corrected outside Postwerk; the event log says what happened.
- **Content goes at once.** Closing removes the stored message in the same transaction when no recipient of the
  mail, at any target, may still need it (`queued`, `sending`, `unknown`, or `failed` and not closed). `Finish`
  counts closed recipients as done when it removes the message at the last hand-off.
- **Per delivery, not per recipient.** One SMTP transaction carries all recipients of a delivery, and an unclear
  outcome hits all recipients of that attempt alike, so the admin's check at the provider answers for all of them.
  Closing single recipients would need a selection in the UI and the API for a case not seen yet; it can come later.

## D54 Resend only while the message is stored, never longer

Postwerk should take on as little responsibility as possible (D57): it keeps no content longer than delivery needs,
and it is never the system of record for content; the sending application is.

- **Within the existing retention.** A resend queues the delivery's `failed` (not closed) and `unknown` recipients
  again, due at once, and is possible only while the encrypted message is still stored under the rules of S7
  (removed at hand-off, kept `RAW_KEEP_AFTER_FAILURE` after a failure, `RAW_MAX_AGE` at most). No action extends them.
  Once the message is gone, resend is not offered in the UI and gets `409 message_removed` from the API; the
  application sends the mail again if it is still needed. A resend that fails again counts its 72 h from that failure,
  like every attempt; the maximum age never moves.
- **An explicit duplicate.** For an `unknown` recipient the admin must accept that the mail may arrive twice (a
  required tick in the UI, `"accept_duplicate": true` in the API), because the target may have it. The form says to
  check at the target first and to close as confirmed if it has the mail. This keeps P2 (never send twice by accident):
  a duplicate is possible only by an admin's explicit choice.
- **The same retry window.** The resent recipients get the retries left in the window counted from the mail's
  arrival, no new window. Past it, the resend is one attempt; a temporary failure fails it again. A new window would
  have to be stored per delivery and would keep the mail in the queue past what the operator configured.
- **Only to a configured target**, because nothing else would send it.

## D55 Close and resend: admins only, checked in the transaction, recorded with the actor

- **Who.** UI accounts (all admins, D35) and `admin` API tokens; `read` tokens get `403` and an `api.forbidden`
  event. In the UI each action is a `POST` with the CSRF token and the account's current password, like an account
  change (U18), because it changes a mail record and can send mail; a wrong password counts against the login
  throttle. The API keeps its usual checks (method, JSON body, size, token, rate limit, role).
- **No race with a worker.** The store checks the rules (`store.CloseCheck`, `store.ResendCheck`) inside the
  transaction that makes the change, on the single connection every worker also uses. While any recipient of the
  delivery is `sending`, both are refused (`409 in_progress`), so a recipient is never in two attempts at once; a
  resend only takes recipients that are `failed` or `unknown`, so of several resends at once one queues the mail and
  the others find nothing. The UI and the API use the same functions to show what is possible, through
  `internal/resolve`, which both depend on; neither depends on `delivery`, a resend wakes the workers through a
  function passed in.
- **Recorded with the change.** Each changed recipient gets one event, `delivery.closed` (`resolution=… was=…
  via=ui|api` and the note) or `delivery.resend` (`was=… via=…`), with the account or token name as `actor`, in the
  same transaction. They belong to the mail's history, so its timeline shows them, unlike `ui.view` and `api.view`;
  the API shows their actor to `admin` tokens only. For the API they take the place of `api.request`, as `api.view`
  does for a returned mail; a refused request is an `api.request` with its status.

## D56 Semantic versioning from 1.0 covers the operator's contract

Operators upgrade a relay that holds their queue and audit trail. They need to know which upgrades are safe without
reading every commit. The rules are in [versioning.md](versioning.md).

- **Covered:** what an operator or a client writes against: environment variables and defaults, secret file formats,
  commands, SMTP behaviour, the `/v1` API, event kinds and their JSON fields, the Helm values, the health checks.
  Additions are minor; removals and breaking changes only in a major release, after a deprecation in a minor one.
- **Not covered:** the UI's HTML, log text, free text inside events, the database schema and `internal/` packages.
  The UI is for people; programs use the API. Keeping these free lets the UI and messages improve in any release.
- **Forward-only migrations.** The schema version lives in the file and every migration runs at start in its own
  transaction. An older binary refuses a newer database (as it did before). A down migration would be code that is
  rarely run and never tested against real data; a backup taken before the upgrade is the simpler way back.
- **Only the latest minor version is supported.** One maintainer cannot keep several branches patched; minor
  releases do not break setups, so moving up is the fix.

## D57 Postwerk takes as little responsibility as it can

Every duty Postwerk takes on is one more thing that must work for every operator, forever. Most duties around mail
already have a better home: the provider knows what happened after hand-off, the monitoring tool knows who to page,
the platform knows how to back up a volume.

- **Postwerk stores, forwards and records.** It reports facts with levels and leaves the rest to the tools around it.
- **Named in the docs.** [scope.md](scope.md#what-stays-with-you) lists what stays with the operator: alert routing,
  delivery status and bounce suppression at the provider, backups, DNS and DKIM, provider contracts and data
  processing agreements, certificates, network access, secrets.
- **New features are checked against it.** A suggestion that moves one of these duties into Postwerk needs a reason
  why the tool that has it today cannot do it.
