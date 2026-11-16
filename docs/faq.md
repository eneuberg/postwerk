# Postwerk FAQ

Short answers about Postwerk, a self-hosted SMTP relay for outgoing transactional email. Postwerk is in early
development and not ready for production yet. Planned features are marked as planned.

## Is Postwerk a mail server?

No. It only relays outgoing mail from your applications to targets you configure. It does not receive mail for
mailboxes, offer IMAP, act as an MX or deliver directly to recipients' servers.

## Does Postwerk replace Mailjet, Amazon SES or Postmark?

No. It sits in front of such a provider. The provider still delivers the mail to the recipient, signs DKIM and keeps
its sending reputation. Postwerk gives your applications one queue and one audit log in front of it.

## What does a `250` reply from Postwerk mean?

`250 2.0.0 queued as pw_…` means Postwerk has stored the mail and is now responsible for it. It does not mean the mail
was delivered. Each target's result is recorded later as an event. If the data volume is lost before a mail is handed
off, that mail is lost.

## What happens if the provider is down or the credentials are wrong?

Mails stay in the queue for 24 hours by default. A rejected login (`535`) or a provider that cannot be reached takes
the target down: one `target.down` error event at once, no further mail is tried, and a probe (log in, no mail) runs
every 30 seconds, doubling up to 5 minutes, so the provider does not see a login per queued mail and lock the
account. When the probe works, `target.recovered` is written and the queue goes out. Rotated credentials do not turn
into failed mails. Other targets are not affected.

## Does Postwerk store email content?

Only as long as it needs it to deliver. The raw message is encrypted at rest and removed once every target has it, or
a short time after a failure. Events and logs never contain message content, and addresses are removed from reply
texts before they are stored. Recipient addresses in the mail records are replaced by a keyed hash after a
configurable period.

## Can I use Postwerk in automated tests?

Yes. Add [Mailpit](https://mailpit.axllent.org) as a target and read the mails from Mailpit's API in your tests. A
per-target recipient allow-list (`ALLOW_RECIPIENTS`) keeps a test setup with a real provider from mailing real people
by accident. See [Getting started](getting-started.md#mailpit-as-a-target-in-development-and-testing).

## Can I use Mailpit in production?

Not by default. A capture tool stores every mail in plain text, including password reset links and one-time codes.
Anyone who can open it can take over accounts. That is a privacy and a security problem. In production, Postwerk's own
records are meant to be enough to audit and trace mails without keeping their content: the event log, the UI and the
query API, and error reports to Sentry or GlitchTip.

If you need a capture copy in production for a short time, for example to debug one integration:

- mark the target `OPTIONAL`, so its failures are warnings, not errors;
- keep mails with credentials out of it with the header `X-Postwerk-Skip: <target name>` (a dedicated sensitive-mail
  flag that fails safe is an open suggestion, see [scope.md gap 6](scope.md#7-gaps-between-the-ideal-and-the-current-design));
- protect Mailpit's UI with `--ui-auth-file` and limit its retention with `--max-age` (for example `1d`);
- remove the target again when you are done.

## Is Postwerk GDPR-friendly?

It is built to support data minimisation and storage limitation (GDPR Art. 5(1)(c) and (e)): no content in events or
logs, raw messages encrypted and removed early, addresses redacted from reply texts and pseudonymized after a period,
configurable retention. It does not make anyone compliant. The organisation that runs it is responsible for its
processing. See [privacy-and-security.md](privacy-and-security.md).

## Does Postwerk need root or special Kubernetes permissions?

No. The image is built from `scratch`, runs as uid 65532, works with a read-only root file system and
`capabilities.drop: [ALL]`, listens on ports above 1024 and needs no service account token. Run one replica with the
`Recreate` strategy, since one SQLite file has one writer.

## How does Postwerk compare with Postfix or maddy?

Postfix and maddy are mature, complete mail servers. Postwerk is a thin relay for applications that only send. Use
Postfix or maddy if you need to receive mail, host mailboxes or sign DKIM yourself. The full comparison is in
[scope.md](scope.md#4-comparison).
