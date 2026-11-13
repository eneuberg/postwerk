# Postwerk

**Status: design phase. There is no code yet.** This repository holds the design; the implementation follows in small,
tested steps.

Postwerk is a small, self-contained mail relay. Your applications hand it finished mails over SMTP; Postwerk stores each
one, forwards it to a sending provider (for example Mailjet), keeps an audit trail of every step, reports what goes wrong,
and answers the question *"did mail X get through?"*.

```
app ──SMTP──▶ Postwerk ──SMTP──▶ provider (e.g. Mailjet) ──▶ recipient
                 │  ▲
                 │  └── status lookups at the provider (optional)
                 ├──▶ events: stdout · OpenTelemetry · Sentry protocol · webhook
                 └──▶ status API and internal UI
```

## Why

Sending straight from every application to a provider spreads mail handling over many places, and each one has to
deal with timeouts, retries and error reporting on its own. Full mail servers (Postfix, maddy, …) solve the transport
side well, but they do not give you a per-mail audit trail, a status lookup, or error reports in your existing tools,
and some of them silently drop mail in edge cases (see [docs/design.md](docs/design.md#retry-policy)).

Postwerk aims to be the one boring place every mail passes through:

- **Store first, then say "accepted".** A `250` reply means the mail is on disk.
- **Content-blind.** Postwerk forwards the mail as it came. It never renders templates or builds links.
- **Retry what is temporary, keep what is our fault.** A rejected login (`535`) holds the queue and raises an error
  instead of dropping mail.
- **Never send twice on purpose.** If the outcome of an attempt is unknown, Postwerk marks it and asks a human.
- **Everything is an event.** One event shape, written to an append-only log, exported to the sinks you configure.
- **Few moving parts.** One static binary, one SQLite file, no external services required.

## Planned components

| Part | Does |
|---|---|
| Ingress | SMTP server with per-sender login; replies `250 queued as <id>` once the mail is stored |
| Store | SQLite: mails with status, append-only event log |
| Delivery | forwards to the upstream SMTP provider, retries with backoff, records every reply |
| Provider status | optional adapter that asks the provider what happened after hand-off (Mailjet first) |
| Reporter | reads the event log and exports to stdout, OpenTelemetry, Sentry protocol, webhook |
| Query API | read-only HTTP API: status and history of a mail, search, stats, health |
| UI | small read-only web UI on an internal-only listener |
| Mirror | optional copy of every mail to a capture tool such as Mailpit, off by default |

Non-goals: templates, link building, newsletters, inbound mail for users, alert rules. Postwerk reports facts; another
system decides what becomes an alert.

## Documentation

- [docs/design.md](docs/design.md): components, mail lifecycle, retry policy, event model, API, configuration
- [docs/decisions.md](docs/decisions.md): design decisions and their reasons

## License

[MIT](LICENSE)
