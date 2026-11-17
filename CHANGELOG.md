# Changelog

All notable changes to Postwerk are listed here. The format follows [Keep a Changelog](https://keepachangelog.com),
and versions follow [semantic versioning](https://semver.org) as described in [docs/versioning.md](docs/versioning.md).

<!--
Add each change under Unreleased, in the matching group: Added, Changed, Deprecated, Removed, Fixed, Security.
One short line per change, written for operators. At a release, Unreleased becomes the version with its date.
-->

## [Unreleased]

### Added

- A Statistics page in the UI (`/stats`): mails received per bucket by their state now, as stacked bars drawn on the
  server, for the last hour, 6 hours, 24 hours, 7 days, 30 days or a custom range in UTC, filtered by target and
  sending application; totals, success rate, latency p50 and p95, and tables per target (with down time) and per
  application (with refused logins, commands and rate-limit refusals).
- `GET /v1/stats` for `admin` API tokens with the same counts as JSON.
- Migration 10 adds two indexes for the statistics; counted hours are kept in memory (at most about 8 MiB).

- Sender keys: `postwerk gen-sender NAME` prints a random key (`pwk_<id>_<secret>`, the application's SMTP password)
  and its senders-file line, which holds only a SHA-256 of the key. One key per application, two at once for a
  rotation without downtime. Password lines from 1.0 keep working.
- A policy per sender in the senders file (`name:policy from=*@example.com max_per_minute=60 max_rcpt_per_hour=500`):
  an envelope sender outside `from=` gets `550 5.7.1` (`ingress.rejected reason=from_not_allowed`), a sender over a
  limit `451 4.7.1` (new event `ingress.rate_limited`, warn).
- `POSTWERK_SENDER_KEYS` (`file`, the default and 1.0's behaviour; `db`; `both`): senders can also be kept in the
  database, with create, rotate, revoke and policy changes as events with the actor (`sender.created`,
  `sender.rotated`, `sender.revoked`, `sender.policy_changed`).
- UI: **Settings → Sender keys** lists every sender of the file and the database with its keys, policy, last login and
  the last 24 hours; a page per sender. Database senders are created, rotated, revoked (one key or all) and given a
  policy there, with the admin's password; the new key is shown once, with the SMTP settings. File senders are
  read-only. Senders are not deleted: "Revoke all keys" switches one off.
- API: `GET /v1/senders`, `GET /v1/senders/{name}`, `POST /v1/senders` (the key in the response, once),
  `POST /v1/senders/{name}/rotate`, `…/keys/{id}/revoke`, `…/revoke-all`, `PUT /v1/senders/{name}/policy`, for
  `admin` tokens, with error codes such as `name_taken`, `too_many_keys`, `file_sender`. Senders can be provisioned
  from CI (docs/api.md).
- The mail list filters by sender and key id (UI; API: `sender`, `key_id`); a mail's page shows the key, client
  address, TLS, `EHLO` and `AUTH` mechanism it came with, and so does an admin token's mail detail (`audit`).
- Every mail records the key it came with, the client address, TLS version and cipher, the `EHLO` name and the
  `AUTH` mechanism; `mail.accepted` carries `source=… key=… client=…` as text. Removed with the mail record.
- `sender.volume_high` (error) when a sender sends far more than its own average of the past week
  (`POSTWERK_SENDER_VOLUME_FACTOR`, `POSTWERK_SENDER_VOLUME_MIN`, `POSTWERK_SENDER_VOLUME_MAX`).
- The senders file is re-read every 30 s: a valid change is used without a restart (`senders.reloaded`), an invalid
  one is not (`senders.reload_failed`, error). A removed or revoked credential ends its open sessions at the next
  `MAIL`.
- The chart value `senderKeys` (and `senderVolume`) for the new settings.

### Changed

- Database schema version 9 (two migrations: managed senders, per-mail audit columns). As always, there is no way
  back to an older version except a backup.
- A senders-file password that has the exact form of a sender key (`pwk_` followed by 12 and 52 base32 characters)
  is now checked as a key. No password made by `hash-password` or chosen by a person has that form.
- The UI has a new design ([docs/design-system.md](docs/design-system.md)): colour only for state, each status a lamp
  with its own shape and its word, a summary on top of the overview, a route to each target and a timeline on a
  mail's page, mail lists as blocks on phones. Pages, forms and addresses are unchanged.
- The UI embeds the Barlow typeface (SIL Open Font License 1.1) and its Content-Security-Policy gains
  `font-src 'self'`. Nothing is loaded from outside.
- New logo: one track branching into three, each ending in a lamp.

### Fixed

- A second Postwerk on the same database can no longer run alongside the first. `kubectl delete pod` and evictions
  start the new pod while the old one is still stopping, `Recreate` or not. Postwerk now takes an exclusive lock on
  `postwerk.db.lock` next to the database and waits for it up to `POSTWERK_DATA_LOCK_WAIT` (default `150s`, chart
  value `dataLockWait`), logging the holder every 5 seconds, then exits with status 1. The lock is released after the
  final export at shutdown, and by the kernel if the process dies.
- The health listener opens before the store, so `/healthz` answers while Postwerk waits for the lock or runs
  recovery, and `/readyz` fails until it can take mail.

### Security

- A revoke or policy change of a database sender whose read-back failed left the old state in memory, so a revoked key
  kept working until the next successful change. Now the keys the change was about stop at once (fail closed) until
  a later read succeeds.
- A senders file written in place could be read half-written and still be valid, dropping a sender's policy line
  until the next check 30 s later. A change is now used only once it reads the same twice, 2 s apart.
- The statistics cache also counts its entries: requests for ever new sender or target names (which need not
  exist) cached an empty entry per hour of the range each and grew the memory without limit (D80).
- A policy's `from=` patterns also bind the message's `From:` header: every address in it must match, and a missing,
  doubled or unparseable `From:` is refused (`550 5.7.1` at the end of `DATA`, nothing stored,
  `ingress.rejected reason=from_not_allowed`), so a leaked key cannot show another sender (D81). Without `from=`
  nothing changes.

## [1.0.0] - 2026-11-17

The first release.

### Added

- SMTP ingress with `AUTH PLAIN`, one login per sending application, and STARTTLS. The reply
  `250 2.0.0 queued as pw_…` comes only after the mail is stored durably in one SQLite file.
- Delivery to a fixed list of targets (`smtps://`, `smtp://` with STARTTLS, `smtp+plain://`), each target and each
  recipient tracked on its own, with derived statuses including `partial`.
- Retries with backoff within a retry window.
- Target health: a rejected login (`535`) or an unreachable target takes the target down. Mails stay queued, one
  `target.down` event is raised, and the target is probed (log in, no mail) from 30 s up to every 5 minutes
  (`POSTWERK_PROBE_INTERVAL`, `POSTWERK_PROBE_MAX_INTERVAL`) until `target.recovered`; then, and after every
  start, the queued mails go out at once. The UI and the API overview
  show each target as up or down.
- `target.down` for an unreachable target only after the first probe failed too
  (`POSTWERK_TARGET_DOWN_AFTER_PROBES`, default 1, `0` at once), so a blip writes no event pair; a rejected login
  (`535`) is still reported at once (D58).
- `delivery.rejected` (warn) for a recipient address the target refuses, apart from `delivery.failed` (error).
- A mail's state comes from its required targets only; optional targets keep their own delivery state.
- Mailjet open and click tracking switched off per mail by default (`POSTWERK_TARGET_<NAME>_MAILJET_TRACKING`).
- `POSTWERK_TARGET_<NAME>_CA_FILE` for a target with a private CA.
- Per-target settings: optional targets, retry window, the policy for an unclear outcome (`hold`, `retry`) and a
  recipient allow-list. Headers `X-Postwerk-Ref` and `X-Postwerk-Skip` for senders.
- Envelope parameters passed on: `SMTPUTF8`, `8BITMIME` and `SIZE`, with `delivery.unsupported` and
  `delivery.too_large` when a target cannot take a mail. 15 MiB default size, with exact room for Postwerk's own
  headers.
- Memory budget for messages being received (`POSTWERK_MAX_INFLIGHT_BYTES`).
- Graceful shutdown that finishes a send in progress.
- Append-only event log, exported as JSON lines on stdout and, optionally, to Sentry or GlitchTip (Sentry protocol,
  no SDK). Each sink runs on its own and catches up after an outage. An event carries `smtp` only when it has an SMTP
  reply code; any other text is in `text`.
- Operator signals: `queue.stale` when a target's oldest queued mail waits longer than 15 minutes
  (`POSTWERK_QUEUE_STALE_AFTER`), `disk.low` with `452` and a failing `/readyz` below a free-space reserve on the data
  volume (`POSTWERK_MIN_FREE_BYTES`, 64 MiB), and counted `ingress.auth_failed` and `ingress.rejected` events for
  refused SMTP logins and commands. `/readyz` also fails while the store cannot be written; never because a target is
  down. A suggested alert routing is in design.md, "Alerting".
- Retention: stored messages removed after hand-off, addresses pseudonymized after 30 days, records and events
  deleted after 90 days; all configurable.
- Internal web UI on a loopback admin listener: overview per target, mail list with search, one mail with its
  timeline, and accounts with passwords under Settings. A tokens file gives first and break-glass access.
- JSON query API under `/v1/` on its own listener, with `read` and `admin` bearer tokens.
- Close and resend of a delivery by an admin, in the UI and with `admin` API tokens: close as `confirmed` or
  `given_up` (a closed `unknown` no longer turns `failed` when its message expires), resend while the message is
  still stored, with explicit consent for a possible duplicate.
- STARTTLS and API certificates reloaded from their files without a restart, with `tls.*` events before they expire:
  a warning from 14 days, an error from 7 days.
- Commands `hash-password`, `gen-key`, `gen-token`, `gen-api-token` and `version`.
- Database migrations at start, forward only; the start log names the schema version and the migrations applied.
- Container image from `scratch`, a Helm chart and a Docker Compose example with Mailpit.
- Release workflow for a signed multi-platform image on GHCR with SBOM and provenance.

### Security

- Helm chart sets `hostNetwork`, `hostPID` and `hostIPC` to `false` explicitly, for policy checks that do not accept defaults.
- Stored messages encrypted at rest with AES-256-GCM; keys only from files, with rotation by key id.
- Recipient addresses replaced by keyed hashes (HMAC-SHA256) after the pseudonymization period.
- No message content in events, logs, the UI or the API; addresses removed from reply texts before they are stored.
- TLS with certificate checks towards every target; cleartext only with `smtp+plain://`, which refuses credentials.
- Login hardening on SMTP and the UI: PBKDF2 at equal cost for every check, a limit on concurrent checks, login
  deadlines and throttling of failed UI logins.
- UI protected against cross-site attacks: CSRF tokens, a `Host` check against DNS rebinding, strict security headers,
  no JavaScript. Every login, search, opened mail, account change, close and resend is an event; close and resend ask
  for the account's password again.
- Query API tokens stored as SHA-256; addresses and the `Message-ID` masked for `read` tokens; rate limits; every
  request and failed authentication is an event.
- Helm chart NetworkPolicy: every egress rule names its peers. "Any address" is written out as `0.0.0.0/0` and
  `::/0` (the default for the target ports and Sentry); an empty `dns.to`, `targetCIDRs` or `sentry.cidrs` fails the
  render instead of opening the port to every address. `networkPolicy.egress.sentry.enabled: false` drops the 443
  rule for a GlitchTip inside the cluster (D59).
- Helm chart closed by default: one replica, ClusterIP only, NetworkPolicy, non-root, read-only root file system, no
  service account token.
- Build inputs pinned by digest; releases only from tags on `main`.
- `X-Postwerk-Id` is written after Postwerk's other header fields, so a message that starts with a continuation line
  cannot append to `X-Mailjet-TrackClick` and turn Mailjet tracking back on (D60).
- `ALLOW_RECIPIENTS` refuses an address with a character that only lower-cases to ASCII (the Kelvin sign), which
  would otherwise match an ASCII pattern (D60).

[Unreleased]: https://github.com/eneuberg/postwerk/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/eneuberg/postwerk/releases/tag/v1.0.0
