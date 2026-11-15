<img src="docs/logo.svg" width="72" height="72" alt="Postwerk logo">

# Postwerk

A thin, audited relay for outgoing mail inside a server or cluster.

**Status: design draft. Nothing is implemented yet.** This repository holds the design. The code will follow in small,
tested steps.

![Applications hand a mail to Postwerk over SMTP; Postwerk stores it, logs it, and delivers one copy to each target](docs/flow.svg)

## Purpose

Postwerk is meant to be the one central point for outgoing mail in a server or cluster. Every application hands its
finished mails to Postwerk over SMTP. Postwerk stores each mail on disk before it accepts it, delivers a copy to each
configured target, and records every step in an append-only log. From that one place, every mail can be audited and
tracked: accepted, handed off, delivered, bounced, failed. A small read-only web UI and a JSON query API answer the
question "did mail X get through?".

It is deliberately thin: one static Go binary and one SQLite file. It runs as non-root, needs no special cluster
permissions and no inbound port from the internet; provider status is fetched by polling. It has far fewer moving parts
than a full mail server.

Two typical uses:

- Production: one audited path from all applications to a sending provider such as Mailjet.
- Testing: add Mailpit as a second target and know exactly which mails were sent and that each one arrived there.

## What it does

- Accepts finished mails over SMTP, with a login per sending application.
- Stores each mail on disk before it replies `250`.
- Delivers a copy to each configured target. Each target has its own queue state, retries and status.
- Records every step in an append-only event log.
- Fetches delivery status from the provider by polling (Mailjet first).
- Answers status queries through a JSON API and a small internal UI.
- Reports events to stdout, OpenTelemetry, Sentry-protocol tools such as GlitchTip, and signed webhooks.

## What it does not do

- Receive mail for mailboxes. There is no IMAP.
- Sign (DKIM) or rewrite messages. Mails are forwarded as received, apart from tracking headers.
- Render templates or build links.
- Decide what counts as an alert. It reports events; alerting is the job of another system.

Does Postwerk fit your setup? See [docs/scope.md](docs/scope.md).

## What `250` means

A `250` reply means Postwerk has stored the mail and is now responsible for it. It does not mean the mail was
delivered. Use the status API for that.

Stored is not indestructible: if the data volume is lost before a mail is handed off, that mail is lost.

## Targets

Every accepted mail is copied to each configured target, for example `mailjet` and `capture`. A slow or broken target
does not block the others. A target can be marked optional; its failures are then reported as warnings.

A sender can keep a mail away from named targets with a header, for example to keep mails that contain credentials out
of a capture tool:

```
X-Postwerk-Skip: capture
```

## Architecture

![Postwerk architecture](docs/architecture.svg)

- [docs/design.md](docs/design.md): components, mail lifecycle, retry policy, event model, API, configuration
- [docs/decisions.md](docs/decisions.md): design decisions and their reasons
- [docs/scope.md](docs/scope.md): scope, comparison in depth, and whether Postwerk fits your setup

## Compared with maddy and Postfix

Postfix and maddy are mature, complete mail servers and do far more. Postwerk is a thin relay for applications that only
send. The Postwerk column describes the plan, not working software.

| | Postwerk (planned) | maddy | Postfix |
|---|---|---|---|
| What it is | relay for outgoing application mail | all-in-one mail server in Go (SMTP, submission, IMAP, DKIM, …) | full MTA |
| Moving parts | one binary, one SQLite file | one binary | several cooperating daemons (master, smtpd, qmgr, cleanup, pickup, …), file-based queue |
| Runs as | non-root | can run as non-root | master process starts as root and drops privileges |
| Configuration | environment variables, secrets files | own config language | `main.cf`, `master.cf`; extensive |
| Receives mail for mailboxes | no | yes | yes |
| Multiple targets | yes; one tracked delivery per target | several `deliver_to` per block (per its docs) | extra copies via `always_bcc` / `recipient_bcc_maps`; these are added recipients, not separately tracked deliveries |
| Upstream login rejected (`535`) | queue held, error raised at once, retried | treated as permanent, not retried (in our reading of the v0.9.7 source) | deferred and retried by default (`smtp_sasl_auth_soft_bounce = yes`) |
| Per-mail status and audit | append-only event log | logs | log files, `postqueue -p` |
| Query API / UI | JSON API, internal read-only UI | no | no |
| Delivery status from provider | polling (Mailjet first) | — | — |
| Error reporting | stdout, OpenTelemetry, Sentry protocol, signed webhooks | logs | log files |

When to use what:

- If you need a mail server (receiving mail, mailboxes, IMAP, DKIM signing), use Postfix or maddy.
- If your applications only send, through a provider, and you want every mail tracked and errors reported to your
  existing tools, that is the case Postwerk is designed for.

## Planned configuration

Environment variables, with credentials in files. Names may still change; [docs/design.md](docs/design.md) is the
reference.

```
POSTWERK_SMTP_LISTEN=:2525
POSTWERK_HTTP_LISTEN=127.0.0.1:8080        # API and UI, internal only
POSTWERK_DATA=/data/postwerk.db
POSTWERK_SENDERS_FILE=/run/secrets/senders
POSTWERK_RETRY_WINDOW=24h

POSTWERK_TARGETS=mailjet,capture
POSTWERK_TARGET_MAILJET_URL=smtp://in-v3.mailjet.com:587
POSTWERK_TARGET_MAILJET_CREDENTIALS_FILE=/run/secrets/mailjet
POSTWERK_TARGET_MAILJET_STATUS=mailjet     # fetch delivery status from Mailjet
POSTWERK_TARGET_CAPTURE_URL=smtp://mailpit:1025
POSTWERK_TARGET_CAPTURE_OPTIONAL=true      # failures are warnings

POSTWERK_OTEL_ENDPOINT=                    # empty: off
POSTWERK_SENTRY_DSN=                       # empty: off
POSTWERK_WEBHOOK_URL=                      # empty: off
```

## License

MIT. The software is provided as is, without warranty. See [LICENSE](LICENSE).
