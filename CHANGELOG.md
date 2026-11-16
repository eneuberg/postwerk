# Changelog

All notable changes to Postwerk are listed here. The format follows [Keep a Changelog](https://keepachangelog.com),
and versions follow [semantic versioning](https://semver.org) as described in [docs/versioning.md](docs/versioning.md).

<!--
Add each change under Unreleased, in the matching group: Added, Changed, Deprecated, Removed, Fixed, Security.
One short line per change, written for operators. At a release, Unreleased becomes the version with its date.
-->

## [Unreleased]

No version has been released yet. This section lists what exists today.

### Added

- SMTP ingress with `AUTH PLAIN`, one login per sending application, and STARTTLS. The reply
  `250 2.0.0 queued as pw_…` comes only after the mail is stored durably in one SQLite file.
- Delivery to a fixed list of targets (`smtps://`, `smtp://` with STARTTLS, `smtp+plain://`), each target and each
  recipient tracked on its own, with derived statuses including `partial`.
- Retries with backoff within a retry window; a rejected target login (`535`) keeps mails queued and raises an error
  at once.
- Per-target settings: optional targets, retry window, the policy for an unclear outcome (`hold`, `retry`) and a
  recipient allow-list. Headers `X-Postwerk-Ref` and `X-Postwerk-Skip` for senders.
- Envelope parameters passed on: `SMTPUTF8`, `8BITMIME` and `SIZE`, with `delivery.unsupported` and
  `delivery.too_large` when a target cannot take a mail. 15 MiB default size, with exact room for Postwerk's own
  headers.
- Memory budget for messages being received (`POSTWERK_MAX_INFLIGHT_BYTES`).
- Graceful shutdown that finishes a send in progress.
- Append-only event log, exported as JSON lines on stdout and, optionally, to Sentry or GlitchTip (Sentry protocol,
  no SDK). Each sink runs on its own and catches up after an outage.
- Retention: stored messages removed after hand-off, addresses pseudonymized after 30 days, records and events
  deleted after 90 days; all configurable.
- Internal web UI on a loopback admin listener: overview per target, mail list with search, one mail with its
  timeline, and accounts with passwords under Settings. A tokens file gives first and break-glass access.
- Read-only JSON query API under `/v1/` on its own listener, with `read` and `admin` bearer tokens.
- STARTTLS and API certificates reloaded from their files without a restart, with `tls.*` events before they expire.
- Commands `hash-password`, `gen-key`, `gen-token`, `gen-api-token` and `version`.
- Container image from `scratch`, a Helm chart and a Docker Compose example with Mailpit.
- Release workflow for a signed multi-platform image on GHCR with SBOM and provenance.

### Security

- Stored messages encrypted at rest with AES-256-GCM; keys only from files, with rotation by key id.
- Recipient addresses replaced by keyed hashes (HMAC-SHA256) after the pseudonymization period.
- No message content in events, logs, the UI or the API; addresses removed from reply texts before they are stored.
- TLS with certificate checks towards every target; cleartext only with `smtp+plain://`, which refuses credentials.
- Login hardening on SMTP and the UI: PBKDF2 at equal cost for every check, a limit on concurrent checks, login
  deadlines and throttling of failed UI logins.
- UI protected against cross-site attacks: CSRF tokens, a `Host` check against DNS rebinding, strict security headers,
  no JavaScript. Every login, search, opened mail and account change is an event.
- Query API tokens stored as SHA-256; addresses and the `Message-ID` masked for `read` tokens; rate limits; every
  request and failed authentication is an event.
- Helm chart closed by default: one replica, ClusterIP only, NetworkPolicy, non-root, read-only root file system, no
  service account token.
- Build inputs pinned by digest; releases only from tags on `main`.

[Unreleased]: https://github.com/eneuberg/postwerk/commits/main
