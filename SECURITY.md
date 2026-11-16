# Security policy

## Reporting a vulnerability

Please report security problems privately, through GitHub's private vulnerability reporting: on the repository's
**Security** tab, choose **Report a vulnerability**. Do not open a public issue or pull request for it.

A good report says which version or commit you tested, how Postwerk was configured (without real secrets), the steps
to reproduce, and what an attacker gains.

## Supported versions

Only the latest minor release of the latest major version gets security fixes. A fix comes as a new patch release of
that minor version. Before the first release, report against `main`.

| Version | Security fixes |
|---|---|
| latest `1.x` minor | yes |
| older minors, `0.x` and pre-releases | no; upgrade first |

## In scope

The code in this repository, the container image `ghcr.io/eneuberg/postwerk`, the Helm chart in `deploy/helm/postwerk`
and the release workflow that builds and signs the image. Examples of what counts:

- getting past the login on SMTP, the UI or the query API, or a `read` API token seeing more than
  [api.md](docs/api.md) allows;
- message content, recipient addresses or secrets ending up in events, logs, Sentry reports, the UI or the API,
  against what [privacy-and-security.md](docs/privacy-and-security.md) promises;
- weaknesses in encryption at rest, keyed hashing, retention or the append-only event log;
- a target connection that falls back to cleartext or skips certificate checks;
- cross-site attacks on the UI (CSRF, DNS rebinding, missing security headers);
- a release image that is not what the signature claims.

## Not in scope

- Setups that go against the documentation: the UI or API reachable from the internet, cleartext `AUTH` on an
  untrusted network, the example keys or tokens in use, a capture target in production.
- An authenticated sender that sends mail it is allowed to send. Senders are trusted applications; their logins and
  the limits in [configuration.md](docs/configuration.md#limits) are the boundary.
- Problems in services Postwerk talks to (your provider, Sentry or GlitchTip, Mailpit) or in Go modules it uses. Report
  those upstream. If a dependency's vulnerability affects Postwerk, a report here is welcome.

## What to expect

Postwerk is maintained by one person in their spare time. Handling is best effort, without a guaranteed response or
fix time. The aim is to acknowledge a report within about a week and to keep you informed while it is worked on.

Once a fix is released, the issue is published as a GitHub security advisory, with credit to the reporter if they
want it. Please give a reasonable time for a fix before you disclose the problem yourself.
