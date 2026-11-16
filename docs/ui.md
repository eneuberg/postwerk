# Internal web UI

A small, read-only web UI for people who run Postwerk: what is queued, what failed, and what happened to one mail.
It never shows message content. This page describes how the UI is reached, how it is protected and what each page
shows. Requirements: U1–U16 in [requirements.md](requirements.md#ui-and-admin-listener); reasons: decision D29 in
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

## 2. Login

The tunnel decides who reaches the port. A login is still required, because a forwarded port on `localhost` is open to
every process on the operator's laptop, and inside the pod to every other container.

- **Tokens file.** `POSTWERK_UI_TOKENS_FILE` is required while the admin listener is on. One line per person or team:
  `name:role:pbkdf2-sha256$<iterations>$<salt>$<key>`, role `read` or `admin`. Names as for senders,
  `[a-z0-9][a-z0-9._-]{0,63}`. The file holds hashes only. It is read once at start.
- **Making a token.** `postwerk gen-token <name> <read|admin>` prints a random token (32 bytes, base64url) on the
  first line and the matching line for the tokens file on the second. Give the token to the person; it is not stored.
- **Login form.** Name and token, sent by `POST`. A failed login always shows the same message.
- **Equal cost and limited checks**, as for SMTP (I12, I13): every login costs one PBKDF2 check with the strongest
  iteration count in the file, for unknown names too; at most one check per CPU runs at once, and a login that waits
  more than 5 s for a slot gets a "try again" page.
- **Sessions** live in memory on the server: a random 32-byte id, an idle timeout of 30 minutes and an absolute limit
  of 8 hours. A new id is made at every login. Logout ends the session. A restart ends every session.
- **Cookie** `postwerk_session` with `HttpOnly; SameSite=Strict; Path=/`, plus `Secure` when the listener is served
  over TLS (browsers treat `http://localhost` as a secure context, so a tunnel needs no TLS of its own).
- **Roles.** `read` sees addresses masked, `admin` sees them in full. Both see every page. Actions such as resend and
  close will need `admin` (later).

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

Every login, failed login and search is an event in the event log (exported like every other event):

| Kind | Level | Carries |
|---|---|---|
| `ui.login` | info | `actor`: the token name, `text`: the role |
| `ui.login_failed` | warn | `actor`: the name only if it is in the tokens file (a mistyped token pasted into the name field must not end up in the log) |
| `ui.search` | info | `actor`: the token name, `text`: what was searched by (`id`, `ref`, `message_id`, `address`, `any`), never the query |

On stdout and in every sink these events carry `"actor"` and `"text"` instead of an `"smtp"` object. A login or a search
that cannot be written to the event log is refused, so nothing happens in the UI that the audit log misses. Searches
are limited to 30 per minute per token name; more get `429`.

## 5. Pages

All pages are server-rendered HTML with one stylesheet, readable at phone width, light and dark by the system setting.
A status is always written out as text, colour only supports it.

| Page | Path | Shows |
|---|---|---|
| Login | `GET /login`, `POST /login` | name and token |
| Overview | `GET /` | per target: recipients of the last 24 hours by status, mails queued now and the age of the oldest queued; the most recent problems (mails that are `failed`, `unknown` or `partial`) |
| Mails | `GET /mails` | list with filters (state, target, received from and to), newest first, 50 per page |
| Search | `GET /search`, `POST /search` | find mails by mail id, `X-Postwerk-Ref`, `Message-ID` or recipient address; the query goes in the request body only and the results come back in the same response, so it never lands in a URL, the browser history or an access log |
| Mail | `GET /mails/{id}` | id, received time, sending application, size, ref, Message-ID, envelope sender and parameters (`BODY`, `SMTPUTF8`, `SIZE`), whether the message is still stored; per target the derived status (including `partial`), attempts, next attempt, last reply, and a table with each recipient's status and reply; the event timeline |
| Logout | `POST /logout` | |

The state filter and every status shown are the derived summaries of architecture.md (`queued`, `sending`,
`unknown`, `handed_off`, `failed`, `partial`, `skipped`); with a target filter they are the state at that target.

**Addresses.** `admin` sees recipients and the envelope sender in full. `read` sees them masked: first character of the
local part, first character of the domain, the top-level domain (`jane@example.com` → `j•••@e•••.com`). After
pseudonymization nobody sees them; the page says "pseudonymized". A search by address still finds the mail through the
keyed hash (K3).

Recipients are numbered by their position in the envelope, from 0, as in the events.

**Never shown:** message content, subject, headers other than `Message-ID`, and anything from the stored message.

The UI is read-only for now. Resend and close (D13) will come as `POST` forms for `admin` only.

## 6. Code

| Package | Holds |
|---|---|
| `internal/store` | the SQL for every read (lists, counts, events of a mail); still the only package that speaks SQL |
| `internal/query` | read logic on top of the store: overview, list, search, mail detail, derived statuses, address masking by role; the JSON API (H2, [api.md](api.md)) uses the same functions |
| `internal/ui` | HTTP handlers, sessions, login, CSRF, Host check, headers; templates and the stylesheet embedded with `embed` and rendered with `html/template` |
| `internal/auth` | token file parsing on top of the sender password hashes |

For design work, `UI_DEMO_ADDR=127.0.0.1:18081 go test ./internal/ui -run TestDemo -timeout 30m` serves made-up data,
logged in as `admin` on that port and as `read` on the next one.

## 7. Not built yet

Resend and close (admin), TLS on the admin listener
itself, and sessions that survive a restart.
