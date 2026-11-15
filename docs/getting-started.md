# Getting started

Postwerk is in early development and not ready for production yet. This page runs it on a local machine with Docker
Compose and Mailpit, then shows the commands and settings for your own setup.

## Run the example with Docker Compose and Mailpit

The example in [examples/compose.yaml](../examples/compose.yaml) runs Postwerk with [Mailpit](https://mailpit.axllent.org)
as its only target. It is for a local machine only: passwords cross the network in cleartext, Mailpit keeps full message
content, and the key files next to it are public examples.

Run from the root of the repository:

```sh
docker build -t postwerk:dev .
docker compose -f examples/compose.yaml up
```

## Send a test mail

Send a test mail with `curl`. The example sender is `example-app` with the password `change-me-local-only`:

```sh
printf 'From: app@example.test\r\nTo: user@example.test\r\nSubject: Hello\r\n\r\nTest mail.\r\n' > mail.txt
curl smtp://localhost:2525 --user example-app:change-me-local-only \
  --mail-from app@example.test --mail-rcpt user@example.test --upload-file mail.txt
```

Open http://localhost:8025 to see the mail in Mailpit. The Postwerk log shows one JSON line per event
(`mail.accepted`, `delivery.attempt`, `delivery.handed_off`).

The reply `250 2.0.0 queued as pw_…` means Postwerk has stored the mail. It does not mean the mail was delivered. Each
target's result is recorded later as an event.

## Open the UI

Open http://localhost:8081 and log in with the name `ops` and the token `example-ui-token-local-only` (from
[examples/ui-tokens.example](../examples/ui-tokens.example), a public example: never use it elsewhere). The overview
shows each target with what is queued and what happened in the last 24 hours; **Mails** lists every mail with its
status per target; **Search** finds a mail by its id, `X-Postwerk-Ref`, `Message-ID` or a recipient address. The UI
never shows message content.

The example publishes the UI on your machine's loopback only (`127.0.0.1:8081`). On a server, keep it that way and
open it through an SSH tunnel (`ssh -L 8081:127.0.0.1:8081 your-server`); in Kubernetes, use `kubectl port-forward`.
Details: [ui.md](ui.md).

## Create senders and keys

```sh
# Hash a password for the senders file (one name:hash line per application).
printf '%s' 'your-password' | docker run --rm -i postwerk:dev hash-password

# Create a new key for a key file (encryption at rest, address hashing).
docker run --rm postwerk:dev gen-key

# Create a UI token: prints the token, then the line for the UI tokens file.
docker run --rm postwerk:dev gen-token alice admin
```

Use separate key files for the store key and the hash key. Never use the example keys outside a local machine.

## Mailpit as a target in development and testing

With Mailpit as a target, every mail Postwerk accepts lands there. You can open each one and check the template, the
links and the recipients. Automated tests can read the mails through Mailpit's API and assert on them. With a real
provider and Mailpit as two targets, a test run also shows exactly which mails were sent.

Use the cleartext scheme for a local Mailpit, and resend on an unclear outcome, since a duplicate does no harm there:

```sh
POSTWERK_TARGETS=mailpit
POSTWERK_TARGET_MAILPIT_URL=smtp+plain://mailpit:1025
POSTWERK_TARGET_MAILPIT_ON_UNKNOWN=retry
```

`smtp+plain://` must be chosen explicitly and cannot carry credentials. Plain `smtp://` requires STARTTLS, which a
default Mailpit does not offer.

In production, do not add a capture target by default. A capture tool stores every mail in plain text, including
password reset links and one-time codes. See the [FAQ](faq.md#can-i-use-mailpit-in-production) for a short-term
capture copy in production.

## Minimal production-style configuration

A provider as the only target:

```sh
POSTWERK_SENDERS_FILE=/run/secrets/senders
POSTWERK_SMTP_TLS_CERT_FILE=/run/secrets/tls.crt            # AUTH needs TLS on ingress
POSTWERK_SMTP_TLS_KEY_FILE=/run/secrets/tls.key
POSTWERK_TARGETS=mailjet
POSTWERK_TARGET_MAILJET_URL=smtp://in-v3.mailjet.com:587    # STARTTLS required
POSTWERK_TARGET_MAILJET_CREDENTIALS_FILE=/run/secrets/mailjet
POSTWERK_UI_TOKENS_FILE=/run/secrets/ui-tokens               # the UI on 127.0.0.1:8081, or POSTWERK_ADMIN_LISTEN=off
# plus the key files for encryption at rest and address hashing, see configuration.md
```

Every variable, its default, and the key files are described in [configuration.md](configuration.md). Unknown
`POSTWERK_*` variables stop the start, so a typo is never silently ignored.

## Next steps

- [Scope and fit](scope.md#6-before-you-integrate): what to check before you connect your applications
- [Internal UI](ui.md): how the UI is reached and protected, and what it shows
- [Privacy and security](privacy-and-security.md): what is stored, for how long, and who can read it
- [FAQ](faq.md): short answers to common questions
