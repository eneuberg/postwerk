# Internal web UI

A small web UI for people who run Postwerk: what is queued, what failed, and what happened to one mail. Mail records
are read-only in it; the only things it changes are its own accounts. It never shows message content. This page
describes how the UI is reached, how it is protected and what each page shows. Requirements: U1–U24 in
[requirements.md](requirements.md#ui-and-admin-listener); reasons: decisions D29, D35, D36 and D37 in
[decisions.md](decisions.md).

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
| Managed | in the file; read once at start | on the **Accounts** pages, by any logged-in account |
| Meant for | the first login and break-glass access | everyday use |

The tokens file is optional once the database has an account. A line with role `read` (from an older file) is a
configuration error. If a name is in both, the tokens file wins at login and a warning is logged at start. Without a
tokens file and without an account in the database, Postwerk does not start (U23).

**First start, step by step:**

1. Make a token: `postwerk gen-token ops` prints the token, then the line for the tokens file. Put the line in the
   file (or the Kubernetes Secret), keep the token.
2. Start Postwerk, open the UI and log in with the name `ops` and the token.
3. Under **Accounts**, create one account per person with a password; you confirm each with the token. Give each
   person their password through a safe channel; they change it on their account page.
4. Log in with your own account. Then remove the `ops` line, or the whole file (in Helm: `secrets.uiTokens.name=""`),
   and restart; or keep it, stored safely, as break-glass access.

If every password is lost, add a token line again and restart.

**Changing accounts.** On the **Accounts** page: create an account (name, password, the password again). On an
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
  counted the same way. A login that has to wait gets `429` without a check. The same limits apply to the password
  asked for a change. This allows about 100 wrong guesses a day per name and never locks an account for good, so
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
  `Content-Security-Policy: default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`,
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

On stdout and in every sink these events carry `"actor"` and `"text"` instead of an `"smtp"` object. A login, a
search, an opened mail or an account change that cannot be written to the event log is refused, so nothing happens in
the UI that the audit log misses. Searches are limited to 30 per minute per account; more get `429`. The timeline on a
mail's page shows the relay's events only; who opened the mail is in the event log.

## 5. Pages

All pages are server-rendered HTML with one stylesheet, readable at phone width, light and dark by the system setting.
A status is always written out as text, colour only supports it.

| Page | Path | Shows |
|---|---|---|
| Login | `GET /login`, `POST /login` | name and password or token |
| Overview | `GET /` | per target: recipients of the last 24 hours by status, mails queued now and the age of the oldest queued; the most recent problems (mails that are `failed`, `unknown` or `partial`) |
| Mails | `GET /mails` | list with filters (state, target, received from and to), newest first, 50 per page |
| Search | `GET /search`, `POST /search` | find mails by mail id, `X-Postwerk-Ref`, `Message-ID` or recipient address; the query goes in the request body only and the results come back in the same response, so it never lands in a URL, the browser history or an access log |
| Mail | `GET /mails/{id}` | id, received time, sending application, size, ref, Message-ID, envelope sender and parameters (`BODY`, `SMTPUTF8`, `SIZE`), whether the message is still stored; per target the derived status (including `partial`), attempts, next attempt, last reply, and a table with each recipient's status and reply; the event timeline. Opening it writes `ui.view` |
| Accounts | `GET /accounts`, `POST /accounts` | every account with where it is kept, when and by whom it was created and last changed; the form for a new account |
| Account | `GET /accounts/{name}`, `POST /accounts/{name}/password`, `POST /accounts/{name}/delete` | set a new password (your own: change it), delete the account |
| Logout | `POST /logout` | |

The state filter and every status shown are the derived summaries of architecture.md (`queued`, `sending`,
`unknown`, `handed_off`, `failed`, `partial`, `skipped`); with a target filter they are the state at that target.

**Addresses** are shown in full: recipients and the envelope sender. After pseudonymization nobody sees them; the page
says "pseudonymized". A search by address still finds the mail through the keyed hash (K3).

Recipients are numbered by their position in the envelope, from 0, as in the events.

**Never shown:** message content, subject, headers other than `Message-ID`, and anything from the stored message.

Mail records are read-only for now. Resend and close (D13) will come as `POST` forms.

## 6. Code

| Package | Holds |
|---|---|
| `internal/store` | the SQL for every read (lists, counts, events of a mail) and for the accounts table; still the only package that speaks SQL |
| `internal/query` | read logic on top of the store: overview, list, search, mail detail, derived statuses, address masking (`query.Mask`, used by the API); the JSON API (H2, [api.md](api.md)) uses the same functions |
| `internal/ui` | HTTP handlers, sessions, login and throttling, account pages, CSRF, Host check, headers; templates and the stylesheet embedded with `embed` and rendered with `html/template` |
| `internal/auth` | token file parsing, password rules and the equal-cost checker, on top of the sender password hashes |

For design work, `UI_DEMO_ADDR=127.0.0.1:18081 go test ./internal/ui -run TestDemo -timeout 30m` serves made-up data,
logged in as `ops` (tokens file) on that port and as `alice` (database) on the next one.

## 7. Not built yet

Resend and close, TLS on the admin listener itself, a check of new passwords against a list of breached passwords,
and sessions and login limits that survive a restart.
