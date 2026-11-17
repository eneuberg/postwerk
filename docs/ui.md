# Internal web UI

A small web UI for people who run Postwerk: what is queued, what failed, and what happened to one mail. It changes
its own accounts and, by an admin's decision, a delivery: close it or resend it (section 6). It never shows message
content. This page describes how the UI is reached, how it is protected and what each page shows. Requirements:
U1–U25 in [requirements.md](requirements.md#ui-and-admin-listener); reasons: decisions D29, D35, D36, D37 and D53–D55
in [decisions.md](decisions.md).

## 1. How it is reached

The UI runs on its own listener, the **admin listener**, separate from SMTP and from the health listener:

| Listener | Variable | Default | In containers | Who reaches it |
|---|---|---|---|---|
| SMTP | `POSTWERK_SMTP_LISTEN` | `:2525` | `:2525` | sending applications |
| Health | `POSTWERK_HTTP_LISTEN` | `127.0.0.1:8080` | `:8080` (probes come from outside the pod's loopback) | the kubelet, monitoring |
| Admin (UI) | `POSTWERK_ADMIN_LISTEN` | `127.0.0.1:8081` | `127.0.0.1:8081` in Kubernetes; see Docker below | people, through a tunnel |

The admin listener binds to the loopback interface, also inside a container. Nothing in front of Postwerk forwards to
it: no Service port, no Ingress, no NetworkPolicy rule. Probes never touch it, so the health port can stay open to
the kubelet without exposing the UI. `POSTWERK_ADMIN_LISTEN=off` (or empty) turns it off.

A person opens it through a tunnel that ends on the loopback interface of the machine or pod Postwerk runs in:

**Kubernetes.** `kubectl port-forward` connects to the pod's loopback, so a loopback-only listener is reachable:

```sh
kubectl -n mail port-forward deploy/postwerk 8081:8081
# then open http://localhost:8081
```

Who may do this is decided by Kubernetes RBAC: the `create` verb on `pods/portforward` in Postwerk's namespace. Grant
it only to the people who should see mail records. The tunnel runs inside the API server's TLS connection.

**A VM or bare host.** Postwerk listens on the host's `127.0.0.1:8081`; open an SSH tunnel:

```sh
ssh -L 8081:127.0.0.1:8081 relay.example.test
# then open http://localhost:8081
```

**Docker and Docker Compose.** Docker's port publishing forwards to the container's network interface, not to its
loopback, so a loopback-only listener inside a container cannot be published. The container listens on all of its
interfaces and the port is published on the host's loopback only:

```yaml
environment:
  POSTWERK_ADMIN_LISTEN: ":8081"
  POSTWERK_ADMIN_ALLOW_NON_LOOPBACK: "true"   # needed for Docker port publishing
ports:
  - "127.0.0.1:8081:8081"                     # never "8081:8081"
```

On a remote Docker host, add the SSH tunnel above. Other containers on the same Docker network can reach the port;
the login (below) protects it from them.

**Never expose the admin listener publicly**: no public address, no reverse proxy, no Ingress, no `LoadBalancer` or
`NodePort`. Postwerk refuses a non-loopback address unless `POSTWERK_ADMIN_ALLOW_NON_LOOPBACK=true` is set, and logs a
warning at every start when it is.

## 2. Accounts and login

The tunnel decides who reaches the port. A login is still required, because a forwarded port on `localhost` is open to
every process on the operator's laptop, and inside the pod to every other container.

**One kind of account.** Every account is an admin: it sees every page and every address in full, and may manage
accounts. Reaching the UI already needs the right to port-forward into Postwerk's pod or to log in to its host, so the
people who can use it run Postwerk anyway. Lookups for support staff belong in a tool on top of the JSON API, which has
its own roles (D35).

Accounts come from two places:

| | Tokens file | Database |
|---|---|---|
| Where | `POSTWERK_UI_TOKENS_FILE`, one `name:admin:<hash>` line each | table `ui_accounts` in the SQLite file |
| Secret | a random token from `postwerk gen-token <name>` | a password chosen in the UI |
| Managed | in the file; read once at start | under **Settings**, by any logged-in account |
| Meant for | the first login and break-glass access | everyday use |

The tokens file is optional once the database has an account. A line with role `read` (from an older file) is a
configuration error. If a name is in both, the tokens file wins at login and a warning is logged at start. Without a
tokens file and without an account in the database, Postwerk does not start (U23).

**First start, step by step:**

1. Make a token: `postwerk gen-token ops` prints the token, then the line for the tokens file. Put the line in the
   file (or the Kubernetes Secret), keep the token.
2. Start Postwerk, open the UI and log in with the name `ops` and the token.
3. Under **Settings**, create one account per person with a password; you confirm each with the token. Give each
   person their password through a safe channel; they change it under **Settings** (their own account page).
4. Log in with your own account. Then remove the `ops` line, or the whole file (in Helm: `secrets.uiTokens.name=""`),
   and restart; or keep it, stored safely, as break-glass access.

If every password is lost, add a token line again and restart.

**Changing accounts.** On the **Settings** page: create an account (name, password, the password again). On an
account's page: set a new password, or delete it. Every change asks for your own current password, is a `POST` with a
CSRF token, and is written to the event log in the same transaction. Setting a password or deleting an account ends
all of that account's sessions at once, yours included when it is your own account. The last account in the database
cannot be deleted. Accounts from the tokens file are shown as "tokens file" and cannot be changed in the UI.

**Passwords** (D36): 15 to 128 characters, counted as Unicode code points, any characters, no rules about digits or
symbols, not the account name. A passphrase of a few words works well. They are stored as PBKDF2-SHA256 hashes with
600,000 iterations, like sender passwords; never in plain text, never in a log or event.

**Login.** Name and password (or token), sent by `POST`. A failed login always shows the same message.

- **Equal cost.** Every check costs one PBKDF2 at the same iteration count: the tokens file's strongest hash or
  600,000, whichever is higher. Weaker hashes pay the difference, unknown names are checked against a dummy hash. So
  the time of a failed login says nothing about whether a name exists or where it is kept. At most one check per CPU
  runs at once; a login that waits more than 5 s for a slot gets a "try again" page.
- **Throttling** (U22), in memory. After 5 failed checks for one name, every further failure makes that name wait 1
  minute, then 2, 4, 8 and at most 15 minutes; a name without a failure for 24 hours, or with a good login, starts over.
  From one source address, 20 failures are free and the count is forgotten after an hour. Names that do not exist are
  counted the same way. A login that has to wait gets `429` without a check. Checks that are still running count
  against the free failures, so requests sent at once get no more checks than requests sent one after another; past
  the free failures, a name or source gets one check at a time (a second one gets the "try again" page). The same
  limits apply to the password asked for a change. This allows about 100 wrong guesses a day per name and never locks an account for good, so
  nobody can lock out an admin. Behind `kubectl port-forward` every person comes from `127.0.0.1`, which is why the
  source limit is the looser one.
- **Sessions** live in memory on the server: a random 32-byte id, an idle timeout of 30 minutes and an absolute limit
  of 8 hours. A new id is made at every login. Logout ends the session. A restart ends every session.
- **Cookie** `postwerk_session` with `HttpOnly; SameSite=Strict; Path=/`, plus `Secure` when the listener is served
  over TLS (browsers treat `http://localhost` as a secure context, so a tunnel needs no TLS of its own).

## 3. Browser attacks against localhost

A page on the internet can make the operator's browser send requests to `localhost`. Three measures stop that:

- **Host check (DNS rebinding).** A request is answered only if its `Host` header is `localhost`, `127.0.0.1` or
  `[::1]` (any port, so `kubectl port-forward 9000:8081` works too) or a name in `POSTWERK_ADMIN_HOSTS`. Everything
  else gets `403`. A rebinding attack points the attacker's own name at `127.0.0.1`; the browser then sends that name,
  which is not on the list.
- **CSRF tokens.** Every `POST` form carries a token: tied to the session after login, and to a short-lived cookie on
  the login form itself. A missing or wrong token gets `403`. Go's cross-origin protection (`Sec-Fetch-Site` and
  `Origin`) refuses cross-site `POST`s as well.
- **Strict headers on every response:**
  `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`,
  `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`,
  `X-Frame-Options: DENY`. The UI has no JavaScript at all.

## 4. Audit of the UI itself

Every login, failed check, search, opened mail and account change is an event in the event log (exported like every
other event):

| Kind | Level | Carries |
|---|---|---|
| `ui.login` | info | `actor`: the account, `text`: `file` or `database` |
| `ui.login_failed` | warn | `actor`: the name only if it is an account (a password pasted into the name field must not end up in the log); `text`: empty for a login, `confirm` for the password asked for a change |
| `ui.login_throttled` | warn | `actor` as above; `text`: `account` or `source`, which limit started a wait |
| `ui.search` | info | `actor`: the account, `text`: what was searched by (`id`, `ref`, `message_id`, `address`, `any`), never the query |
| `ui.view` | info | `actor`: the account, `mail_id`: the mail whose page was opened |
| `ui.account_created` | info | `actor`: who created it, `text`: the new account's name |
| `ui.password_set` | info | `actor`: who set it, `text`: the account's name |
| `ui.account_deleted` | info | `actor`: who deleted it, `text`: the account's name |
| `delivery.closed` | info | `actor`: who closed it, `mail_id`, `target`, the recipient's position; `text`: `resolution=… was=… via=ui` and the note, redacted. One per recipient |
| `delivery.resend` | info | `actor`: who resent it, `mail_id`, `target`, position; `text`: `was=… via=ui`. One per recipient |

On stdout and in every sink these events carry `"actor"` and `"text"` instead of an `"smtp"` object. A login, a
search, an opened mail or an account change that cannot be written to the event log is refused, so nothing happens in
the UI that the audit log misses. Searches are limited to 30 per minute per account; more get `429`. The timeline on a
mail's page shows the relay's events and an admin's close and resend (with the account's name); who opened the mail is
in the event log. A close or resend writes its events in the transaction that makes the change.

## 5. Pages

All pages are server-rendered HTML with one stylesheet and the embedded Barlow typeface, readable at phone width,
light and dark by the system setting. A status is always written out as text, next to a lamp whose colour and shape
both name it. How the UI looks, and why: [design-system.md](design-system.md).

| Page | Path | Shows |
|---|---|---|
| Login | `GET /login`, `POST /login` | name and password or token |
| Overview | `GET /` | a summary first: targets up or down, the queue and its oldest mail, failed and unknown recipients of the last 24 hours; then per target: up, or down since when and why (`auth`, `unreachable`; D45), whether it is optional, recipients of the last 24 hours by status, mails queued now and the age of the oldest queued; the most recent problems (mails that are `failed`, `unknown` or `partial` by their required targets) |
| Mails | `GET /mails` | the search form; list with filters (state, target, received from and to), newest first, 50 per page |
| Search | `POST /mails` | the search form at the top of the Mails page: find mails by mail id, `X-Postwerk-Ref`, `Message-ID` or recipient address. The query goes in the request body only and the results come back in the same response, on the Mails page in place of the list, so it never lands in a URL, the browser history or an access log. The page says "Results for your search" and never shows the query again; a link leads back to the list. `GET /search` (the old search page) redirects to `/mails` |
| Mail | `GET /mails/{id}` | on top the id, the overall status, received time, sending application, number of recipients and size, and a route to each target with its status; per target the derived status (including `partial`), attempts, next attempt, last reply, a table with each recipient's status, resolution and reply, and the close and resend forms (section 6); the event timeline, the day shown once; beside it (below on narrow screens) the record: ref, Message-ID, envelope sender and parameters (`BODY`, `SMTPUTF8`, `SIZE`), whether the message is still stored, the recipients. Opening it writes `ui.view` |
| Close, resend | `POST /mails/{id}/targets/{target}/close`, `POST /mails/{id}/targets/{target}/resend` | on success back to the mail page with a notice; when refused, the mail page with the reason at that target |
| Statistics | `GET /stats` | throughput and outcomes over a range (section 5.1) |
| Settings | `GET /settings`, `POST /settings/accounts` | your own account, with a link to change your password; every account with where it is kept, when and by whom it was created and last changed; the form for a new account |
| Account | `GET /settings/accounts/{name}`, `POST /settings/accounts/{name}/password`, `POST /settings/accounts/{name}/delete` | set a new password (your own: change it), delete the account; your own is linked from your name in the header |
| Logout | `POST /logout` | |

The navigation is Overview, Mails, Statistics, Settings. The old account addresses `GET /accounts` and `GET /accounts/{name}`
redirect to their pages under `/settings`; the query of a redirected request is dropped.

The state filter and every status shown are the derived summaries of architecture.md (`queued`, `sending`,
`unknown`, `handed_off`, `failed`, `partial`, `skipped`); with a target filter they are the state at that target.

### 5.1 Statistics

`GET /stats` answers "how much went through, and how much of it arrived?" (D73–D75). It has its own place in the
navigation, so it is one click from every page; the overview stays a picture of now.

- **Range and filters** are one `GET` form, so a view can be bookmarked and needs no JavaScript: a preset (last
  hour, 6 hours, 24 hours, 7 days, 30 days; 24 hours without a choice) or "From and to below" with a day
  (`2026-11-17`) or a day and time (`2026-11-17 08:00`) in UTC; a day alone as "to" includes that day. After a preset
  the fields show the range counted, ready to change. A target and a sending application narrow it. A range that
  starts before what retention keeps (the shorter of `POSTWERK_RECORD_RETENTION` and `POSTWERK_EVENT_RETENTION`) is
  cut there, and the page says so.
- **Buckets** follow the range: 1 minute for an hour, 5 minutes for 6 hours, 30 minutes for 24 hours, 3 hours for
  7 days, a day for 30 days; a custom range gets the narrowest width with at most 72 bars, a day from two weeks, a
  week from 200 days. All times are UTC and say so.
- **Summary**: mails received (in flight, skipped), handed off (partial), failed (unknown, rejected recipients; the
  number links to the failed mails of those days), the success rate, and the latency p95 with p50.
- **Throughput**: stacked bars of the mails received per bucket by their state now (handed off, partial, failed,
  unknown, in flight, skipped), drawn on the server as inline SVG and filled by class from the design tokens, so
  they follow light and dark. A count above zero is at least 2 units high, so one failure among thousands shows.
  Both axes take their ticks from one scale; the time labels are spaced by their width and never overlap. A tinted
  band marks the time a target was down. Each bar has a tooltip (`<title>`), the chart a `<title>` and `<desc>`, and
  "The chart as a table" below it lists every bar. On phones a narrower drawing with fewer labels replaces it.
- **By target**: deliveries by their state at that target (optional targets too), rejected recipients, retries,
  success rate, p50 and p95, and the time it was down. **By sending application**: mails by their state, and failed
  logins, refused commands and rate-limit refusals; failed logins with a name no sender has are one row,
  "unknown name".

What counts how: a mail in the bar of its received time with the state it has now, so the last bars change while
mails are delivered. Retries, rejected recipients, recipients outside an allow-list, ingress refusals and down time
count when they happened. The success rate is handed off divided by handed off, partial, failed and unknown; skipped
and in-flight mails are left out, an unknown one counts against it until someone closes it. Latency runs from
acceptance to the target's `250` for the last recipient of a delivery the relay handed off; deliveries an admin
closed are left out. Counts only: the page shows no address and no content, and opening it writes no event.

**Addresses** are shown in full: recipients and the envelope sender. After pseudonymization nobody sees them; the page
says "pseudonymized". A search by address still finds the mail through the keyed hash (K3).

Recipients are numbered by their position in the envelope, from 0, as in the events.

**Never shown:** message content, subject, headers other than `Message-ID`, and anything from the stored message.

## 6. Close and resend

When Postwerk cannot finish a delivery alone, an admin decides on the mail's page (D53–D55). Each target with
something to resolve has a **Resolve** part under its recipients, with two forms that open on a click (no JavaScript):

- **Resend…** queues the recipients that are `failed` or `unknown` at this target again and sends at once. Only
  while the message is still stored: Postwerk keeps it 72 h after a failure and 7 days at most, and never longer for
  a resend. If a recipient is `unknown`, the connection broke after the message was sent, so the target may have it;
  the form says so and asks for a tick at "Resend anyway; it may arrive twice". Check at the target first: if it has
  the mail, close the delivery as confirmed instead.
- **Close…** ends the delivery for its open recipients (`queued`, `unknown` or `failed`): nothing sends them any
  more, and retention no longer turns an `unknown` one into `failed` when the message expires. Choose a resolution:
  **confirmed** (you checked at the target, it has the mail; shown as handed off with "closed: confirmed") or
  **given up** (shown as failed with "closed: given up"). An optional note of at most 200 characters, plain text on one
  line, goes into the event; addresses in it are replaced by `<redacted>`. Confirmed is not offered while a recipient
  is queued. The stored message is removed at once when no other delivery of the mail needs it. A close cannot be
  undone.

Both forms ask for your own current password, like the account changes, and carry the CSRF token; a wrong password
counts against the login throttle. They act on every open recipient of that target at once, never on a single one.
When an action is not possible, its button is shown disabled with the reason: the message was removed, the delivery is
being sent right now (wait a moment), or the target is no longer configured (only close then). A target whose
recipients are all final shows no actions. Every close and resend is an event with your name (section 4).

The typical case: a connection broke after `DATA`, the delivery is `unknown` (policy `hold`). Look the mail up at the
provider by its `X-Postwerk-Id` (Mailjet: the custom id). Found: close as confirmed. Not found: resend.

## 7. Code

| Package | Holds |
|---|---|
| `internal/store` | the SQL for every read (lists, counts, events of a mail) and for the accounts table; still the only package that speaks SQL |
| `internal/resolve` | close and resend for the UI and the API: input checks, what is possible for a delivery, the store's change and the wake signal for the workers |
| `internal/query` | read logic on top of the store: overview, list, search, mail detail, statistics, derived statuses, address masking (`query.Mask`, used by the API); the JSON API (H2, [api.md](api.md)) uses the same functions |
| `internal/ui` | HTTP handlers, sessions, login and throttling, settings and account pages, CSRF, Host check, headers; templates, the stylesheet, the logo and the typeface (with its licence, `static/OFL.txt`) embedded with `embed`, served from a fixed list and rendered with `html/template` |
| `internal/auth` | token file parsing, password rules and the equal-cost checker, on top of the sender password hashes |

For design work, `UI_DEMO_ADDR=127.0.0.1:18081 go test ./internal/ui -run TestDemo -timeout 30m` serves made-up data,
logged in as `ops` (tokens file) on that port and as `alice` (database) on the next one.

## 8. Not built yet

Closing or resending a single recipient of a delivery, TLS on the admin listener itself, a check of new passwords against a list of breached passwords,
and sessions and login limits that survive a restart.
