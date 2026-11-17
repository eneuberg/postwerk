# Postwerk scope and fit

What Postwerk should do, what it should not do, how it differs from other ways to send mail, and whether it fits your
setup. It starts from the ideal and compares it with the current [design](design.md) at the end.

Status: version 1.0, a young project. The foundation is built (SMTP ingress, durable queue, per-target delivery with retries,
event log on stdout and to Sentry or GlitchTip, retention and encryption at rest, the internal UI and the query API,
with an admin's close and resend of a delivery); provider status and the OpenTelemetry and webhook sinks are planned.
Little here is measured production behavior yet.

## 1. The ideal in one paragraph

Inside a server or cluster, many applications send mail: password resets, invoices, notifications. Each one talks to a
provider on its own, handles errors on its own, and nobody can answer "did mail X get out, and where did it go?" from
one place. Postwerk is the one checkpoint every outgoing mail passes through. It takes a finished mail over SMTP, stores
it before it says "accepted", delivers it to a fixed list of targets (a sending provider, a capture tool, or both),
records every step in an append-only log, and reports problems to the tools you already watch. It does nothing else.
It needs no special rights, no inbound port from the internet and no database server.

## 2. Should do and should not do

### Should do

| # | Postwerk should | Why |
|---|---|---|
| 1 | Accept finished mails over SMTP with a login per sender | Every framework can send SMTP; the login tells you which application sent what. |
| 2 | Store the mail durably before replying `250` | A `250` is a promise. It must survive a crash one millisecond later. |
| 3 | Deliver to a list of targets, each tracked on its own | Production, tests and "both at once" use the same mechanism; one broken target does not block another. |
| 4 | Retry temporary errors with backoff, within a window | Providers have short outages; the application should not have to care. |
| 5 | Treat its own misconfiguration (`535`, rejected login) as an outage, not as a property of the mail | Rotated credentials must not turn into a pile of failed mails. |
| 6 | Avoid duplicates by default, and make the unclear case an explicit, per-target choice | After `DATA`, nobody knows whether the target has the mail. Guessing silently is worse than either answer. |
| 7 | Keep an append-only audit trail per mail and per target | "Did it get out?" needs facts with timestamps, not log greps. |
| 8 | Ask the provider for the final status after hand-off | The provider's `250` only means the provider has it. Bounces and blocks happen later. |
| 9 | Report facts as events to existing tools (stdout, OpenTelemetry, error tracker, webhook) | You already have monitoring. Postwerk should feed it, not replace it. |
| 10 | Offer a read-only lookup API and a small internal UI | Support and developers need to find one mail quickly, without shell access. |
| 11 | Stay content-blind: forward the message unchanged except for tracking headers | Content (links, tokens, personal data) stays the responsibility of the system that wrote it. |
| 12 | Minimize stored personal data: drop the raw message when done, pseudonymize recipient addresses after a period, keep no content in events | Recipient addresses are personal data. A relay should not become an archive. |
| 13 | Use TLS towards every target outside the local network, and refuse to fall back to plaintext there | Mail content and provider credentials cross the internet. |
| 14 | Limit how fast it sends to each target (connections, and optionally messages per second) | Providers throttle. A burst from one sender should not turn into a wall of `4xx` replies. |
| 15 | Offer a recipient allow-list per target | So a staging setup with a real provider target can never mail real users by accident. This is an envelope check, so it stays content-blind. |

On item 15, the argument: the target list already makes a staging setup with only Mailpit safe by construction. The
risk is the mixed case ("tests against a real provider"), where one wrong address in a test fixture reaches a real
person. An allow-list such as `*@example.test, qa-*@your-domain` per target is a few lines of code, needs no content
access, and protects against a mistake that is hard to undo. Mailpit offers a similar setting for its own relay
feature, which shows the need is real. It is built as the per-target setting `ALLOW_RECIPIENTS`.

Built so far: items 1 to 7, 10 (the UI and the query API, read-only except for an admin's close and resend), 11 to 13 and 15, and item 9 for stdout and
the error tracker (Sentry protocol). Planned: items 8 and 14, and item 9 for OpenTelemetry and webhooks.

### Should not do

| Postwerk should not | Why |
|---|---|
| Receive mail for mailboxes, act as MX, or offer IMAP/POP | That is a full mail server with spam, storage and DNS duties. Use one if you need it. |
| Deliver directly to recipients' MX servers | Direct delivery needs port 25 egress, a clean IP, reverse DNS and reputation work. The provider does this better. |
| Sign mail with DKIM | The provider signs with your domain's key. A second signer adds key management for no gain. |
| Render templates, build links, add tracking pixels | Content belongs to the application (see design decision D3). |
| Decide what an alert is | Postwerk reports facts with levels. Thresholds and paging rules live in your monitoring. |
| Filter spam or scan content | It only relays mail from known internal senders and does not read content. |
| Process bounces into applications (suppression lists, parsing bounce mails, disabling users) | Postwerk reports `bounced` as a fact, per mail. What an application does about it is the application's decision. The provider already keeps its own suppression list. |
| Generate bounce mails (DSN) back to senders | Applications rarely read a mailbox. Failures go out as events instead. |
| Fail over from one provider to another | Failover needs SPF and DKIM set up at both providers and risks duplicates. Targets are copies, not fallbacks. |
| Offer tenants, billing, per-customer domains or a public API | It is an internal component, not a sending service. |
| Handle newsletters, list management or unsubscribe | Bulk mail has different rules and volumes. Use a provider's marketing tools. |
| Offer priority classes or complex routing rules | Every rule is a moving part. A fixed target list is easy to reason about. |
| Run as a highly available cluster | That needs a shared store and leader election. See the costs in section 3. |

### What stays with you

Postwerk takes on as little as it can. It stores, forwards and records; everything around that stays with the
operator:

| Task | Where it belongs |
|---|---|
| Deciding which events are alerts, who gets paged, and alerting when Postwerk itself is down | your monitoring: GlitchTip or Sentry rules, log alerts, health checks |
| What happened after hand-off: delivered, bounced, blocked, marked as spam | the provider's dashboard and API; Postwerk's record ends at `handed_off` |
| Bounce suppression, and what an application does about a bounce | the provider's suppression list and the application. Postwerk sends no bounce mails (DSN) and passes the envelope sender to the target unchanged, so bounces go where the provider sends them |
| Backups of the data volume and the key files, and restore tests | you ([kubernetes.md](kubernetes.md#8-backups)) |
| SPF, DKIM, DMARC and the sending domain | DNS and the provider |
| Provider account, sending limits, contract and data processing agreement | you and the provider |
| Issuing and renewing TLS certificates | cert-manager or your CA; Postwerk only reloads the files |
| Who can reach SMTP, the UI and the API | NetworkPolicy, firewall, RBAC for `port-forward` |
| Making, storing and rotating secrets: sender passwords, target credentials, keys, tokens | your secret store |
| Retention periods in your records of processing, and requests from data subjects | the organisation that runs Postwerk ([privacy-and-security.md](privacy-and-security.md)) |
| Retrying when Postwerk is briefly unreachable, and not sending the same mail twice | the sending application (section 6) |
| Upgrading to new releases | you ([versioning.md](versioning.md)) |

## 3. What makes it lightweight

"Lightweight" here means few things to run and few rights to grant. It does not mean few features. Each property below
can be checked in the deployed container.

| Property | How to check |
|---|---|
| One static binary, no C dependencies (pure-Go SQLite) | `file postwerk` shows a statically linked executable |
| One process | one process in the container, no supervisor, no helper daemons |
| One data file (SQLite with its WAL files) | one volume mounted at the data path |
| No database server, broker or cache | nothing else in the deployment |
| Runs as non-root | `securityContext.runAsNonRoot: true` works |
| Read-only root file system | `readOnlyRootFilesystem: true` works |
| No Linux capabilities | `capabilities.drop: [ALL]` works; ports are above 1024 |
| No host network, no cluster API access | no service account token needed |
| No inbound port from the internet | SMTP ingress and HTTP listen for internal traffic only; provider status is polled |
| No DNS MX lookups, no port 25 egress | only egress to the configured targets and the provider API |
| Configuration by environment variables and secret files | no config language to learn |
| Small dependency list | today: the SMTP library, its SASL package and the SQLite driver (`go.mod`); every new dependency needs a recorded reason |

What it costs:

- **Single instance.** One SQLite file has one writer. Two copies of Postwerk cannot share it: a second one waits for
  the first to stop (D61).
- **No high availability.** During a restart or upgrade, the SMTP port is closed for a few seconds. Senders see a
  refused connection.
- **Local disk only.** SQLite needs a local file system with working locks. Network file systems are not supported.
- **Lose the volume, lose the queue.** Mails not yet handed off are gone if the disk is gone. Normally a mail waits only
  seconds.
- **No own MX, no direct delivery.** You need a provider or another SMTP server as a target.
- **Throughput is bounded.** Every accepted mail is written with a full sync before `250`. That favors safety over speed.
  It suits transactional mail; bulk volumes have not been measured and are not a goal.

## 4. Comparison

### Postwerk, Postfix, maddy

| | Postwerk | Postfix | maddy |
|---|---|---|---|
| What it is | outbound relay with audit trail | full mail transfer agent | all-in-one mail server (SMTP, submission, IMAP, DKIM) |
| Processes | one | several daemons (`master`, `smtpd`, `qmgr`, `smtp`, `cleanup`, …) | one |
| Privileges | non-root from the start | `master` starts as root and drops privileges | can run as non-root |
| Configuration | environment variables, secret files | `main.cf`, `master.cf`, lookup tables | own configuration language |
| Queue | rows in one SQLite file | files in the queue directory | its own queue module |
| Inbound mail, MX, mailboxes | no | yes | yes |
| DKIM signing | no (provider does it) | via milter (e.g. OpenDKIM) | built in |
| Upstream login rejected (`535`) | queue held, one error event at once, login probed with backoff | deferred and retried by default (`smtp_sasl_auth_soft_bounce = yes`) | in our reading of the v0.9.7 source, treated as a permanent failure (not retried) on the SMTP downstream |
| Several destinations per mail | list of targets, each with its own status | extra recipients via `always_bcc` or `recipient_bcc_maps`; not tracked as separate deliveries for audit | several `deliver_to` targets possible per the docs |
| Per-mail status lookup | internal UI and a JSON query API by id, sender reference, `Message-ID` or address, plus the event log | log files and `postqueue` | logs |
| Provider status after hand-off | planned, polled (Mailjet first) | no | no |
| Error reports to monitoring | events as JSON on stdout, error events to Sentry or GlitchTip; OpenTelemetry, webhook planned | via log shipping | via log shipping |
| Maturity | 1.0, young project, little production use yet | decades of production use | younger project |

The point is not that Postfix or maddy are worse. They are complete mail servers. They solve transport, including the
hard parts Postwerk leaves to a provider. What they do not give you out of the box is a per-mail answer to "what
happened to this mail, at each destination", and they bring more to configure and secure than an outbound-only relay
needs.

### Other common choices

| Choice | Good at | Missing for this job |
|---|---|---|
| Each app calls the provider's HTTP API | simplest; provider dashboard shows status | every app handles retries and outages on its own; no audit across apps; provider-specific code in every app |
| Each app sends to the provider's SMTP relay directly | no extra component | provider outage or rotated credentials become errors in the app at send time; no local queue; no central audit |
| Postfix-based SMTP relay container (common in Kubernetes) | queue and retries; well-known behavior | same as Postfix: log-based status, `master` starts as root, no per-target tracking or UI |
| Mailpit or MailHog alone | capture and inspect mails in development | built for testing. Mailpit can relay to an upstream (`--smtp-relay-all`, with `blocked-recipients`), but its docs describe no retry queue and no per-mail delivery status for relayed mail |

## 5. Does it fit you?

**It fits when:**

- Several applications in one server or cluster send transactional mail.
- You want one place to answer "did mail X get out, when, and what did the provider say?".
- You use a sending provider (for example Mailjet) and want problems to show up in your existing monitoring.
- You want the same setup in tests (Mailpit as target) and in production (provider as target), or both at once.
- You run with few rights: no root, no special cluster permissions, no public ingress.
- A few seconds of refused connections during an upgrade are acceptable, because senders retry or the gap is rare.

**It does not fit when:**

- You need to receive mail, host mailboxes or be an MX.
- You need zero ingress downtime or active-active high availability.
- You send bulk or marketing volume.
- You want to deliver directly to recipients' servers without a provider.
- You want to sign DKIM yourself or manage IP reputation yourself.
- You have one application that already uses the provider's API, and you do not need a central audit trail. Postwerk
  would only add a hop.
- Your only persistent storage is a network file system.

## 6. Before you integrate

**Senders.**

- Senders must speak SMTP with `AUTH`. Each application gets its own credentials.
- Set `X-Postwerk-Ref: <your id>` (for example the id of your own notification record). You can look the mail up by it
  later. Postwerk also returns its own id in the reply: `250 2.0.0 queued as pw_…`.
- Your application's "sent" flag now means "Postwerk has stored it". It does not mean delivered. For delivery, read the
  events (stdout, Sentry) or ask the query API ([api.md](api.md)).
- Postwerk removes every `X-Postwerk-*` header you set before it stores the mail. Towards the targets it adds
  `X-Postwerk-Id` and, for Mailjet targets, `X-MJ-CustomID` and (unless `MAILJET_TRACKING=account`)
  `X-Mailjet-TrackOpen: 0` and `X-Mailjet-TrackClick: 0`, so Mailjet does not rewrite your links. A `ref` that
  contains `@` is not stored.

**Duplicates.** Postwerk avoids sending twice towards its targets. It cannot see duplicates created before it. If your
application's mail library retries after a timeout, and the first attempt did reach Postwerk, you get two mails. To
avoid this:

- Let the mail library time out generously, so the reply has time to arrive.
- Keep the same `Message-ID` and `X-Postwerk-Ref` across retries of the same mail, so duplicates are at least visible.
- If your queue (for example a job runner) already retries, do not add a second retry layer in the mail library.

**Restarts.** The SMTP port is closed for a few seconds during a restart. A sender then gets "connection refused" or a
`421` reply. Treat both as temporary: retry after a few seconds, or let your job queue retry. Do not treat them as a
failed mail. In Kubernetes, use one replica and the `Recreate` deployment strategy, because only one process may use the
SQLite file at a time.

**Network.**

- Egress: to each target (for Mailjet, SMTP on port 587; later also its HTTPS API for status).
- Ingress: SMTP and HTTP only from inside your network. Never expose the UI or API to the internet.
- `AUTH` on ingress needs STARTTLS with a certificate. Cleartext `AUTH` must be allowed explicitly
  (`POSTWERK_SMTP_ALLOW_INSECURE_AUTH=true`); do that only on a local machine or if your network encrypts traffic
  between nodes.
- Towards targets, TLS is required: `smtps://` or `smtp://` with STARTTLS. Cleartext needs `smtp+plain://`, for a local
  capture tool only, and cannot carry credentials.

**Storage and backups.** Use a local disk or block volume, not a network file system. Size it for the raw messages of
a long target outage (mail size × mails per day × days). A copied SQLite file of a running process may be inconsistent;
take a volume snapshot, or stop Postwerk and copy the file ([kubernetes.md](kubernetes.md#8-backups)). If the audit
trail matters to you, keep the event lines from stdout in your log system as well, so they survive the loss of the
volume.

**Secrets.** Sender password hashes, target credentials, the encryption key and the hash key live in secret files, not
in environment variables or images. Rotate target credentials before they expire; a rejected login takes the target down: the
queue is held, one `target.down` error is raised, and the login is probed with backoff until it works.

**Privacy and retention.** Recipient addresses are personal data. Postwerk keeps them in its records and replaces them
with a keyed hash after a configurable period (default 30 days). A hash with a key is pseudonymized, not anonymous. The
defaults are reasoned, not legal requirements; set retention to the shortest period your support process needs, and
write it into your records of processing. Details: [privacy-and-security.md](privacy-and-security.md#3-lifecycle-and-retention).

**Capture targets.** A capture tool such as Mailpit stores full mail content, including password reset links and
one-time codes. Use one in development and tests. In production, add one only temporarily, with its own short
retention and access control. Mark mails with credentials with `X-Postwerk-Skip: <capture target name>` so they never
reach it.

**DNS and authentication of your domain.** SPF, DKIM and DMARC are still set up at the provider. Postwerk does not
change them. It adds only tracking headers, which does not break the provider's DKIM signature. If your application
signs DKIM itself, do not include `X-Postwerk-*` headers in the signed header list.

**Monitoring.** Decide who receives the events: which sink, which level, which person. Two traps:

- Do not send alerts about broken mail through Postwerk. If mail is broken, the alert is too. Use another channel.
- A stopped process sends no events. Alert on the absence of metrics or on failing health checks, not only on error
  events.

## 7. Gaps between the ideal and the current design

These started as suggestions against the design. Each item now says whether it is built or still open; open items do
not change the design until they are accepted there.

1. **Duplicate submissions from senders.** The design prevents duplicates towards targets but not at ingress.
   *Suggestion:* if the same sender submits a mail with the same `X-Postwerk-Ref` (or, without it, the same
   `Message-ID`) within a configurable window, reply `250` with the existing id and store nothing new. Log it as an
   event.
   *Status:* open.

2. **Resend after `failed` is not possible.** The raw message is removed when every delivery is final, and `failed` is
   final. The resend call needs the raw message. A credential outage longer than the 24 h retry window also turns every
   queued mail into `failed` and then deletes it.
   *Suggestion:* keep the raw message for a configurable period after a failure (for example 72 h). Consider a separate,
   longer window for errors on Postwerk's own side (`535`, target unreachable), since these are not about the mail.
   *Status:* built: the raw message is kept 72 h after a failure by default, and an admin can resend within it in the
   UI or the API (D54); it is never kept longer for a resend. A separate window for Postwerk's own errors is open.

3. **Every deploy can create `unknown` deliveries.** If the process stops while it is sending `DATA` to a target, the
   outcome is unclear. With `hold`, each deploy could leave work for an admin.
   *Suggestion:* define graceful shutdown: stop accepting new sessions (`421`), finish incoming transactions, finish
   outgoing transactions that are already in `DATA`, start no new ones, then exit. Document the needed grace period.
   *Status:* built: a send in progress is always finished; the deadline is `POSTWERK_SHUTDOWN_TIMEOUT` (default 100 s).

4. **Outbound TLS is not specified.** The target URL is `smtp://` or `smtps://`, but the design does not say whether
   STARTTLS is required on `smtp://`.
   *Suggestion:* require TLS on every target by default. Allow plaintext only with an explicit per-target setting (for a
   local Mailpit).
   *Status:* built: `smtp://` requires STARTTLS, cleartext needs the scheme `smtp+plain://` and refuses credentials.

5. **No recipient allow-list for test setups.** See item 15 in section 2.
   *Suggestion:* per-target `ALLOW_RECIPIENTS` with address or domain patterns. Recipients that do not match are dropped
   for that target and logged. If a mail has no allowed recipient on any required target, reject it at `RCPT TO` so the
   sender sees the mistake.
   *Status:* built in a simpler form: if any recipient does not match, the mail is not delivered to that target
   (`skipped`, event `delivery.not_allowed`, level `warn`). There is no rejection at `RCPT TO` yet.

6. **Keeping credentials out of capture tools depends on matching names.** `X-Postwerk-Skip` takes target names. If an
   application writes `capture` and the deployment calls the target `mailpit`, the mail reaches the capture tool and
   the mismatch is only logged. The design also lets a sender skip a required target.
   *Suggestion:* add a header such as `X-Postwerk-Sensitive: yes` and a per-target setting `ACCEPT_SENSITIVE`, which is
   `false` for optional targets by default. This fails safe. Allow skipping only for optional targets.
   *Status:* open.

7. **Internal headers reach recipients.** The design removes `X-Postwerk-Skip` but not `X-Postwerk-Ref`, so a sender's
   internal id goes to the recipient.
   *Suggestion:* remove all `X-Postwerk-*` headers set by senders before delivery. Keep `X-Postwerk-Id` only where it is
   useful (capture targets), configurable per target. List in the README exactly which headers reach the recipient.
   *Status:* built in part: every `X-Postwerk-*` header set by the sender is removed before storage. `X-Postwerk-Id` is
   added for every target (and `X-MJ-CustomID` for Mailjet); making it configurable per target is open.

8. **No rate or concurrency limit towards targets.** The design limits connections at ingress (in total), not
   per sender or per target at egress. A large batch from one sender can also delay a password reset from another.
   *Suggestion:* per-target maximum connections and optional messages per second. Pick due deliveries round-robin across
   senders, so one sender cannot starve the others. No priority classes.
   *Status:* open (requirement D12, later phase).

9. **Full disk is not handled.** A long target outage keeps raw messages on disk.
   *Suggestion:* reply `452` (temporary) at ingress when free space or queue size crosses a limit, fail `readyz`, and
   raise an event. Senders then retry instead of losing mail.
   *Status:* built for free space (`POSTWERK_MIN_FREE_BYTES`, `disk.low`, D50); a mail that waits too long raises
   `queue.stale` (D49). A limit on the queue size is open.

10. **No backup or restore story.**
    *Suggestion:* a `postwerk backup <file>` command that uses SQLite's online backup, and a short restore guide. Note
    that events exported to sinks are a second copy of the audit trail.
    *Status:* in part: [kubernetes.md](kubernetes.md#8-backups) describes volume snapshots, a copy with Postwerk
    stopped, and restoring before a downgrade ([versioning.md](versioning.md#4-upgrades-and-the-database)). Backups
    stay with the operator; a `backup` command is open.

11. **Events are hard to map back in applications.** Events carry no recipient address (correct) but also no sender
    reference. An application that wants to react to `provider.bounced` must call the API for each event.
    *Suggestion:* include `ref` in events. The sender chooses it, so it can be kept free of personal data. This is the
    supported way to "process bounces in the app" without Postwerk doing it.
    *Status:* open.

12. **Secret rotation needs a restart.** Fixing a `535` means new credentials, and a restart closes the ingress.
    *Suggestion:* re-read secret files when they change (mounted secrets in Kubernetes update in place).
    *Status:* in part: the STARTTLS and API certificates are re-read every minute (D38), the senders file every 30 s
    (D66). Target credentials, keys and tokens are still read once at start.

13. **Senders can use any envelope sender.** A compromised application could send mail with another application's
    address.
    *Suggestion:* optional per-sender list of allowed envelope-from addresses or domains. This is an envelope check and
    stays content-blind. The provider's own domain checks remain the second line.
    *Status:* built in 1.1: `from=` in each sender's policy, `550 5.7.1` otherwise, together with rate limits per
    sender and one key per application (D62–D64).

14. **Bounces and envelope sender are not described.** The design does not say that Postwerk sends no bounce mails
    (DSN) to senders, or which envelope sender it passes to the target.
    *Suggestion:* state both: no DSN, envelope sender passed through unchanged, bounce handling belongs to the provider.
    *Status:* built: no DSN, envelope sender passed through unchanged, bounces stay with the provider
    ([What stays with you](#what-stays-with-you)).

15. **Dependency weight is not yet bounded.** The OpenTelemetry and Sentry SDKs bring many indirect dependencies, and
    the planned feature list (four sinks, status adapter, API, UI) is large for a "thin" relay.
    *Suggestion:* use OTLP over HTTP only. Consider sending Sentry-protocol events with the standard library. Define a
    first release with ingress, store, delivery, retry policy, stdout events and the lookup API; add the rest after it
    runs.
    *Status:* in part: the foundation (ingress, store, delivery, retry policy, stdout events, health checks) was built
    first with three direct dependencies. The Sentry-protocol sink (D30), the UI and the query API use the standard
    library and added none. The same choice for the OpenTelemetry sink is open.
