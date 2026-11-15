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
| `POSTWERK_HOSTNAME` | `postwerk` | a host name | Name in the SMTP greeting and in `EHLO` towards targets. |
| `POSTWERK_SMTP_TLS_CERT_FILE` | none | path to a PEM file | Certificate for STARTTLS on ingress. Set together with the key file. |
| `POSTWERK_SMTP_TLS_KEY_FILE` | none | path to a PEM file | Private key for STARTTLS. Secret file. |
| `POSTWERK_SMTP_ALLOW_INSECURE_AUTH` | `false` | `true`, `false` | Accept `AUTH` over cleartext SMTP. |

Security notes:

- Either set both TLS files (then `AUTH` is only offered after `STARTTLS`), or set
  `POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true` explicitly. Setting neither is a configuration error, and so is setting
  both (ambiguous). Cleartext `AUTH` sends sender passwords readable to anyone on the network path: use it only on a local
  machine or a network that encrypts traffic between nodes.
- **Health probes in containers.** The default HTTP address is the loopback interface, which is right for a process on
  a host. In a container, probes come from outside the loopback (the kubelet, Docker's health check from the
  host network), so containers set `POSTWERK_HTTP_LISTEN=:8080`, as [examples/compose.yaml](../examples/compose.yaml)
  does. Never publish this port to the internet; the later query API will share it.

## Senders

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_SENDERS_FILE` | required | path | Applications allowed to send. One line per sender: `name:pbkdf2-sha256$<iterations>$<salt>$<key>`. Empty lines and `#` lines are ignored. Names: `[a-z0-9][a-z0-9._-]{0,63}`. |

Create a line with `printf '%s' 'password' | postwerk hash-password` (reads the password from stdin, prints the hash).
The file holds hashes only, but it is still a secret file: a copy allows offline guessing.

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
| `POSTWERK_RAW_KEEP_AFTER_HANDOFF` | `0` (**recommended**) | `0` or a duration ≤ `RAW_MAX_AGE` | How long the stored message is kept once every delivery of the mail is `handed_off` or `skipped`. With `0` it is removed in the same transaction that makes the last delivery final; a mail whose deliveries are all skipped is not stored at all. |
| `POSTWERK_RAW_KEEP_AFTER_FAILURE` | `72h` (**recommended**) | `0` or a duration ≤ `RAW_MAX_AGE` | How long the stored message is kept once every delivery is final and one of them is `failed` or `unknown`, so an admin can resend. When it is removed, `unknown` deliveries become `failed` with `retention.raw_expired`. |
| `POSTWERK_RAW_MAX_AGE` | `7d` (**recommended**) | a duration > 0 | Hard cap for any state, counted from receipt. When the message is removed while a delivery is still `queued` or `unknown`, that delivery becomes `failed` with a `retention.raw_expired` event (`error`, `warn` for optional targets). A delivery being sent at that moment is left to finish; the next run takes the message. |
| `POSTWERK_PSEUDONYMIZE_AFTER` | `30d` (**recommended**) | `0` (off) or a duration ≥ `RAW_MAX_AGE` and ≤ `RECORD_RETENTION` | Replaces recipient addresses and the envelope sender by keyed hashes (below). Only mails with no stored message and no delivery that may still be sent are touched. `0` keeps plain addresses until the records are deleted; a warning is logged. |
| `POSTWERK_RECORD_RETENTION` | `90d` (**recommended**) | a duration ≥ `RAW_MAX_AGE` | Deletes `mails` rows with their `recipients` and `deliveries`. A mail with a delivery still `queued`, `sending` or `unknown` is kept. Raise it if you need delivery proof for longer, and record why. |
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

**Copies outside the store.** Sinks (stdout, later others), backups and capture targets keep their own copies with
their own retention. Backups should not live longer than `POSTWERK_RECORD_RETENTION`.

## Targets

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_TARGETS` | required | comma-separated names, `[a-z0-9_]{1,32}`, at most 16 | Every target receives every accepted mail, each with its own delivery. |
| `POSTWERK_RETRY_WINDOW` | `24h` | a duration > 0, < `RAW_MAX_AGE` | Default retry window: after it, a delivery that keeps failing temporarily becomes `failed`. |
| `POSTWERK_TARGET_<NAME>_URL` | required | `smtps://host[:465]`, `smtp://host[:587]`, `smtp+plain://host[:25]` | `smtps` is implicit TLS, `smtp` requires STARTTLS, `smtp+plain` is cleartext and must be chosen explicitly. No user, password, path or query in the URL. Certificates are always verified. |
| `POSTWERK_TARGET_<NAME>_CREDENTIALS_FILE` | none | path | First line `username:password` for `AUTH PLAIN`. Not allowed with `smtp+plain`. Secret file. |
| `POSTWERK_TARGET_<NAME>_STATUS` | none | `mailjet` | Provider status adapter. For `mailjet`, Postwerk removes any `X-MJ-CustomID` the sender set and adds its own (the mail id), so tracking is unambiguous. The adapter itself is a later phase. |
| `POSTWERK_TARGET_<NAME>_OPTIONAL` | `false` | `true`, `false` | Failures of an optional target are warnings, not errors. |
| `POSTWERK_TARGET_<NAME>_RETRY_WINDOW` | `POSTWERK_RETRY_WINDOW` | a duration > 0, < `RAW_MAX_AGE` | Retry window for this target. |
| `POSTWERK_TARGET_<NAME>_ON_UNKNOWN` | `hold` (`check` with a status adapter) | `hold`, `retry`, `check` | What to do when the connection breaks after the message was sent (unclear outcome). `hold` marks it `unknown` for an admin; `retry` sends again and may duplicate; `check` needs a status adapter and behaves like `hold` until the adapter exists. |
| `POSTWERK_TARGET_<NAME>_ALLOW_RECIPIENTS` | none (all allowed) | comma-separated patterns with `@`, `*` and `?` wildcards, case-insensitive | Allow-list. If any recipient of a mail is outside it, the whole delivery to this target is skipped (`delivery.not_allowed`, warning); the mail is not rejected and other targets still get it. |

`<NAME>` is the target name in upper case. Security notes: prefer `smtps://` or `smtp://`; use `smtp+plain://` only for
a capture tool on the same host or network. **No capture target in production**: a capture tool holds working password
reset links. If one is needed for a short test, exclude sensitive mails, put authentication on its UI, and set its own
retention to a day or less (Mailpit: `--max-age 1d`, `--ui-auth-file`).

## Limits

| Variable | Default | Allowed | What it does |
|---|---|---|---|
| `POSTWERK_MAX_MESSAGE_BYTES` | `26214400` (25 MiB) | 1024 to 1073741824 | Larger messages get `552`. |
| `POSTWERK_MAX_RECIPIENTS` | `50` | 1 to 1000 | More recipients per message get `452`. |
| `POSTWERK_MAX_CONNECTIONS` | `100` | 1 to 10000 | Concurrent SMTP connections. A connection over the limit gets `421 4.7.0 Too many connections, try again later` and is closed. |

Fixed, not configurable: line length 2000 bytes, address length 320 bytes, connection closed after 3 failed `AUTH`,
SMTP read timeout 2 minutes, write timeout 1 minute. Envelope addresses with more than one `@`, or with characters that
need a quoted local part (space, `"`, `\`, `<`, `>`, `(`, `)`, `,`, `;`), get `553`: they could not be passed on to a
target unchanged.

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
| `POSTWERK_SENTRY_DSN` | Sentry-protocol sink for error events (A4). |
| `POSTWERK_WEBHOOK_URL` | Signed webhook sink (A4). |

## Commands

| Command | What it does |
|---|---|
| `postwerk` or `postwerk serve` | Run the relay. |
| `postwerk hash-password` | Read one password from stdin, print the value for the senders file. |
| `postwerk gen-key` | Print a new key line for a store or hash key file. |
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
- [ ] **Retention** at the recommended values, or changes written down with their reason; backups no longer than
      `POSTWERK_RECORD_RETENTION`.
- [ ] Start log checked: no warnings about file permissions, encryption or pseudonymization.
