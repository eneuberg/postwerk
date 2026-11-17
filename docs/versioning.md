# Versioning and upgrades

Postwerk uses [semantic versioning](https://semver.org) from 1.0.0 on: `MAJOR.MINOR.PATCH`. This page says which
parts of Postwerk the version number makes promises about, how a database upgrade works, and how something is
deprecated. What changed in each release is in [CHANGELOG.md](../CHANGELOG.md).

Before 1.0.0, any release may break anything. Read the changelog.

## 1. What the version number means

- **Patch** (`1.4.1` → `1.4.2`): fixes only. No new settings, no changed behaviour beyond the fix.
- **Minor** (`1.4` → `1.5`): new features and additions that keep existing setups working. Upgrading needs no change
  to your configuration, secret files, senders, API clients or alert rules.
- **Major** (`1.x` → `2.0`): may remove or change anything covered below. The changelog says what to do.

Only the latest minor version gets fixes, security fixes included ([SECURITY.md](../SECURITY.md)).

## 2. What is covered

These are Postwerk's contract with you. Within one major version they do not change in a way that breaks a working
setup:

| Area | Covered | Allowed in a minor release |
|---|---|---|
| Environment variables | every `POSTWERK_*` name in [configuration.md](configuration.md), its meaning and its allowed values | new variables; new allowed values |
| Defaults | the default of every variable | a changed default, always listed under **Changed** in the changelog |
| Secret files | the formats of the senders file, key files, UI and API tokens files, target credentials, the DSN file and the TLS files | new formats next to the old ones; old lines keep working |
| Commands | `serve`, `hash-password`, `gen-sender`, `gen-key`, `gen-token`, `gen-api-token`, `version`, their arguments and output format | new commands and options |
| SMTP behaviour | ingress: `AUTH`, the offered extensions, reply codes and enhanced codes, the mail id in `250 2.0.0 queued as pw_…`, the `X-Postwerk-*` headers it reads and removes; towards targets: the headers it adds and the TLS rules | new extensions; stricter checks only where a mail could not be delivered anyway |
| Query API | everything under `/v1/` in [api.md](api.md): paths, methods, roles, request and response fields, status codes | new endpoints; new response fields (clients must ignore fields they do not know) |
| Events | every event kind and its level, the JSON fields of a stdout line, and the Sentry tags and grouping | new kinds and new fields (consumers must ignore what they do not know) |
| Helm chart | the values in `values.yaml`, their meaning and defaults, and the Secret names and keys it expects | new values |
| Health checks | `/healthz` and `/readyz` and their status codes | |

The Helm chart is released with Postwerk: at each release its `version` and `appVersion` are both the Postwerk
version.

## 3. What is not covered

These can change in any release:

- the UI: its HTML, pages, paths and wording. Do not scrape it; use the query API.
- log text: the messages of log lines that are not events, start-up and error messages.
- free text inside events: the reply text in `smtp.text`, the `text` of an event, and the Sentry message.
- the database schema: tables and columns are internal. Read data through the API, never with SQL.
- the Go packages under `internal/`: they cannot be imported, and are not an API.
- the minimum Go version for building from source, and the base images of the container.
- behaviour marked *planned* in the docs, and anything a doc calls not built yet.

## 4. Upgrades and the database

Postwerk keeps its schema version in the SQLite file. At start it applies every migration the database does not have
yet, in order, each in its own transaction, before it accepts mail. You can skip versions: a 1.2 database goes
straight to 1.7.

**Migrations only go forward.** There is no downgrade. An older Postwerk refuses to start on a database written by a
newer one. To go back, restore a backup taken before the upgrade, together with the older image.

**How to upgrade:**

1. Read the changelog entries between your version and the new one.
2. Back up the database: a volume snapshot, or stop Postwerk and copy the file
   ([kubernetes.md](kubernetes.md#8-backups)). Keep the key files; the backup is useless without them.
3. Verify the new image's signature ([kubernetes.md](kubernetes.md#image)) and pin its digest.
4. Replace the running instance. Only one process may use the SQLite file, so the old one stops before the new one
   starts: the `Recreate` strategy in Kubernetes (the chart's default), `docker compose up -d` with Compose. The SMTP
   port is closed for a few seconds; senders retry, and mails already accepted wait in the store.
5. Check the start log: `database migrated` names the schema version and the migrations applied (`database schema
   up to date` if there were none); no errors, no new warnings.

Mails that were queued before the upgrade are delivered by the new version.

## 5. Deprecation

Something covered in section 2 is removed or changed in a breaking way only in a major release, and only after it was
deprecated:

1. A minor release marks it deprecated: under **Deprecated** in the changelog, in the docs, and with a warning in the
   start log where Postwerk can see that you use it. It keeps working.
2. It stays deprecated for at least one more minor release.
3. The next major release removes it, listed under **Removed** with what to use instead.

A security fix can be the exception: if keeping something would leave users exposed, a minor or patch release may
change it. The changelog then says so under **Security**.
