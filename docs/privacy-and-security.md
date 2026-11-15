# Privacy and security

What Postwerk stores, for how long, who can read it, and how an operator meets data protection duties with it. It
describes what is built, marks what is planned, and ends with the list of privacy changes and their status.

Status: the foundation is built, including address redaction, retention and encryption at rest. The query API, the UI,
the external sinks and the provider status adapter are planned; what this page says about them is intent. The exact
schema is in [architecture.md](architecture.md#store), every setting in [configuration.md](configuration.md).

This page is not legal advice. It points to the relevant rules so that operators know what to check with their own data
protection officer or lawyer.

## 1. Short answers

| Question | Answer |
|---|---|
| Does Postwerk store the mail content? | Yes, the raw message, encrypted at rest, only until it is no longer needed for delivery: removed at hand-off, kept 72 h after a failure, never older than 7 days (defaults). |
| Does it store the subject? | Not as its own field. It is inside the encrypted raw message only, and gone with it. |
| Does it store recipient addresses? | Yes, in the `recipients` table, replaced by a keyed hash after 30 days (default). SMTP reply texts are stored with addresses replaced by `<redacted>`. |
| When is everything gone? | Mail records and events are removed after 90 days (default). Every period can be changed (section 3). |
| Can we audit after deletion of the content? | Yes for "was a mail to this address sent, when, and what happened", no for content (section 4). |
| Who can read mails? | Anyone with a shell in the container, the cluster admin, the provider, and anyone with access to a capture tool. Not the event log or logs. A copy of the volume or a backup without the key file does not reveal content (section 6). |

## 2. Data inventory

### 2.1 The store

Tables as built ([architecture.md](architecture.md#store)). "Personal" means personal data in the GDPR sense: it relates
to an identifiable person, directly or through other data the operator holds. Periods are the defaults.

**`mails`**

| Column | What it holds | Personal | Content | Kept until |
|---|---|---|---|---|
| `id` | Postwerk id, e.g. `pw_01JCX4…` | Indirectly: it links to the provider's record and to the recipient's copy (`X-Postwerk-Id`, `X-MJ-CustomID`) | no | record retention (90 d) |
| `received_at` | time | no, alone | no | record retention |
| `sender` | the application's login name, e.g. `auth-service` | no, unless a login is named after a person | no | record retention |
| `envelope_from` | `MAIL FROM` address | Usually a system address (`noreply@…`). Personal if an application sends as a person, or if it uses per-recipient bounce addresses (VERP, e.g. `bounce+user=example.com@…`) | no | keyed hash after 30 d; record retention |
| `message_id` | the `Message-ID` header | Indirectly: the recipient's copy carries the same id. Some libraries put hostnames or user ids into it | no | record retention |
| `ref` | `X-Postwerk-Ref`, chosen by the sender | Depends on the sender. A value with `@` is not stored; a user id here is still personal | no | record retention |
| `size` | bytes | no | no | record retention |
| `raw` | the full message: headers (To, Cc, From, Subject), body, attachments, reset links, one-time codes; encrypted | **yes** | **yes** | hand-off; 72 h after a failure; 7 d at most |

**`recipients`**

| Column | Personal | Note |
|---|---|---|
| `mail_id`, `position` | no, alone | |
| `address` | **yes** | Every `RCPT TO` address, including Bcc recipients. Plain until the pseudonymization age (30 d), then a keyed hash with a key id. Removed with the mail record. |

**`deliveries`** and **`delivery_recipients`** (one status per mail, target and recipient)

| Column | Personal | Note |
|---|---|---|
| `mail_id`, `target`, `position`, `status`, `attempts`, `next_attempt_at`, `data_sent_at`, `last_code`, `updated_at` | no, alone | The position links to `recipients`; it is not an address. |
| `last_text` | no, after redaction | The target's reply text. Replies such as `550 5.1.1 <user@example.com>: Recipient address rejected` are stored as `550 5.1.1 <redacted>: Recipient address rejected`. |
| provider id | indirectly | *(planned adapter, per recipient)* The provider's message id. It links to the provider's record, which holds the address and often the content. |
| provider status | no, if mapped | *(planned adapter, per recipient)* Should be stored as a fixed status word only (change 16). |

**`events`**

| Column | Personal | Note |
|---|---|---|
| `seq`, `time`, `mail_id`, `target`, `recipient`, `kind`, `level`, `attempt`, `code` | no, alone | `recipient` is the position in the envelope (0, 1, …), never the address. Together with the `recipients` table it points to a person until pseudonymization or deletion of the record. |
| `text` | no, after redaction | Same redaction as `last_text`. |
| `sender` | no, unless a login is named after a person | |

Every free text that is stored or exported goes through one redaction function: anything that looks like an address
becomes `<redacted>`, control characters are removed, and the length is capped at 300 bytes. The reply code and the
enhanced status code (`5.1.1`) stay, because they carry the useful information. Events are append-only and exported to
every sink, so an address that got in once would be copied everywhere; redaction at write time prevents that.
Internationalized addresses (UTF-8, such as `jörg@exämple.test`) are redacted the same way: an address token ends only
at white space (ASCII or Unicode), brackets, quotes or separators, so non-ASCII letters are part of it. A leak test in
the test suite checks it with an ASCII and a UTF-8 canary address (section 8.2, change 14).

### 2.2 Outside the store

| Place | What ends up there | Personal | Content | Who can see it |
|---|---|---|---|---|
| stdout (container logs) | one JSON line per event, plus operational log lines | no addresses (redacted); ids and sender names | no | everyone with access to the log collector |
| OpenTelemetry *(planned)* | events as logs, metrics | as stdout; metric labels must never contain addresses or recipient domains | no | monitoring readers |
| Sentry-protocol tool *(planned)* | error events | as stdout, plus anything an SDK adds by default: breadcrumbs, request data (change 12 avoids this) | no | error-tracker users |
| Webhook *(planned)* | signed events | as stdout; with [scope.md gap 11](scope.md#7-gaps-between-the-ideal-and-the-current-design) also `ref` | no | whoever runs the receiving endpoint |
| Query API *(planned)* | metadata and addresses; never content | yes | no | token holders |
| API access logs, reverse proxy, browser history *(planned API)* | a search such as `GET /v1/mails?to=user@example.com` would put the address into a URL (change 8 avoids this) | **yes** | no | anyone reading proxy or access logs |
| UI *(planned)* | the same as the API | yes | no | token holders (change 10) |
| SQLite free pages and WAL file | deleted rows until overwritten; `secure_delete` overwrites them, a checkpoint clears the WAL | yes, briefly | encrypted only | anyone with the volume |
| Backups | a full copy of the store at that time | yes | encrypted; readable only with the key file | anyone with the backup and the key |
| Provider | the full message, addresses, its own event history | yes | yes | provider staff and systems, under the provider's retention |
| Capture tool (e.g. Mailpit) | the full message, including reset links and one-time codes | yes | yes | anyone who can open its UI or API |
| Sending application | its own copy and logs | yes | yes | outside Postwerk's scope |

### 2.3 Not stored

- The subject as a separate field. Recommendation: do not add one. If support needs to know the kind of mail, the sender
  can encode it in an opaque `ref` (for example `pwreset-8f3a2c`).
- Message bodies in events, logs or sink payloads.
- Sender passwords in plaintext. The senders file holds PBKDF2-SHA256 hashes (section 6.3).
- Key material in the store. Keys live in key files (section 6.2).

## 3. Lifecycle and retention

### 3.1 When data is created and removed

| Data | Created | Removed (default) |
|---|---|---|
| `raw` | at `DATA`, before `250` | when every delivery is `handed_off` or `skipped`; 72 h after a delivery failed; in any case after 7 days |
| `recipients.address` | at `RCPT TO` | replaced by a keyed hash after 30 days; removed with the mail record |
| `envelope_from` | at `MAIL FROM` | replaced by a keyed hash after 30 days, like the recipients; removed with the mail record |
| `last_text`, event `text` | each attempt | with the row or event; addresses are redacted before writing |
| `mails`, `recipients`, `deliveries` rows | at acceptance | after 90 days |
| `events` | each state change | after 90 days |
| sink copies | on export | by the sink's own retention; the operator sets it |
| backups | by the operator | by the operator; no longer than the record retention |

Why `raw` goes at hand-off: the only use of the stored message is resending. Resend is offered for `failed` and
`unknown` only, not after `handed_off`. Waiting for the provider's final status would keep content for a day without a
use.

### 3.2 Why these periods

The periods are reasoned defaults for a typical support process. They are not legal requirements. The GDPR does not
set fixed numbers. It sets principles: data limited to what is necessary (Art. 5(1)(c)) and kept in identifiable form
no longer than necessary (Art. 5(1)(e)). The operator decides what "necessary" means for its purposes and documents it.
Operators with their own rules (a retention policy, a legal obligation, a works agreement) override the defaults.
Variable names and formats are in [configuration.md](configuration.md).

| Setting | Default | Reason | Trade-off |
|---|---|---|---|
| raw after hand-off | removed at once | Every target has the mail; Postwerk has no further use for the content. | No resend after hand-off, which is not offered anyway. |
| raw after a failure | 72 h | Enough time over a weekend for someone to see the error, fix the cause and resend ([scope.md gap 2](scope.md#7-gaps-between-the-ideal-and-the-current-design)). | Longer means more time to react, and more content at rest. |
| raw maximum age | 7 d | A hard cap for every state, including `unknown` under `hold` and long `535` outages, so content never stays by accident. | After it, resend is no longer possible; the delivery record stays. |
| addresses pseudonymized after | 30 d | Support can see plain addresses for a month ("I did not get my invoice last month"). After that, lookups still work by hash (section 4). | Shorter is better for privacy and makes support slightly harder. |
| mail records | 90 d | Covers typical support and incident questions, and one quarter of trend data. | Operators who need delivery proof for longer, for example for legal claims, raise it and write the reason into their records of processing. |
| events | 90 d | Equal to the record retention, so events never outlive the mail they describe without a reason. | Export events to a sink with its own retention if longer audit history is needed. |

Capture targets keep their own copy, outside Postwerk's retention. For Mailpit, set `--max-age` (for example `1d`) and
`--ui-auth-file`; without `--max-age` it keeps up to 500 messages by default (`--max`)
([Mailpit runtime options](https://mailpit.axllent.org/docs/configuration/runtime-options/)).

## 4. Audit after deletion

### 4.1 How lookups work on hashed addresses

After the pseudonymization age, each address is replaced by `HMAC-SHA256(key, normalized address)` plus a key id. A
lookup for `user@example.com` hashes the query with the same key and compares hashes. The normalization is fixed in
the code; otherwise lookups would miss. The lookup itself is part of the planned query API.

A keyed hash (HMAC) is used instead of a plain hash because addresses are easy to guess. Without a key, anyone with the
store could hash a list of known addresses and compare. With a key, only someone who holds the key, or can call the
lookup, can test guesses. The EDPB names keyed one-way functions such as HMACs as suitable, and warns that unauthorised
use of the secret allows lookup tables to be built
([EDPB Guidelines 01/2025](https://www.edpb.europa.eu/our-work-tools/documents/public-consultations/2025/guidelines-012025-pseudonymisation_en),
paras. 107–109, version for public consultation). So: the hash key lives in its own key file, not in the store or the
image, and address lookups are to be logged and rate-limited once the API exists.

Key rotation: new hashes use the newest key; old keys stay in the key file for lookups until the operator removes them.
Removing an old key is a deliberate step that makes the old hashes unsearchable for Postwerk.

### 4.2 What can still be answered

| Question | Raw present | Raw deleted | Address hashed | Old hash key removed | Rows past retention |
|---|---|---|---|---|---|
| Was a mail to `user@example.com` sent on day X? | yes | yes | yes, by hash | no | no |
| What happened to it, per target, with timestamps? | yes | yes | yes | only by `id`, `ref`, `Message-ID` or provider id | no, unless events were exported to a sink |
| Which application sent it? | yes | yes | yes | yes, by id | no |
| What did the provider say (delivered, bounced)? *(planned adapter)* | yes | yes | yes | yes, by id | no |
| Which addresses received mail on day X (a list)? | yes | yes | no, only hashes | no | no |
| What did the mail say, what was the subject? | yes | no | no | no | no |
| How many mails, failures, bounces per target and day? | yes | yes | yes | yes | only from exported events |

Content is never answerable after hand-off. If an operator must prove what a mail said, the sending application has to
keep that record. Postwerk is not an archive.

A recipient can also bring the `Message-ID` or `X-Postwerk-Id` from their own copy. Both lookups keep working after
pseudonymization.

### 4.3 Pseudonymized is still personal

Hashed addresses are pseudonymized, not anonymous. The operator holds the key and can link a hash back to a person who
gives their address. Under GDPR Recital 26, pseudonymized data that can be attributed to a person with additional
information is information on an identifiable person
([GDPR, EUR-Lex](https://eur-lex.europa.eu/eli/reg/2016/679/oj)). The EDPB adds that even after the key is erased, the
data is anonymous only if the conditions for anonymity are met
([EDPB Guidelines 01/2025](https://www.edpb.europa.eu/our-work-tools/documents/public-consultations/2025/guidelines-012025-pseudonymisation_en),
para. 22). For Postwerk that matters: a row with no address still has a `Message-ID` and a timestamp, and the provider
and the recipient hold the matching mail.

In 2025 the Court of Justice held that identifiability is judged from the view of the party holding the data
([C-413/23 P, EDPS v SRB](https://curia.europa.eu/juris/liste.jsf?num=C-413/23)). For the operator, who holds the key,
the hashed data stays personal. Do not rely on that ruling to treat Postwerk's store as anonymous.

Only deletion of the rows, or a real anonymization (counts without ids), ends the GDPR duties for this data.

## 5. Access and erasure requests (Art. 15, 17)

### 5.1 Where one person's data sits

| Place | Found by | Erasure |
|---|---|---|
| `recipients.address`, `mails.envelope_from` | plain match, or hash with every key in the key file | replace with a random tombstone that no key can reproduce |
| `mails.raw` | through the mail row | delete (usually already gone) |
| `mails.ref`, `message_id` | through the mail row | clear if they may contain personal data |
| `deliveries.last_text`, event `text` | — | nothing to do: addresses are redacted before writing |
| `events` | by `mail_id` | no change needed: events contain no addresses |
| sinks, backups | not by Postwerk | operator; backups expire by their own retention |
| provider, capture tool | not by Postwerk | operator, using the provider's deletion feature |

Because no address enters the event log, an erased mail's events can stay as facts about mail id `pw_…`. That is still
pseudonymous data (section 4.3), but no longer searchable by address in Postwerk.

Until the admin calls below exist, the retention periods are the only built way to remove one person's data from
Postwerk early; a targeted request has to wait for them or be handled in the database by an operator.

### 5.2 Admin calls for data subject requests (planned)

```
POST /v1/subjects/lookup   {"address": "user@example.com"}    scope admin
POST /v1/subjects/erase    {"address": "user@example.com"}    scope admin
```

- `lookup` returns every mail record and its events for the address as JSON, without content. This is the Postwerk part
  of an access request. Art. 15(1) also asks for purposes, recipients and the storage period; those come from the
  operator's records and from this page (the provider is a recipient).
- `erase` deletes `raw`, replaces addresses with a tombstone, clears `ref` and `message_id` if configured, and writes one
  `subject.erased` event with the number of affected mails and no address.
- Both use a request body, never a URL, so the address stays out of access logs.

Erasure has exceptions. Art. 17(3) lists among others a legal obligation that requires the processing and the
establishment, exercise or defence of legal claims. Whether a delivery record must be kept is the operator's decision.

## 6. Security: who can read what

### 6.1 Access table

| Actor | Content | Addresses | Metadata | Mitigation |
|---|---|---|---|---|
| Holder of a `read` token *(planned API)* | no | yes, and can test any address by lookup | yes | masked addresses for `read` (change 9), lookups by body and logged with token id (change 8), rate limit |
| Holder of an `admin` token *(planned API)* | no; can trigger a resend | yes | yes | few holders, named tokens, every call is an event |
| Shell or exec in the container | **yes**, plus key files and memory | yes | yes | the image has no shell; restrict exec and debug rights; encryption does not help here |
| Copy of the volume or a backup | **no**, unless the key file is in the same copy | yes until hashed; hashes are useless without the hash key | yes | keep key files out of volume backups; backups encrypted and short-lived ([scope.md gap 10](scope.md#7-gaps-between-the-ideal-and-the-current-design)) |
| Cluster admin | **yes** (can read secrets and exec) | yes | yes | a trust decision; Postwerk cannot protect against it |
| Network observer inside the cluster | **yes** if ingress uses cleartext or a target uses `smtp+plain://`; sender passwords too with cleartext `AUTH` | yes | yes | STARTTLS on ingress (required for `AUTH` unless explicitly allowed), TLS required towards targets by default |
| Provider | **yes** | yes | yes | processor contract (section 7), the provider's retention settings |
| Anyone who can open the capture tool | **yes**, including reset links and one-time codes | yes | yes | not in production by default; if needed: mails with credentials skipped, UI authentication, short retention ([README](../README.md#mailpit-recommended-for-development-and-testing-not-for-production)) |
| Log and monitoring readers | no | no (redacted) | ids, sender names, codes, times | redaction and leak test (built); no SDK defaults for the planned Sentry sink (change 12) |
| Compromised sender credential | only mails it sends itself | no | its own replies only | per-sender limits, allowed envelope senders ([gap 13](scope.md#7-gaps-between-the-ideal-and-the-current-design)), recipient allow-list; it can send spam or phishing through the provider, not read stored mail |

### 6.2 Encryption of the raw message at rest

Built scheme:

- Each raw message is encrypted with AES-256-GCM and a random nonce before it is written. The mail id is the additional
  authenticated data, so a ciphertext cannot be moved to another row and still decrypt.
- Each ciphertext is stored with the id of the key that encrypted it. New mails use the newest key. Older keys stay in
  the key file for decryption until no row needs them; since the raw message is never older than the maximum age
  (7 days by default), an old key can be removed once that period has passed after rotation. From then on, leftovers in
  free pages, the WAL file or old backups cannot be decrypted.
- There is no per-mail data key wrapped by a master key (envelope encryption). Envelope encryption pays off when data
  lives long and the master key sits in a separate key service. Here the content lives hours, not months, and the key
  id already gives rotation. Encrypting directly keeps the code and the failure modes small.

**Where the key lives.** In a key file, mounted into the container from a secret (for example a Kubernetes Secret or
a Docker secret), readable by the Postwerk user only. It is never in the store, in the image or in the logs.
`postwerk gen-key` creates a new random key. The format and the variable that names the file are in
[configuration.md](configuration.md).

**Why a file, not an environment variable.** Environment variables reach more places than a file: process listings
and `/proc/<pid>/environ`, `docker inspect`, crash dumps, debug output of orchestration tools, and every child process.
A mounted secret file can be updated in place, holds several keys with their ids for rotation, and has file
permissions. The same rule applies to sender passwords, target credentials and the hash key.

What it protects against: a copied volume, a lost disk, a backup, a forensic read of deleted pages, as long as the key
file is not in the same copy.

What it does not protect against: anyone who can run code in the process or read its mounted secrets (shell in the
container, cluster admin), and anyone who controls a target. It does not hide addresses and metadata, which stay in
plain columns until they are pseudonymized. An option is to store addresses encrypted from the start, next to their
HMAC; "pseudonymize after N days" would then mean deleting the ciphertext and keeping the hash.

### 6.3 Other points

- **Hash key.** In its own key file, never in the store, never logged. Separate from the encryption key, so one leak
  does not give both.
- **File permissions.** The database file is created with mode `0600`. Key files and the data directory should be
  readable by the Postwerk user only (`0600`, directory `0700`).
- **Deleted data.** SQLite `secure_delete` is on, so deleted content is overwritten
  ([SQLite docs](https://www.sqlite.org/pragma.html#pragma_secure_delete)). Old page copies can sit in the WAL file
  until a checkpoint, so the retention job checkpoints after a purge.
- **Sender passwords.** The senders file holds PBKDF2-SHA256 hashes (`postwerk hash-password`, 600,000 iterations;
  fewer than 100,000 are refused). Only `AUTH PLAIN` is offered; it works with hashes. Mechanisms that need the
  plaintext on the server (such as `CRAM-MD5`) are not offered. A connection is closed after 3 failed attempts. Target
  credentials must stay usable, so they stay in plain files; protect those by permissions.
- **TLS on ingress.** `AUTH` needs STARTTLS with a configured certificate. Cleartext `AUTH` must be allowed explicitly
  (`POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true`), only on a local machine or a network that encrypts traffic between nodes.
- **TLS on egress.** `smtps://` or `smtp://` with STARTTLS required; cleartext only with `smtp+plain://`, which refuses
  credentials.
- **Tokens** *(planned)*. Stored as hashes in a tokens file, one token per person or team, so the actor in token-use
  events means something. Log the call and the token id, never the parameters.
- **UI** *(planned)*. Every page needs at least a `read` token. Admin actions need CSRF protection. Never expose the
  listener to the internet.
- **Headers to recipients.** Postwerk removes every `X-Postwerk-*` header set by the sender, including `X-Postwerk-Ref`,
  and adds only `X-Postwerk-Id` (and `X-MJ-CustomID` for Mailjet). The id reaches the recipient; it carries no personal
  data. A `Message-ID` with an internal hostname also reaches the recipient; that is the sender's choice.
- **Capture targets in production.** A capture tool holds working password reset links. Whoever can open it can take
  over accounts. Default: no capture target in production. If one is needed for a short time: mails with credentials
  skipped, authentication on its UI, retention of a day or less, removed afterwards.

## 7. Legal frame

Postwerk is software, not a service. The organisation that runs it decides why and how mail is sent, so it is the
controller, or a processor if it runs Postwerk for a customer. The relevant rules, with references to the
[GDPR text on EUR-Lex](https://eur-lex.europa.eu/eli/reg/2016/679/oj):

| Rule | What it asks | Postwerk can support | Stays with the operator |
|---|---|---|---|
| Art. 5(1)(c) data minimisation | data adequate, relevant and limited to what is necessary | no content in events, no subject field, addresses redacted from texts, raw deleted at hand-off | keep `ref` free of personal data |
| Art. 5(1)(e) storage limitation | identifiable form no longer than necessary | retention settings with short defaults, pseudonymization | choose and justify the values |
| Art. 5(2) accountability | be able to demonstrate compliance | this page, retention events | the documentation itself |
| Art. 6 lawful basis | every processing needs a basis | — | Sending usually rests on the basis of the mail itself, e.g. a contract (6(1)(b)) or a legal obligation (6(1)(c)). Keeping delivery records is often based on legitimate interest (6(1)(f)), which needs a balancing test; short retention helps it. |
| Art. 15, 17 access and erasure | answer and act on requests | planned `subjects/lookup` and `subjects/erase` (section 5) | handle the request, including provider, sinks, backups and the sending application |
| Art. 25 privacy by design and by default | by default only the data necessary, also as to storage period and accessibility (25(2)) | private defaults (section 8) | not weaken them without a reason |
| Art. 28 processors | use only processors with sufficient guarantees, under a contract | — | contract with the provider, and with the hoster if it can access the data |
| Art. 30 records of processing | keep a record, including where possible time limits for erasure (30(1)(f)) | the tables in sections 2 and 3 as input | write the record; Art. 30(5) has an exemption for organisations under 250 staff, which does not apply when the processing is not occasional |
| Art. 32 security | measures appropriate to the risk; names pseudonymisation and encryption (32(1)(a)) | keyed hashing, encryption at rest, TLS by default, hashed sender passwords | access control around the cluster, backups, key handling |
| Chapter V (Art. 44 ff.) transfers | transfers to third countries only under the Chapter's conditions | — | check where the provider and its subprocessors process data |

German telecommunications secrecy (§ 3 TDDDG) binds providers of telecommunications services
([§ 3 TDDDG](https://www.gesetze-im-internet.de/ttdsg/__3.html)); an organisation relaying only its own application mail
is usually not one, but an operator relaying mail for third parties should check whether it applies.

## 8. Private by default

### 8.1 Defaults

| Area | Default | Status |
|---|---|---|
| Raw message | encrypted at rest; deleted at hand-off; 72 h after a failure; never older than 7 days | built |
| Addresses | plain for 30 days, then keyed hash | built, for recipients and the envelope sender |
| Mail and delivery rows, events | 90 days | built |
| Free text from targets | addresses redacted before storing or exporting | built |
| Provider status | stored as a fixed word | planned with the adapter |
| Subject | not stored | built |
| API and UI | token required for every page; addresses masked for `read`; address lookups by request body, logged and rate-limited | planned |
| Deleted data | SQLite `secure_delete` on; checkpoint after purges | built |
| Secrets | sender passwords as hashes; hash key and encryption key in separate key files | built; tokens as hashes planned with the API |
| Sinks | no addresses; no SDK breadcrumbs or request data | stdout built; others planned |
| Capture targets | none in production; if used: no sensitive mails, UI authentication, retention of a day or less | recommendation ([README](../README.md#mailpit-recommended-for-development-and-testing-not-for-production)) |

### 8.2 Privacy changes and their status

Proposed in an earlier review of the design. Related gaps in
[scope.md section 7](scope.md#7-gaps-between-the-ideal-and-the-current-design) are referenced, not repeated.

| # | Change | Status |
|---|---|---|
| 1 | Strip addresses from all free text before storing or exporting; keep reply code and enhanced status code | built |
| 2 | Delete the raw message at hand-off; failure window and hard maximum age | built |
| 3 | Retention for every table | built |
| 4 | Pseudonymize the envelope sender too (sending as a person, VERP bounce addresses) | built |
| 5 | Keyed hash: HMAC-SHA256, key id stored with each hash, several keys for rotation | built |
| 6 | Encrypt the raw message at rest | built, as direct AES-256-GCM per row with key id (section 6.2), not envelope encryption; encrypting addresses too is open |
| 7 | SQLite `secure_delete`, checkpoint after each retention run | built |
| 8 | Keep addresses out of URLs and logs: `POST /v1/mails/search` instead of `GET /v1/mails?to=…`; token-use events record the call and the token id, not its parameters | open, with the query API |
| 9 | Mask addresses for `read` tokens (`u***@example.com`), full address for `admin` | open, with the query API |
| 10 | Token required for the UI, tokens stored as hashes, CSRF protection for admin actions | open, with the UI |
| 11 | Hash sender passwords; refuse `AUTH` without TLS unless explicitly allowed | built, as PBKDF2-SHA256 and `AUTH PLAIN` only (decisions D15, D16) |
| 12 | Sentry sink without SDK defaults: each error event built from the event record only, no breadcrumbs, no request data | open, with the Sentry sink |
| 13 | `ref` must be opaque; a `ref` with `@` is rejected | built (such a `ref` is not stored) |
| 14 | Leak test: a canary address must not appear in logs, exported events, the events table or reply texts | built; one test also searches the database file and the WAL byte by byte after a retention run |
| 15 | Admin calls for data subject requests (section 5.2) with `subject.erased` events | open |
| 16 | Provider adapter stores only the provider id and a mapped status word, never addresses, subjects or bounce texts | open, with the adapter |
| 17 | Capture guidance in the README | done |
