<img src="docs/logo.svg" width="72" height="72" alt="Postwerk logo: an envelope that fans out to three targets">

# Postwerk

Postwerk is a small, self-hosted SMTP relay for outgoing transactional email, written in Go. Applications in a server,
a Docker setup or a Kubernetes cluster hand their mails to Postwerk over SMTP. Postwerk stores each mail before it
accepts it, delivers one copy to each configured target (a sending provider such as Mailjet, a capture tool such as
Mailpit, or any SMTP server), retries temporary failures, and records every step in an append-only audit log. That
gives one place for delivery tracking of all outgoing mail. It runs as one static binary with one SQLite file, as a
non-root user.

**Status: early development, not production-ready.** The foundation is built and tested: SMTP ingress with a login per
sender, a durable queue in SQLite, per-target delivery with retries, the event log with JSON output on stdout, and
retention and encryption of stored mail. Not built yet: the query API, the web UI, delivery status from the provider,
and the OpenTelemetry, Sentry-protocol and webhook outputs.

![Animated diagram: applications send a mail to Postwerk over SMTP; Postwerk stores it, writes an event, and delivers one copy each to Mailjet and Mailpit](docs/flow.svg)

## Why Postwerk

- **One place for all outgoing mail.** Every application sends through the same relay, with its own login, instead of
  each one talking to the provider on its own.
- **Every mail is traceable.** Each mail gets an id (`pw_…`) and one tracked delivery per target. Every attempt, retry
  and result is an event with a timestamp and the target's reply code.
- **No mail lost when the provider is down or credentials rotate.** Mails wait in the queue and are retried with
  backoff for up to 24 hours. A rejected login (`535`) keeps the mails queued and raises an error at once, instead of
  failing them.
- **Small.** One binary, one SQLite file, no database server. The container runs as non-root on a read-only root file
  system, with no Linux capabilities and no cluster permissions.

## Quick start with Docker Compose and Mailpit

The example in [examples/compose.yaml](examples/compose.yaml) runs Postwerk with Mailpit as its only target. It is for a
local machine only.

```sh
docker build -t postwerk:dev .
docker compose -f examples/compose.yaml up
```

Send a test mail with `curl`. The example sender is `example-app` with the password `change-me-local-only`:

```sh
printf 'From: app@example.test\r\nTo: user@example.test\r\nSubject: Hello\r\n\r\nTest mail.\r\n' > mail.txt
curl smtp://localhost:2525 --user example-app:change-me-local-only \
  --mail-from app@example.test --mail-rcpt user@example.test --upload-file mail.txt
```

Open http://localhost:8025 to see the mail in Mailpit. The Postwerk log shows one JSON line per event
(`mail.accepted`, `delivery.attempt`, `delivery.handed_off`).

For your own senders and keys:

```sh
# Hash a password for the senders file (one name:hash line per application).
printf '%s' 'your-password' | docker run --rm -i postwerk:dev hash-password

# Create a new key for a key file (encryption at rest, address hashing).
docker run --rm postwerk:dev gen-key
```

A minimal production-style configuration with a provider as the only target:

```sh
POSTWERK_SENDERS_FILE=/run/secrets/senders
POSTWERK_SMTP_TLS_CERT_FILE=/run/secrets/tls.crt            # AUTH needs TLS on ingress
POSTWERK_SMTP_TLS_KEY_FILE=/run/secrets/tls.key
POSTWERK_TARGETS=mailjet
POSTWERK_TARGET_MAILJET_URL=smtp://in-v3.mailjet.com:587    # STARTTLS required
POSTWERK_TARGET_MAILJET_CREDENTIALS_FILE=/run/secrets/mailjet
# plus the key files for encryption at rest and address hashing, see docs/configuration.md
```

Every variable, its default, and the key files are described in [docs/configuration.md](docs/configuration.md).
Unknown `POSTWERK_*` variables stop the start, so a typo is never silently ignored.

## Mailpit: recommended for development and testing, not for production

[Mailpit](https://mailpit.axllent.org) ([source on GitHub](https://github.com/axllent/mailpit)) is an SMTP server with a
web UI and an API that keeps every mail it receives. Postwerk does not need it, but it fits well as a target.

**In development and testing, add Mailpit as a target.** Every mail Postwerk accepts lands there. You can open each
one and check the template, the links and the recipients. Automated tests can read the mails through Mailpit's API and
assert on them. With a real provider and Mailpit as two targets, a test run also shows exactly which mails were sent.
Use the cleartext scheme for a local Mailpit, and resend on an unclear outcome, since a duplicate does no harm there:

```sh
POSTWERK_TARGETS=mailpit
POSTWERK_TARGET_MAILPIT_URL=smtp+plain://mailpit:1025
POSTWERK_TARGET_MAILPIT_ON_UNKNOWN=retry
```

`smtp+plain://` must be chosen explicitly and cannot carry credentials. Plain `smtp://` requires STARTTLS, which a
default Mailpit does not offer.

**In production, do not use a capture target by default.** A capture tool stores every mail in plain text, including
password reset links and one-time codes. Anyone who can open it can take over accounts. That is a privacy and a
security problem. In production, Postwerk's own records are meant to be enough to audit and trace mails without keeping
their content: the event log (built), and the status lookup and error reporting (planned).

**If you need a capture copy in production for a short time**, for example to debug one integration:

- mark the target `OPTIONAL`, so its failures are warnings, not errors;
- keep mails with credentials out of it with the header `X-Postwerk-Skip: <target name>` (a dedicated sensitive-mail
  flag that fails safe is planned, see [scope.md gap 6](docs/scope.md#7-gaps-between-the-ideal-and-the-current-design));
- protect Mailpit's UI with `--ui-auth-file` and limit its retention with `--max-age` (for example `1d`);
- remove the target again when you are done.

## What a `250` reply from Postwerk means

`250 2.0.0 queued as pw_…` means Postwerk has stored the mail and is now responsible for it. It does not mean the mail
was delivered. Each target's result is recorded later as an event. If the data volume is lost before a mail is handed
off, that mail is lost.

## Postwerk compared with Postfix and maddy

Postfix and maddy are mature, complete mail servers. Postwerk is a thin relay for applications that only send.

| | Postwerk | Postfix | maddy |
|---|---|---|---|
| What it is | outbound relay with audit log | full mail transfer agent | all-in-one mail server (SMTP, IMAP, DKIM) |
| Moving parts | one binary, one SQLite file | several daemons, file-based queue | one binary |
| Runs as | non-root | `master` starts as root, drops privileges | can run as non-root |
| Configuration | environment variables, secret files | `main.cf`, `master.cf` | own config language |
| Receives mail for mailboxes | no | yes | yes |
| Several destinations per mail | list of targets, each tracked on its own | extra recipients (`always_bcc`), not tracked separately | several `deliver_to` per block |
| Upstream login rejected (`535`) | mails stay queued, error at once, retried | deferred and retried by default | permanent failure, in our reading of the v0.9.7 source |
| Per-mail audit | append-only event log; query API and UI planned | log files, `postqueue` | logs |
| Provider delivery status | planned (Mailjet first, by polling) | no | no |
| Maturity | early development | decades of production use | younger project |

Use Postfix or maddy if you need to receive mail, host mailboxes or sign DKIM yourself. More detail and other options
(direct provider API, Postfix relay containers, Mailpit alone): [docs/scope.md](docs/scope.md).

## Documentation

- [Design](docs/design.md): components, targets, mail lifecycle, retry policy, event model, planned API
- [Code architecture](docs/architecture.md): packages, data flow, store schema, delivery worker
- [Configuration](docs/configuration.md): every environment variable, secret files and key files
- [Requirements](docs/requirements.md): numbered requirements, built and later
- [Decisions](docs/decisions.md): design decisions and their reasons
- [Scope and fit](docs/scope.md): what Postwerk does and does not do, comparison, integration checklist
- [Privacy and security](docs/privacy-and-security.md): what is stored, for how long, who can read it, GDPR notes

## FAQ

**Is Postwerk a mail server?**
No. It only relays outgoing mail from your applications to targets you configure. It does not receive mail for
mailboxes, offer IMAP, act as an MX or deliver directly to recipients' servers.

**Does Postwerk replace Mailjet, Amazon SES or Postmark?**
No. It sits in front of such a provider. The provider still delivers the mail to the recipient, signs DKIM and keeps
its sending reputation. Postwerk gives your applications one queue and one audit log in front of it.

**Does Postwerk store email content?**
Only as long as it needs it to deliver. The raw message is encrypted at rest and removed once every target has it, or
a short time after a failure. Events and logs never contain message content, and addresses are removed from reply
texts before they are stored. Recipient addresses in the mail records are replaced by a keyed hash after a
configurable period.

**What happens if the provider is down or the credentials are wrong?**
Mails stay in the queue and are retried with backoff (from 30 seconds up to hourly) for 24 hours by default. A rejected
login (`535`) raises an error event at once and keeps the mails queued, so rotated credentials do not turn into failed
mails. Other targets are not affected.

**Can I use Postwerk in automated tests?**
Yes. Add Mailpit as a target and read the mails from Mailpit's API in your tests. A per-target recipient allow-list
(`ALLOW_RECIPIENTS`) keeps a test setup with a real provider from mailing real people by accident.

**Is Postwerk GDPR-friendly?**
It is built to support data minimisation and storage limitation (GDPR Art. 5(1)(c) and (e)): no content in events or
logs, raw messages encrypted and removed early, addresses redacted from reply texts and pseudonymized after a period,
configurable retention. It does not make anyone compliant. The organisation that runs it is responsible for its
processing. See [docs/privacy-and-security.md](docs/privacy-and-security.md).

**Does Postwerk need root or special Kubernetes permissions?**
No. The image is built from `scratch`, runs as uid 65532, works with a read-only root file system and
`capabilities.drop: [ALL]`, listens on ports above 1024 and needs no service account token. Run one replica with the
`Recreate` strategy, since one SQLite file has one writer.

## License

MIT. The software is provided as is, without warranty of any kind. See [LICENSE](LICENSE).
