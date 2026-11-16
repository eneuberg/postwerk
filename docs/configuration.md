# Configuration

The single reference for every setting. Postwerk is configured only through `POSTWERK_*` environment variables and
the files they name (O1). Every `POSTWERK_*` variable must be known: a typo stops the start. All problems are listed
together, and the process exits with status 2. Error texts name variables and files, never secret values.

**Recommended** marks a default that is the recommended value from
[privacy-and-security.md](privacy-and-security.md) section 3.2 and 8.1. Change it only with a reason, and write the
reason into your records of processing.

Durations use Go syntax (`90s`, `30m`, `72h`) or days with `d` (`7d`), also combined (`1d12h`). Booleans are `true` or
`false`; switches are `on` or `off`.

## Listeners and TLS

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_SMTP_LISTEN` | `:2525` | `host:port` or `:port` | Address of the SMTP server (ingress). |
| `POSTWERK_HTTP_LISTEN` | `127.0.0.1:8080` | `host:port` or `:port` | Address of the HTTP listener with `/healthz` and `/readyz`. |
| `POSTWERK_ADMIN_LISTEN` | `127.0.0.1:8081` (**recommended**) | a loopback `host:port`, or `off` (or empty) | The admin listener with the internal UI (section "Admin listener and UI"). |
| `POSTWERK_API_LISTEN` | `off` | `host:port`, `:port`, or `off` (or empty) | The JSON query API for other applications (section "Query API"). |
| `POSTWERK_HOSTNAME` | `postwerk` | a host name | Name in the SMTP greeting and in `EHLO` towards targets. |
| `POSTWERK_SMTP_TLS_CERT_FILE` | none | path to a PEM file | Certificate for STARTTLS on ingress. Set together with the key file. |
| `POSTWERK_SMTP_TLS_KEY_FILE` | none | path to a PEM file | Private key for STARTTLS. Secret file. |
| `POSTWERK_SMTP_ALLOW_INSECURE_AUTH` | `false` | `true`, `false` | Accept `AUTH` over cleartext SMTP. |

Security notes:

- **Certificate renewal.** Postwerk re-reads both TLS files every minute and serves a new pair to the next
  connection, without a restart. It compares the content, so a Kubernetes Secret update (a symlink swap) is picked
  up. A new pair must parse, the key must match and the certificate must not have expired; otherwise Postwerk keeps
  serving the old one and writes `tls.reload_failed` (once per content). Each reload writes `tls.reloaded`. When the
  served certificate has less than 14 days left, `tls.expiring` is written once a day. At start an invalid pair stops
  Postwerk as before.
- Either set both TLS files (then `AUTH` is only offered after `STARTTLS`), or set
  `POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true` explicitly. Setting neither is a configuration error, and so is setting
  both (ambiguous). Cleartext `AUTH` sends sender passwords readable to anyone on the network path: use it only on a local
  machine or a network that encrypts traffic between nodes.
- **Health probes in containers.** The default HTTP address is the loopback interface, which is right for a process on
  a host. In a container, probes come from outside the loopback (the kubelet, Docker's health check from the
  host network), so containers set `POSTWERK_HTTP_LISTEN=:8080`, as [examples/compose.yaml](../examples/compose.yaml)
  does. Never publish this port to the internet. The UI and the query API are not on it: the UI has its own admin
  listener on the loopback interface, the API its own listener.
- **Kubernetes.** The Helm chart in `deploy/helm/postwerk` sets all of this (one replica, `Recreate`, ClusterIP only,
  secrets as files, NetworkPolicy); see [kubernetes.md](kubernetes.md). Pods need `enableServiceLinks: false`:
  service links add variables such as `POSTWERK_PORT` for a Service named `postwerk`, and unknown `POSTWERK_*`
  variables stop the start.

## Senders

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_SENDERS_FILE` | required | path | Applications allowed to send. One line per sender: `name:pbkdf2-sha256$<iterations>$<salt>$<key>`. Empty lines and `#` lines are ignored. Names: `[a-z0-9][a-z0-9._-]{0,63}`. |

Create a line with `printf '%s' 'password' | postwerk hash-password` (reads the password from stdin, prints the hash).
The file holds hashes only, but it is still a secret file: a copy allows offline guessing.

## Admin listener and UI

The internal web UI ([ui.md](ui.md)) runs on its own listener, never on the health listener. It is meant to be opened
through a tunnel (`kubectl port-forward`, `ssh -L`), so it binds to the loopback interface, also inside containers.

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_ADMIN_LISTEN` | `127.0.0.1:8081` (**recommended**) | a loopback address (`127.0.0.0/8`, `[::1]`, `localhost`) with a port; `off` or empty turns it off | Address of the admin listener. Another address is a configuration error unless the next variable is `true`. Must differ from the health and SMTP listeners. |
| `POSTWERK_ADMIN_ALLOW_NON_LOOPBACK` | `false` (**recommended**) | `true`, `false` | Allow a non-loopback admin address. Needed only inside a Docker container whose port is published on the host's loopback (`127.0.0.1:8081:8081`): Docker's port publishing cannot reach the container's own loopback. A warning is logged at every start. |
| `POSTWERK_UI_TOKENS_FILE` | none; needed until the UI has an account in the database | path | Start and break-glass access to the UI. One line per account: `name:admin:pbkdf2-sha256$<iterations>$<salt>$<key>`; every UI account is an admin, a `read` line is an error. Names: `[a-z0-9][a-z0-9._-]{0,63}`. Empty lines and `#` lines are ignored. Read once at start. Secret file. |
| `POSTWERK_ADMIN_HOSTS` | none | comma-separated host names or IP addresses, no port | Host names the UI answers to besides `localhost`, `127.0.0.1` and `[::1]` (any port). Every other `Host` gets `403`, against DNS rebinding. Needed only if people reach the UI under another name, for example through a jump host. |

Create a token with `postwerk gen-token <name>`: it prints a random token (32 bytes, base64url) on the first line and
the tokens-file line on the second. Postwerk keeps only the hash. Log in with it once, create accounts with passwords
under **Settings**, then remove the line or the file if you like ([ui.md](ui.md#2-accounts-and-login)). Accounts with
passwords live in the database (`POSTWERK_DATA`), not in a variable. With the admin listener on and neither a tokens
file nor an account in the database, Postwerk does not start. `POSTWERK_UI_TOKENS_FILE`, `POSTWERK_ADMIN_HOSTS` or
`POSTWERK_ADMIN_ALLOW_NON_LOOPBACK` set while the admin listener is `off` is a configuration error (ambiguous).

Fixed, not configurable: sessions end after 30 minutes without a request and 8 hours after the login; passwords have
15 to 128 characters; at most one password check per CPU at once (a login waits up to 5 s, then gets "try again");
after 5 failed checks for one name (20 from one address) further logins wait 1 minute, doubling up to 15 minutes; at
most 30 searches per minute per account.

## Query API

The read-only JSON API ([api.md](api.md)) for another application, such as a support tool, on its own listener. Off by
default. Unlike the admin listener it may bind a pod address, because another pod calls it.

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_API_LISTEN` | `off` | `host:port` or `:port`; `off` or empty turns it off | Address of the API listener. Must differ from the health, SMTP and admin listeners. |
| `POSTWERK_API_TOKENS_FILE` | required while the API is on | path | Bearer tokens, one line per calling application: `name:role:sha256$<digest>`, the SHA-256 of the token (D41); a `pbkdf2-sha256$…` line from an older version is an error. Role `read` (lookups only, addresses masked) or `admin` (also lists and the overview, addresses in full). Secret file. |
| `POSTWERK_API_HOSTS` | none (any `Host`) | comma-separated host names or IP addresses, no port | If set, the only `Host` names the API answers to (any port); every other `Host` gets `403`. The Helm chart sets the Service names. |
| `POSTWERK_API_TLS_CERT_FILE` | none | path to a PEM file | Certificate for TLS on the API. Set together with the key file. Re-read every minute; a renewed pair is served without a restart. |
| `POSTWERK_API_TLS_KEY_FILE` | none | path to a PEM file | Private key for TLS on the API. Secret file. |

Create a token with `postwerk gen-api-token <name> <read|admin>`: the first line is the bearer value `<name>.<token>`
for the calling application, the second line goes into the tokens file. Any of the other variables set while the API
is `off` is a configuration error (ambiguous). Without TLS on an address that is not loopback, a warning is logged at
every start: bearer tokens and addresses would cross the pod network in cleartext.

Fixed, not configurable: 60 requests per minute per token name and 600 for all callers together (then `429` with
`Retry-After`); failed authentications as at most one `api.auth_failed` event per token name and minute; bodies at
most 4 KiB; a `read` search returns at most 20 mails.

## Store and keys

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_DATA` | `/data/postwerk.db` | path on a local disk | The SQLite database. Created with mode `0600`; its WAL file sits next to it. |
| `POSTWERK_STORE_KEY_FILE` | required, unless encryption is `off` | path | Keys that encrypt stored messages at rest (AES-256-GCM). |
| `POSTWERK_STORE_ENCRYPTION` | `on` (**recommended**) | `on`, `off` | `off` stores messages in plaintext: local development only. A warning is logged at every start. `off` together with a store key file is a configuration error. |
| `POSTWERK_HASH_KEY_FILE` | required while `POSTWERK_PSEUDONYMIZE_AFTER` > 0, so required by default | path | Keys for the keyed hashes that replace addresses after the pseudonymization age, and for lookups by address. |
| `POSTWERK_SECURE_DELETE` | `on` (**recommended**) | `on`, `off` | SQLite `secure_delete`: deleted content is overwritten with zeros. `off` saves some write I/O and leaves deleted content readable in free pages. |

### Key files

Both key files have the same format, but they must hold **different keys** (the same key in both is a configuration
error, so one leak does not give both):

```
# comments and empty lines are ignored; the keys below are illustrations, never use them
20261115a7k2:3q2+7w8nRZcT0mJ0b0yQ1m3a4Wc5Jw0E3fJ6k9Zq2yA=
20260801x9p4:Vt1pZ7c3Jk0qFQ5n6Yb2m8Rw4aL0sE9dH2xU7gC1oNk=
```

- One key per line: `<key-id>:<base64 of 32 bytes>`. Key id: 1 to 16 characters of `a-z` and `0-9`. Standard base64
  with padding (44 characters).
- The **first** key line is the active key: new data uses it. Later lines are older keys, kept for reading
  (store key) or lookups (hash key).
- Create a line with `postwerk gen-key` (32 bytes from the operating system's random source; the id is the date and
  four random characters). Run it once per file.
- Every ciphertext and every hash carries its key id, so rotation needs no migration:
  1. Put a new line from `gen-key` at the top, keep the old line below, restart.
  2. Store key: remove the old line once `POSTWERK_RAW_MAX_AGE` has passed since the restart; by then no stored message
     uses it. A message whose key was removed fails with `store.decrypt_failed` and is never sent.
  3. Hash key: keep the old line as long as hashes made with it must stay searchable (up to
     `POSTWERK_RECORD_RETENTION`). Removing it makes those hashes unsearchable for Postwerk, on purpose.

**Why files and not environment variables.** Keys are read once at start, from files only. Environment variables
leak too easily: `docker inspect` and orchestrator APIs show them, `/proc/<pid>/environ` exposes them to anyone with
the same user or `ptrace` rights, child processes inherit them, and crash reporters and debug dumps often include them.
A mounted secret file has its own permissions and does not appear in any of these. Postwerk has no setting that takes
a key as a value.

**File permissions.** Secret files (senders, key files, target credentials, TLS key) should be readable by the
Postwerk user only (`0400` or `0600`). Postwerk logs a warning at start for a secret file that others can read, or that
a group can read which Postwerk is not a member of. It does not refuse to start: Kubernetes secret volumes show `0644`
by default, and refusing would push operators to weaker workarounds. In Kubernetes set `defaultMode: 0440` with
`fsGroup: 65532`, or `0400`.

**Encryption off.** With `POSTWERK_STORE_ENCRYPTION=off`, messages are stored in plaintext. After switching
encryption on, messages stored before are refused (`store.decrypt_failed`), because a plaintext blob cannot be told
apart from a damaged one; let the queue drain before switching. Messages stored encrypted cannot be read with
encryption off.

## Retention

A retention job runs at start (before any delivery) and then every 10 minutes. Each run writes one `retention.purged`
event with counts only and checkpoints the WAL (`PRAGMA wal_checkpoint(TRUNCATE)`), so deleted content does not linger
in the WAL file.

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_RAW_KEEP_AFTER_HANDOFF` | `0` (**recommended**) | `0` or a duration ≤ `RAW_MAX_AGE` | How long the stored message is kept once every recipient of every delivery of the mail is `handed_off` or `skipped`. With `0` it is removed in the same transaction that makes the last recipient final; a mail whose deliveries are all skipped is not stored at all. |
| `POSTWERK_RAW_KEEP_AFTER_FAILURE` | `72h` (**recommended**) | `0` or a duration ≤ `RAW_MAX_AGE` | How long the stored message is kept once every recipient is final and one of them is `failed` or `unknown` (a `partial` delivery counts), so an admin can resend. When it is removed, `unknown` recipients become `failed` with `retention.raw_expired`. |
| `POSTWERK_RAW_MAX_AGE` | `7d` (**recommended**) | a duration > 0 | Hard cap for any state, counted from receipt. When the message is removed while a recipient is still `queued` or `unknown`, that recipient becomes `failed` with a `retention.raw_expired` event (`error`, `warn` for optional targets). A recipient being sent at that moment is left to finish; the next run takes the message. |
| `POSTWERK_PSEUDONYMIZE_AFTER` | `30d` (**recommended**) | `0` (off) or a duration ≥ `RAW_MAX_AGE` and ≤ `RECORD_RETENTION` | Replaces recipient addresses and the envelope sender by keyed hashes (below). Only mails with no stored message and no delivery that may still be sent are touched. `0` keeps plain addresses until the records are deleted; a warning is logged. |
| `POSTWERK_RECORD_RETENTION` | `90d` (**recommended**) | a duration ≥ `RAW_MAX_AGE` | Deletes `mails` rows with their `recipients`, `deliveries` and recipient statuses. A mail with a recipient still `queued`, `sending` or `unknown` is kept. Raise it if you need delivery proof for longer, and record why. |
| `POSTWERK_EVENT_RETENTION` | `90d` (**recommended**) | a duration ≥ `RAW_MAX_AGE` | Deletes events older than this. Equal to the record retention by default, so events do not outlive their mail without a reason. |

Further rules, checked at start:

- Every retry window (`POSTWERK_RETRY_WINDOW` and each target's `RETRY_WINDOW`) must be shorter than
  `POSTWERK_RAW_MAX_AGE`, so retention never cuts a retry short.
- All durations are at most 36500 days.

**Keyed hashes.** A hash is `HMAC-SHA256(key, normalized address)`, stored as `<key-id>:<64 hex digits>`.
Normalization: remove leading and trailing ASCII white space, then lower-case the ASCII letters `A`–`Z` in the whole
address. The local part of an address is case-sensitive by RFC 5321, but Postwerk treats it case-insensitively for
lookups, as practically every mail system does. Other characters are kept as they are, because Unicode case mapping
changes between Unicode versions and a hash must stay reproducible. The null sender (`MAIL FROM:<>`) stays empty.
Pseudonymized data is still personal data for whoever holds the hash key
([privacy-and-security.md](privacy-and-security.md) section 4.3).

**Event log.** Events stay append-only: a database trigger refuses every update and every delete of an event that is
not older than a retention cutoff. The cutoff is a single row that can only move forward; the retention job raises it
to `now − POSTWERK_EVENT_RETENTION` before deleting.

**Copies outside the store.** Sinks (stdout, Sentry, later others), backups and capture targets keep their own copies with
their own retention. Backups should not live longer than `POSTWERK_RECORD_RETENTION`.

## Targets

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_TARGETS` | required | comma-separated names, `[a-z0-9_]{1,32}`, at most 16 | Every target receives every accepted mail, each with its own delivery. |
| `POSTWERK_RETRY_WINDOW` | `24h` | a duration > 0, < `RAW_MAX_AGE` | Default retry window: after it, a delivery that keeps failing temporarily becomes `failed`. |
| `POSTWERK_TARGET_<NAME>_URL` | required | `smtps://host[:465]`, `smtp://host[:587]`, `smtp+plain://host[:25]` | `smtps` is implicit TLS, `smtp` requires STARTTLS, `smtp+plain` is cleartext and must be chosen explicitly. No user, password, path or query in the URL. Certificates are always verified. |
| `POSTWERK_TARGET_<NAME>_CREDENTIALS_FILE` | none | path | First line `username:password` for `AUTH PLAIN`. Not allowed with `smtp+plain`. Secret file. |
| `POSTWERK_TARGET_<NAME>_STATUS` | none | `mailjet` | Provider status adapter. For `mailjet`, Postwerk removes any `X-MJ-CustomID` the sender set and adds its own (the mail id), so tracking is unambiguous, and turns open and click tracking off (`MAILJET_TRACKING`). The adapter itself is a later phase. |
| `POSTWERK_TARGET_<NAME>_OPTIONAL` | `false` | `true`, `false` | Failures of an optional target are warnings, not errors, and they do not change a mail's state: a mail is `failed`, `partial` or `handed_off` by its required targets alone (the delivery to the optional target keeps its own state). |
| `POSTWERK_TARGET_<NAME>_RETRY_WINDOW` | `POSTWERK_RETRY_WINDOW` | a duration > 0, < `RAW_MAX_AGE` | Retry window for this target. |
| `POSTWERK_TARGET_<NAME>_ON_UNKNOWN` | `hold` (`check` with a status adapter) | `hold`, `retry`, `check` | What to do when the connection breaks after the message was sent (unclear outcome). `hold` marks it `unknown` for an admin; `retry` sends again and may duplicate; `check` needs a status adapter and behaves like `hold` until the adapter exists. |
| `POSTWERK_TARGET_<NAME>_ALLOW_RECIPIENTS` | none (all allowed) | comma-separated patterns with `@`, `*` and `?` wildcards, case-insensitive | Allow-list. If any recipient of a mail is outside it, the whole delivery to this target is skipped (all or nothing per target; `delivery.not_allowed`, warning, one per recipient); the mail is not rejected and other targets still get it. |
| `POSTWERK_TARGET_<NAME>_MAILJET_TRACKING` | `off` (**recommended**) | `off`, `account`; only with `STATUS=mailjet` | `off` adds `X-Mailjet-TrackOpen: 0` and `X-Mailjet-TrackClick: 0` to every mail and removes those headers if the sender set them, so Mailjet neither rewrites links (password reset links included) nor adds a tracking pixel, whatever the account's settings. `account` adds nothing and passes the sender's headers on: the account's settings apply, and a sender can still switch tracking per mail. |
| `POSTWERK_TARGET_<NAME>_CA_FILE` | none (the system roots) | path to a PEM file with one or more `CERTIFICATE` blocks | The certificate authorities this target's certificate comes from, for a relay with a private CA. They **replace** the system roots for this target only; other targets keep the system roots. Checked at start: every PEM block must be a certificate that parses. Not allowed with `smtp+plain`. Not a secret file. |
| `POSTWERK_PROBE_INTERVAL` | `30s` | a duration > 0 | Wait before the first probe of a target that is down (section "Target down"). |
| `POSTWERK_PROBE_MAX_INTERVAL` | `5m` | a duration ≥ `POSTWERK_PROBE_INTERVAL` | Each failed probe doubles the wait, up to this. |

`<NAME>` is the target name in upper case. Security notes: prefer `smtps://` or `smtp://`; use `smtp+plain://` only for
a capture tool on the same host or network. **No capture target in production**: a capture tool holds working password
reset links. If one is needed for a short test, exclude sensitive mails, put authentication on its UI, and set its own
retention to a day or less (Mailpit: `--max-age 1d`, `--ui-auth-file`).

**Target down.** When a target rejects the login (`535`) or cannot be reached (connecting, TLS, the greeting or
`EHLO` fails), Postwerk stops sending to it and writes one `target.down` event (`error`, `warn` for an optional
target). Mails stay queued. It then probes the target (connect, `EHLO`, `AUTH`, `QUIT`; no mail) after
`POSTWERK_PROBE_INTERVAL`, doubling up to `POSTWERK_PROBE_MAX_INTERVAL`. The first probe that works writes
`target.recovered`, and every queued mail of that target is due at once, whatever its backoff; the same happens at
every start, so after fixing credentials and restarting the queue goes out immediately. A provider that locks an account after repeated failed logins sees at most one
login per probe. Mails whose retry window ends meanwhile become `failed`. The state is kept in memory only: after a
restart the first attempt finds a broken target again. Fix the credentials file and restart, or wait for the probe if
the provider was down.

**Private CA.** `CA_FILE` replaces the roots, rather than adding to them, so a target behind a private CA trusts that CA
and nothing else; a public provider needs no `CA_FILE`. Go's `SSL_CERT_FILE` still works but changes the roots for
every target and for the Sentry sink, so prefer `CA_FILE`.

## Limits

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_MAX_MESSAGE_BYTES` | `15728640` (15 MiB, **recommended** for Mailjet) | 1024 to 1073741824 | The largest message a target receives from Postwerk, Postwerk's own header lines included. Set it to the smallest limit of your targets (section "Message size"). Ingress accepts this minus the header lines Postwerk adds and offers that as `SIZE` in `EHLO`; larger messages get `552`. |
| `POSTWERK_MAX_RECIPIENTS` | `50` | 1 to 1000 | More recipients per message get `452`. |
| `POSTWERK_MAX_INFLIGHT_BYTES` | `268435456` (256 MiB, **recommended**) | `POSTWERK_MAX_MESSAGE_BYTES` to 1099511627776 | Memory budget for messages being received at once. Each `DATA` reserves `POSTWERK_MAX_MESSAGE_BYTES`, or the `SIZE=` the sender declared if smaller, before reading; when the budget is used up the reply is `451 4.3.1 Insufficient system resources, try again later` and the message is discarded unread into memory. A message larger than its declared `SIZE` grows its reservation to the maximum or gets the same `451`. Senders retry a `451` like any temporary error. Processing and encryption copy a message a few times, so plan the container's memory limit at about three times this value plus some 50 MiB. |
| `POSTWERK_MAX_CONNECTIONS` | `100` | 1 to 10000 | Concurrent SMTP connections. A connection over the limit gets `421 4.7.0 Too many connections, try again later` and is closed. |
| `POSTWERK_MIN_FREE_BYTES` | `67108864` (64 MiB, **recommended**) | `0` (off) to 2^50 | Free space kept on the data volume. Below it, `MAIL` and `RCPT` get `452 4.3.1 Insufficient system storage, try again later`, and so does a message that would take the volume below it; nothing is stored. `/readyz` fails and one `disk.low` event (error) is written; `disk.ok` once a quarter more than the reserve is free again. Keep at least twice `POSTWERK_MAX_MESSAGE_BYTES` (a warning says so otherwise): the mails already accepted still need room to be delivered ([architecture.md](architecture.md#signals)). Read with `statfs` on Linux, macOS and FreeBSD; on other systems the check is off and the start log says so. `0` turns it off, with a warning. |

Fixed, not configurable: line length 2000 bytes, address length 320 bytes, connection closed after 3 failed `AUTH`,
connection closed when it has not logged in within 30 s (`421`), at most as many password checks at once as the
process has CPUs (`GOMAXPROCS`; a login waits up to 5 s for a slot, then gets `454 4.7.0`), SMTP read timeout 2
minutes, write timeout 1 minute. Envelope addresses with more than one `@`, or with characters that
need a quoted local part (space, `"`, `\`, `<`, `>`, `(`, `)`, `,`, `;`), get `553`: they could not be passed on to a
target unchanged. Internationalized addresses (UTF-8, RFC 6531) are accepted only when the sender gives `SMTPUTF8` in
`MAIL`; otherwise they get `553 5.6.7 Non-ASCII address requires SMTPUTF8`.

### Message size

A provider refuses a message larger than its limit. Postwerk adds header lines to every message it sends, so it
accepts a little less than the limit, computed exactly: `X-Postwerk-Id: pw_…` (46 bytes) for every target, and
`X-MJ-CustomID: pw_…` (46 bytes) as well for a target with `STATUS=mailjet`, and with `MAILJET_TRACKING=off` (the
default) `X-Mailjet-TrackOpen: 0` (24 bytes) and `X-Mailjet-TrackClick: 0` (25 bytes). The mail id always has 29
characters. Postwerk takes the largest of these sums over all targets: with the default and a Mailjet target, ingress
offers `SIZE 15728499` (15728640 − 141). The start log shows both values (`max_message_bytes`, `smtp_size`).

Sizes are counted as the target receives the message: a line that ends in a bare LF is sent with CRLF, so each such
LF counts as two bytes at ingress. A sender's own `X-Postwerk-*` and, for Mailjet, `X-MJ-CustomID` lines (and with
tracking off its `X-Mailjet-TrackOpen` and `X-Mailjet-TrackClick` lines) are removed, which only makes the message
smaller.

For another provider, set `POSTWERK_MAX_MESSAGE_BYTES` to its limit; with several targets, to the smallest one. Some
values (taken from the provider's `EHLO` reply or documentation, November 2026; check yours, they change):

| Provider | Limit | `POSTWERK_MAX_MESSAGE_BYTES` | Source |
|---|---|---|---|
| Mailjet (`in-v3.mailjet.com`) | `SIZE 15728640` in `EHLO` | `15728640` (the default) | `EHLO` reply |
| Amazon SES (SMTP) | 40 MB per message including attachments, after base64 encoding; no `SIZE` in `EHLO` | `40000000` | [SES service quotas](https://docs.aws.amazon.com/ses/latest/dg/quotas.html) |
| Postmark | 10 MB total, including attachments, after base64 encoding (`EHLO` offers `SIZE 20480000`) | `10000000` | [Postmark size limits](https://postmarkapp.com/support/article/1056-what-are-the-attachment-and-email-size-limits) |

Where a document says "MB", use the decimal value (10 MB = 10000000 bytes): it is the smaller reading. To see what a
target offers:

```sh
(printf 'EHLO relay.example.test\n'; sleep 3; printf 'QUIT\n') |
  openssl s_client -quiet -crlf -starttls smtp -connect smtp.example.test:587 2>/dev/null | grep SIZE
```

A target that offers a smaller `SIZE` than the message is never sent it: that delivery fails with
`delivery.too_large`. A larger limit also uses more of `POSTWERK_MAX_INFLIGHT_BYTES`: a `DATA`
without a declared `SIZE=` reserves the whole accepted size, so with 40000000 the default budget of 256 MiB takes six
such messages at once. Raise the budget and the container's memory limit with it if needed.

**SMTP extensions.** Ingress offers `8BITMIME`, `SMTPUTF8`, `SIZE`, `PIPELINING`, `CHUNKING` and
`ENHANCEDSTATUSCODES`, never `DSN` or `REQUIRETLS` (Postwerk could not keep their promise towards every target). The
declared `BODY=`, `SMTPUTF8` and `SIZE=` are stored; `SMTPUTF8` and `BODY=8BITMIME` are passed on to each target that
offers them, and `SIZE=` is sent with the exact size of the message as it leaves Postwerk. A mail that needs `SMTPUTF8`
or `8BITMIME` is never sent to a target without it: that delivery fails with `delivery.unsupported`. A message larger
than the `SIZE` a target offers is not sent there: `delivery.too_large` (details in
[architecture.md](architecture.md#delivery-worker)). Not configurable.

## Shutdown

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_SHUTDOWN_TIMEOUT` | `100s` | a duration > 0 | Deadline for a graceful stop after `SIGTERM` or `SIGINT`. |

On shutdown, the delivery workers stop taking new work and finish the send in progress; at the same time the SMTP
listener closes and open sessions get at most half of the deadline. Then the remaining events are exported and the
store is closed.

One delivery attempt is capped at **90 s** (dial 15 s, each command 30 s, the reply after the message 60 s, all within
the cap). The default deadline of 100 s is longer, so a send in progress always finishes; a shorter deadline logs a
warning at start, and a send cut by it becomes an unclear outcome on the next start. **The orchestrator's grace period
must be longer than the deadline**: Kubernetes `terminationGracePeriodSeconds: 120`, Docker Compose
`stop_grace_period: 120s`, `docker stop --time 120`.

## Error reports to Sentry or GlitchTip

The Sentry-protocol sink sends warn and error events to Sentry or a Sentry-compatible tool such as GlitchTip, so you
get an alert with the mail id, target, sender login and SMTP code, and can look the mail up in the UI. It is off until
a DSN file is set. Details of what is sent: [privacy-and-security.md](privacy-and-security.md#24-what-goes-to-sentry).

With the default level `error`, a report means the operator has something to do: a target is down (`target.down`,
one per outage, not per mail: a wrong password or a provider outage pages once), a mail failed for a reason on the
relay's or the provider's side (`delivery.failed`, including the end of the retry window), an unclear outcome, a
message too large or needing an extension the target lacks, a decryption or retention failure, a TLS reload problem.
A recipient address the target refuses (a typo, a closed mailbox) is `delivery.rejected` at `warn`: the sending
application and its user can fix that, the operator cannot. Failures at optional targets are `warn` too.

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_SENTRY_DSN_FILE` | none (sink off) | path | File whose first line is the project's DSN, `https://<key>@<host>[:port][/path]/<project id>`. Checked at start. Secret file. |
| `POSTWERK_SENTRY_MIN_LEVEL` | `error` (**recommended**) | `error`, `warn` | Lowest level that is sent. `warn` adds retries, rejected recipient addresses (`delivery.rejected`), failures at optional targets, skipped recipients, failed UI and API logins, and refused SMTP logins and commands (`ingress.*`). Lower events only move the sink's cursor. |
| `POSTWERK_SENTRY_ALLOW_HTTP` | `false` | `true`, `false` | Accept an `http://` DSN. For a local test, or a GlitchTip in the same cluster on a network that encrypts traffic between nodes. A warning is logged at every start. |

`POSTWERK_SENTRY_DSN` (the DSN as a value) is refused: the DSN holds the project key, and like every secret it comes
from a file. `POSTWERK_SENTRY_MIN_LEVEL` or `POSTWERK_SENTRY_ALLOW_HTTP` without a DSN file is a configuration error.
The environment of each event is `POSTWERK_INSTANCE` (none if it is empty).

Fixed, not configurable: one request per event, at most 16 KiB, 10 s timeout, no redirects followed. A `429` pauses
the sink for its `Retry-After` (60 s without one), and `X-Sentry-Rate-Limits` on any response pauses it as long as it
says (at most an hour). A network error, a `5xx` or a refused DSN (`401`, `403`, `404`) pauses it for 5 s, doubling up
to 5 minutes, and logs one line until it works again. An event the server refuses for good (`400`, `413`, `422`) is
skipped with a warning. Events wait in the event log meanwhile, so nothing is lost while Sentry is down (until
`POSTWERK_EVENT_RETENTION`). A slow or failing Sentry never holds up delivery or stdout. At shutdown the final export
waits at most 10 s; what is left goes out after the next start.

## Signals

Postwerk's own checks ([design.md](design.md#91-alerting)). They write one event when a state changes; who is
notified is set in the Sentry-compatible tool. The free-space check is under [Limits](#limits).

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_QUEUE_STALE_AFTER` | `15m` (**recommended**) | `0` (off) or a duration | When the oldest mail queued for a target is older than this, `queue.stale` is written (error, warn for an optional target), with the number of queued mails and the oldest age; `queue.recovered` (info) when it no longer is. Checked every minute. 15 minutes is longer than the first five retries (about 18 minutes of backoff start within it, so a short blip passes) and about as long as a password reset or login code stays useful. `0` turns it off, with a warning. |
| `POSTWERK_QUEUE_STALE_REPEAT` | `0` (once per change) | `0` or at least `1m` | Write `queue.stale` again at this interval while the target stays stale. For alert rules that fire on new events only; GlitchTip and Sentry group the repeats into the same issue. |

## Other

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_INSTANCE` | empty | up to 64 of `A-Z a-z 0-9 . _ -` | Name added to every log line and exported event, to tell instances apart. |

## Reserved for later phases

These names are known, so a configuration written ahead does not fail as a typo, but they are **not implemented**: a
non-empty value stops the start.

| Variable | Planned use |
|---|---|
| `POSTWERK_OTEL_ENDPOINT` | OpenTelemetry sink for events (A4). |
| `POSTWERK_WEBHOOK_URL` | Signed webhook sink (A4). |

## Commands

| Command | What it does |
|---|---|
| `postwerk` or `postwerk serve` | Run the relay. |
| `postwerk hash-password` | Read one password from stdin, print the value for the senders file. |
| `postwerk gen-key` | Print a new key line for a store or hash key file. |
| `postwerk gen-token <name>` | Print a new UI token and its line for `POSTWERK_UI_TOKENS_FILE` (always role `admin`; `gen-token <name> admin` works too). |
| `postwerk gen-api-token <name> <read\|admin>` | Print a new API bearer value (`<name>.<token>`) and its line for `POSTWERK_API_TOKENS_FILE`. |
| `postwerk version` | Print the version. |

## Production checklist

- [ ] **TLS on ingress** (`POSTWERK_SMTP_TLS_CERT_FILE`, `POSTWERK_SMTP_TLS_KEY_FILE`), or a private network that
      encrypts traffic between nodes. `POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true` only with a reason.
- [ ] **Targets over TLS** (`smtps://` or `smtp://`); no `smtp+plain://` towards a provider.
- [ ] **Key files as mounted secrets**: a store key and a hash key, made with `postwerk gen-key`, different from each
      other, never the example keys, readable by the Postwerk user only. No key in an environment variable, image or
      repository. A backup of the database is useless without the key files, so keep them apart.
- [ ] **Encryption on and secure delete on** (the defaults); no `off` warnings in the start log.
- [ ] **Data volume owned by uid 65532** (the image's user), on a local disk, not shared with anything else; directory
      `0700`.
- [ ] **Grace period** longer than `POSTWERK_SHUTDOWN_TIMEOUT` (Kubernetes `terminationGracePeriodSeconds: 120`).
- [ ] **No capture target in production.**
- [ ] **HTTP listener** reachable only by the probes (`:8080` inside the container, never published).
- [ ] **Admin listener** on loopback (`127.0.0.1:8081`, the default) and opened only through `kubectl port-forward` or
      an SSH tunnel; no Service port, Ingress or public address. One account with a password per person, created in
      the UI; the tokens file removed or kept safely as break-glass access. Or `POSTWERK_ADMIN_LISTEN=off`.
- [ ] **Query API** off (the default), or on with TLS, one token per calling application, `read` unless full
      addresses or the lists are needed, and in Kubernetes `api.allowFrom` naming only those applications.
- [ ] **In Kubernetes:** the chart's NetworkPolicy on, SMTP only from the sending pods, no Ingress, `LoadBalancer` or
      `NodePort`, a CNI that enforces NetworkPolicy ([kubernetes.md](kubernetes.md)).
- [ ] **Retention** at the recommended values, or changes written down with their reason; backups no longer than
      `POSTWERK_RECORD_RETENTION`.
- [ ] **Sentry or GlitchTip** (if used): DSN in a secret file, `https://`, `POSTWERK_INSTANCE` set so alerts name the
      instance; the error tracker's own retention and access fit the data it receives. Alert rules set for the kinds
      that need a person ([design.md](design.md#91-alerting)), and the pod's readiness watched by the platform.
- [ ] **Data volume** larger than the reserve (`POSTWERK_MIN_FREE_BYTES`, 64 MiB) plus the queue you expect during
      the longest provider outage you plan for.
- [ ] Start log checked: no warnings about file permissions, encryption or pseudonymization.
