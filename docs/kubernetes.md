# Kubernetes

How to run Postwerk in a Kubernetes cluster with the Helm chart in [deploy/helm/postwerk](../deploy/helm/postwerk).
Every Postwerk setting is described in [configuration.md](configuration.md); this page covers what is specific to
Kubernetes.

## 1. What the chart sets up

| Resource | What and why |
|---|---|
| Deployment | **One replica, strategy `Recreate`.** Postwerk keeps its queue in one SQLite file with one writer; two pods on the same file would corrupt it. `Recreate` stops the old pod before the new one starts. |
| Service | **`ClusterIP` only**, port 2525 (SMTP). No Ingress, no `LoadBalancer`, no `NodePort`: Postwerk is never reachable from outside the cluster. Port 8080 (health) is added only with `service.exposeHTTP=true`. The UI port is never on the Service (section 7.1). |
| PersistentVolumeClaim | The SQLite file at `/data`. Kept on `helm uninstall` (`helm.sh/resource-policy: keep`), so the queue and the records survive. |
| NetworkPolicy | On by default: SMTP only from the pods you name, HTTP only from the sources you name, egress only DNS and the target ports 587 and 465 (section 7). |
| ServiceAccount | Without a token: Postwerk never talks to the Kubernetes API. |
| Certificate (optional) | A cert-manager certificate for STARTTLS from an internal CA (section 6). |

The pod runs as uid and gid 65532 (`runAsNonRoot`), with `fsGroup: 65532`, a read-only root file system, all
capabilities dropped, `allowPrivilegeEscalation: false`, the `RuntimeDefault` seccomp profile, no service account
token and no service links. Service links would add variables such as `POSTWERK_PORT` for a Service called
`postwerk`, and Postwerk refuses unknown `POSTWERK_*` variables, so they are off. Secrets are mounted as files with
mode `0440` (owner root, group 65532), so only Postwerk can read them and it logs no permission warning.

Probes: a startup probe and a liveness probe on `/healthz`, a readiness probe on `/readyz`, all on port 8080
(`POSTWERK_HTTP_LISTEN=:8080`). The internal UI runs on its own listener on the pod's loopback,
`127.0.0.1:8081` ([ui.md](ui.md)); probes never touch it, and nothing in the cluster can reach it. The grace period is 120 s, longer than Postwerk's own shutdown deadline of 100 s, so a
send in progress always finishes (configuration.md, section "Shutdown").

## 2. Before you install

- **An image.** The chart uses `ghcr.io/eneuberg/postwerk` with the chart's `appVersion` as tag. Verify its signature
  and pin it by digest (below). For your own build, `docker build -t registry.example.test/postwerk:0.1.0 .`, push it
  and set `image.repository`.
- **A namespace**, for example `mail`.
- **A storage class with local block storage** (`ReadWriteOnce`). SQLite needs real file locks and `fsync`; a network
  file system such as NFS or SMB is not supported. 1 GiB is plenty for typical transactional volumes.
- **A CNI that enforces NetworkPolicy** (Calico, Cilium, and most managed clusters do; plain flannel does not). Without
  it the policy is silently ignored.
- **Optional: cert-manager**, if the chart should issue the STARTTLS certificate.

### Image

Each release tag `v<version>` publishes `ghcr.io/eneuberg/postwerk:<version>` for `linux/amd64` and `linux/arm64`,
with an SBOM and a build provenance attached. There is no `latest` tag. The image is signed keyless with
[cosign](https://docs.sigstore.dev/cosign/) by the release workflow, so the signature names the workflow and the tag
it ran for. Check it with cosign 3 or newer before you use a version:

```sh
cosign verify ghcr.io/eneuberg/postwerk:0.1.0 \
  --certificate-identity https://github.com/eneuberg/postwerk/.github/workflows/release.yml@refs/tags/v0.1.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The command fails unless the image was signed by `release.yml` in this repository for exactly that tag. To accept any
release tag, use `--certificate-identity-regexp '^https://github\.com/eneuberg/postwerk/\.github/workflows/release\.yml@refs/tags/v'`
instead. The output names the digest the signature covers (`docker-manifest-digest`); put it into `image.digest`, so
the cluster pulls exactly the image you checked:

```sh
cosign verify … | jq -r '.[0].critical.image."docker-manifest-digest"'   # sha256:…
```

SBOM and provenance (in-toto, from BuildKit) are part of the signed image index:

```sh
docker buildx imagetools inspect ghcr.io/eneuberg/postwerk:0.1.0 --format '{{ json .SBOM }}'
docker buildx imagetools inspect ghcr.io/eneuberg/postwerk:0.1.0 --format '{{ json .Provenance }}'
```

To enforce signatures in the cluster, use an admission policy (for example Sigstore policy-controller or Kyverno) with
the same issuer and identity.

## 3. Create the secrets

The chart never creates secrets; it references existing ones by name. Make the values on your own machine with the
Postwerk image (it has no shell, so the commands run outside the cluster), then store them as Kubernetes Secrets.
Do not keep the files afterwards, and never commit them.

```sh
IMG=ghcr.io/eneuberg/postwerk:0.1.0     # verified as in section 2

# One line per sending application: name:hash. The password goes in on stdin.
printf '%s' "$APP_PASSWORD" | docker run --rm -i "$IMG" hash-password   # prints pbkdf2-sha256$600000$…
echo "auth-service:pbkdf2-sha256\$600000\$…" > senders

# Two different keys: one encrypts stored messages, one hashes addresses.
docker run --rm "$IMG" gen-key > store-key
docker run --rm "$IMG" gen-key > hash-key

# Target credentials: one line username:password (for Mailjet: API key and secret key).
printf '%s:%s\n' "$MJ_APIKEY_PUBLIC" "$MJ_APIKEY_PRIVATE" > mailjet-credentials

# UI tokens: one name:role:hash line per person or team (role read or admin).
# gen-token prints the token (first line: give it to the person) and the line (second line).
docker run --rm "$IMG" gen-token alice admin   # token, then alice:admin:pbkdf2-sha256$600000$…
echo 'alice:admin:pbkdf2-sha256$600000$…' > ui-tokens

kubectl -n mail create secret generic postwerk-senders --from-file=senders=senders
kubectl -n mail create secret generic postwerk-store-key --from-file=store-key=store-key
kubectl -n mail create secret generic postwerk-hash-key --from-file=hash-key=hash-key
kubectl -n mail create secret generic postwerk-target-mailjet --from-file=credentials=mailjet-credentials
kubectl -n mail create secret generic postwerk-ui-tokens --from-file=tokens=ui-tokens
shred -u senders store-key hash-key mailjet-credentials ui-tokens 2>/dev/null ||
  rm -f senders store-key hash-key mailjet-credentials ui-tokens
```

The secret names and keys above are the chart's defaults (`secrets.*`, `targets[].credentialsSecret`). Keep a copy of
both key files in your password manager or secret store: a backup of the database is useless without them, and a lost
store key makes every queued mail unreadable (it fails with `store.decrypt_failed`, it is never sent).

The STARTTLS certificate is a Secret of type `kubernetes.io/tls` named `postwerk-tls` (`tls.secretName`), either
created by cert-manager (section 6) or by you: `kubectl -n mail create secret tls postwerk-tls --cert=… --key=…`.

To add a sender or a UI token later, add a line to its file, update the Secret and restart Postwerk
(`kubectl -n mail rollout restart deploy/<release>-postwerk`): secret files are read once at start. Without the UI,
set `ui.enabled=false`; then the tokens Secret is not needed.

## 4. Install

A values file for production with Mailjet:

```yaml
# values-prod.yaml
image:
  digest: sha256:…            # recommended: the digest cosign verified (section 2)
instance: prod
tls:
  certManager:
    enabled: true             # or create postwerk-tls yourself
    issuerRef: {name: internal-ca, kind: ClusterIssuer, group: cert-manager.io}
targets:
  - name: mailjet
    url: smtp://in-v3.mailjet.com:587
    credentialsSecret: {name: postwerk-target-mailjet, key: credentials}
    status: mailjet
networkPolicy:
  smtpFrom:                   # who may send through Postwerk
    - podSelector:
        matchLabels:
          postwerk-client: "true"
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: shop
      podSelector:
        matchLabels:
          app: shop-backend
```

```sh
helm install postwerk deploy/helm/postwerk -n mail -f values-prod.yaml
kubectl -n mail rollout status deploy/postwerk
kubectl -n mail logs deploy/postwerk | head    # "postwerk started", no warnings
```

Check the start log: no warnings about file permissions, encryption or pseudonymization (configuration.md,
"Production checklist").

### Values

All values are in [values.yaml](../deploy/helm/postwerk/values.yaml) with comments. The important ones:

| Value | Default | Meaning |
|---|---|---|
| `image.repository`, `image.tag`, `image.digest` | `ghcr.io/eneuberg/postwerk`, empty (the chart's `appVersion`), empty | The image; a digest wins over the tag (**recommended**: the digest you verified). |
| `hostname` | empty (Postwerk's default `postwerk`) | `POSTWERK_HOSTNAME`: name in the greeting and in `EHLO` towards targets. |
| `instance` | empty | `POSTWERK_INSTANCE`: added to every log line and event. |
| `secrets.senders`, `secrets.storeKey`, `secrets.hashKey`, `secrets.uiTokens` | `postwerk-senders`/`senders`, `postwerk-store-key`/`store-key`, `postwerk-hash-key`/`hash-key`, `postwerk-ui-tokens`/`tokens` | Existing Secrets (name and key). |
| `tls.enabled` | `true` (**recommended**) | STARTTLS on ingress with the certificate in `tls.secretName`. |
| `tls.secretName` | `postwerk-tls` | Secret of type `kubernetes.io/tls`. |
| `tls.certManager.enabled` | `false` | Create a cert-manager `Certificate` that fills `tls.secretName`. |
| `tls.certManager.issuerRef`, `duration`, `renewBefore`, `dnsNames` | `internal-ca` ClusterIssuer, `2160h`, `360h`, the Service names | Issuer and lifetime; the DNS names default to `<svc>`, `<svc>.<ns>`, `<svc>.<ns>.svc`, `<svc>.<ns>.svc.<clusterDomain>`. |
| `tls.allowInsecureAuth` | `false` | Only with `tls.enabled=false`: cleartext `AUTH`, for a network that encrypts traffic between nodes. The chart refuses `tls.enabled=false` without it. |
| `targets[]` | Mailjet over STARTTLS | `name`, `url`, `credentialsSecret`, `status`, `optional`, `retryWindow`, `onUnknown`, `allowRecipients`, as in configuration.md "Targets". |
| `retryWindow` | `24h` | `POSTWERK_RETRY_WINDOW`. |
| `limits.maxMessageBytes` | `15728640` (**recommended**) | `POSTWERK_MAX_MESSAGE_BYTES`, the `SIZE` Mailjet offers. |
| `limits.maxRecipients`, `limits.maxConnections` | `50`, `100` | `POSTWERK_MAX_RECIPIENTS`, `POSTWERK_MAX_CONNECTIONS`. |
| `limits.maxInflightBytes` | `268435456` (**recommended**) | `POSTWERK_MAX_INFLIGHT_BYTES`, the memory budget for messages being received. |
| `shutdownTimeout`, `terminationGracePeriodSeconds` | `100s`, `120` | The grace period must stay longer than the shutdown deadline. |
| `storeEncryption` | `on` (**recommended**) | `POSTWERK_STORE_ENCRYPTION`; `off` also drops the store key mount. |
| `retention.rawKeepAfterHandoff` | `0` (**recommended**) | `POSTWERK_RAW_KEEP_AFTER_HANDOFF`. |
| `retention.rawKeepAfterFailure` | `72h` (**recommended**) | `POSTWERK_RAW_KEEP_AFTER_FAILURE`. |
| `retention.rawMaxAge` | `7d` (**recommended**) | `POSTWERK_RAW_MAX_AGE`. |
| `retention.pseudonymizeAfter` | `30d` (**recommended**) | `POSTWERK_PSEUDONYMIZE_AFTER`; `"0"` turns it off and drops the hash key mount. |
| `retention.recordRetention` | `90d` (**recommended**) | `POSTWERK_RECORD_RETENTION`. |
| `retention.eventRetention` | `90d` (**recommended**) | `POSTWERK_EVENT_RETENTION`. |
| `retention.secureDelete` | `on` (**recommended**) | `POSTWERK_SECURE_DELETE`. |
| `ui.enabled` | `true` | The internal UI on `127.0.0.1:<ui.port>` in the pod (`POSTWERK_ADMIN_LISTEN`), with the tokens from `secrets.uiTokens`. `false` sets `POSTWERK_ADMIN_LISTEN=off`. |
| `ui.port` | `8081` | Loopback port of the UI; not 2525 or 8080. It is never added to the Service. |
| `ui.hosts` | `[]` | Extra host names the UI answers to (`POSTWERK_ADMIN_HOSTS`); not needed with port-forward. |
| `extraEnv` | `[]` | More `POSTWERK_*` variables as `{name, value}`. |
| `service.exposeHTTP` | `false` | Add port 8080 to the Service, for monitoring. |
| `persistence.size`, `storageClassName`, `existingClaim` | `1Gi`, default class, empty | The data volume. |
| `resources` | requests 100m CPU and 128 MiB, limits 1 CPU and 1 GiB | Memory limit: plan about 3 × `maxInflightBytes` plus 50 MiB. Go sets `GOMAXPROCS` from the CPU limit, and Postwerk runs that many password checks at once. |
| `networkPolicy.enabled` | `true` (**recommended**) | Section 7. |
| `networkPolicy.smtpFrom` | pods in the namespace labelled `postwerk-client=true` | NetworkPolicy peers that may connect to port 2525. |
| `networkPolicy.httpFrom` | `[]` | Peers that may reach port 8080; probes need no entry. |
| `networkPolicy.egress.dns` | kube-dns in `kube-system` | DNS for the target host names. |
| `networkPolicy.egress.targetPorts`, `targetCIDRs` | `[587, 465]`, `[0.0.0.0/0]` | Where Postwerk may connect. Provider addresses change, so any address is allowed on these ports by default. |
| `networkPolicy.egress.extra` | `[]` | More egress rules, for example to a capture tool in the cluster. |

## 5. Wiring an application

An application needs four things: the host, the port, a sender name and its password. Give its pod the label that
`networkPolicy.smtpFrom` expects, and put the password in its own Secret:

```yaml
# in the application's Deployment
metadata:
  labels:
    postwerk-client: "true"
spec:
  containers:
    - name: app
      env:
        - name: SMTP_HOST
          value: postwerk.mail.svc.cluster.local   # one of the certificate's DNS names
        - name: SMTP_PORT
          value: "2525"
        - name: SMTP_USER
          value: auth-service                      # a name from the senders file
        - name: SMTP_PASSWORD
          valueFrom:
            secretKeyRef: {name: auth-service-smtp, key: password}
        - name: SMTP_SECURITY
          value: starttls                          # STARTTLS required, never "none"
```

The variable names depend on the application (`SMTP_HOST`/`SMTP_PORT`/`SMTP_USER`/`SMTP_PASS`,
`MAIL_HOST`/`MAIL_PORT`/`MAIL_USERNAME`/`MAIL_PASSWORD`, a URL such as `smtp://auth-service:…@postwerk.mail.svc:2525`
with STARTTLS). What matters everywhere: port 2525, STARTTLS on, `AUTH PLAIN` or "login with user and password",
and a host name that is in the certificate. An application in another namespace needs a `namespaceSelector` entry in
`smtpFrom` (example in section 4).

A `250` from Postwerk means "stored", not "delivered" (design.md section 4). A `451` or `454` is temporary: well-behaved
clients retry it.

## 6. TLS inside the cluster

Postwerk offers STARTTLS with the certificate in `tls.secretName`. Many clients refuse to send a password otherwise:
Go's `net/smtp` (and gomail on top of it) only sends `AUTH PLAIN` over TLS or to `localhost`, and verifies the
certificate against the system roots. So the certificate must come from a CA the clients trust, and name the host they
connect to.

**An internal CA with cert-manager.** A self-signed root, a CA issuer on it, then the chart's Certificate:

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: selfsigned}
spec: {selfSigned: {}}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: {name: internal-ca, namespace: cert-manager}
spec:
  isCA: true
  commonName: internal-ca
  secretName: internal-ca
  duration: 87600h          # 10 years
  privateKey: {algorithm: ECDSA, size: 256}
  issuerRef: {name: selfsigned, kind: ClusterIssuer}
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata: {name: internal-ca}
spec:
  ca: {secretName: internal-ca}
```

Then install the chart with `tls.certManager.enabled=true` and `issuerRef.name=internal-ca`.

**Making clients trust the CA.** Put the CA certificate (`ca.crt` of the `internal-ca` Secret, never its key) into a
ConfigMap in each client namespace, by hand or with cert-manager's trust-manager (a `Bundle` with
`useDefaultCAs: true` plus the internal CA), and mount it into the client.

- **Go clients:** `SSL_CERT_FILE` replaces the system root file, so point it at a bundle that holds **both** the public
  roots and the internal CA, or the application loses access to every public TLS endpoint:

  ```yaml
  env:
    - name: SSL_CERT_FILE
      value: /etc/ssl/bundle/ca-bundle.crt   # public roots + internal CA
  volumeMounts:
    - {name: ca-bundle, mountPath: /etc/ssl/bundle, readOnly: true}
  volumes:
    - name: ca-bundle
      configMap: {name: ca-bundle}           # e.g. from a trust-manager Bundle
  ```

  `SSL_CERT_DIR` works too (Go reads every file in the listed directories), with the same rule.
- **Other runtimes:** Node.js `NODE_EXTRA_CA_CERTS` (adds to the built-in roots), Python `SSL_CERT_FILE` or the
  library's CA option, Java a trust store, PHP `openssl.cafile`. Turning certificate checks off is not an option: it
  sends the password to whoever answers.

**Renewal.** cert-manager renews the certificate `renewBefore` (15 days) before it expires and updates the Secret.
The kubelet swaps the mounted files within a minute or two, and Postwerk re-reads them every minute: the next STARTTLS
gets the new certificate, no restart needed. A pair that does not load (key does not match, expired, unreadable)
is not used; Postwerk keeps the old one and writes `tls.reload_failed`. If the served certificate has less than 14
days left, `tls.expiring` is written once a day, so a stuck renewal shows up before clients fail. Alert on both.
Mount the Secret as a volume (the chart does); a `subPath` mount is never updated by Kubernetes.

**Without TLS.** Only if the cluster network encrypts traffic between nodes (for example WireGuard in Cilium or
Calico): `tls.enabled=false` and `tls.allowInsecureAuth=true`, and write down why.

## 7. Network rules

- **Never expose Postwerk outside the cluster.** No Ingress, no `LoadBalancer`, no `NodePort`, no `hostPort`, no
  `hostNetwork`. The chart creates none of these; do not add them. The UI (and later the query API) is on the pod's
  loopback and is opened with `kubectl port-forward` (section 7.1).
- **SMTP (2525)** only from the pods in `networkPolicy.smtpFrom`. Keep the list short: every pod on it can try to log
  in, and a sender that logs in can send mail in your name.
- **HTTP (8080)** only from `networkPolicy.httpFrom`, for example a monitoring namespace. Probes need no entry: the
  kubelet on the pod's own node can always reach it (Kubernetes NetworkPolicy does not block traffic from a pod's
  node).
- **Egress** only DNS and the target ports 587 and 465. Provider addresses change without notice, so the default
  allows those ports to any address; narrow `targetCIDRs` if your provider publishes fixed ranges. A target inside the
  cluster (a capture tool) needs an `egress.extra` rule.
- **Access for people** only through a tunnel: the UI with `kubectl port-forward` (section 7.1), the health port
  with `kubectl -n mail port-forward deploy/postwerk 8080:8080` (then `curl http://127.0.0.1:8080/readyz`). Never a
  public address.
- **No per-IP blocking:** pod addresses change; the NetworkPolicy decides who can connect, the login decides who can
  send (decisions D26, D28).

### 7.1 Opening the UI

The UI listens on `127.0.0.1:8081` inside the pod. `kubectl port-forward` connects to the pod's loopback, so it is the
way in, and the only one:

```sh
kubectl -n mail port-forward deploy/postwerk 8081:8081
# open http://localhost:8081 and sign in with your name and token
```

Any local port works (`9000:8081`, then `http://localhost:9000`). The forwarded port is open to every process on your
machine while the command runs, which is why the UI still asks for a token; stop the command when you are done.

Who may open the UI is decided by Kubernetes RBAC: the `create` verb on `pods/portforward` in Postwerk's namespace
(plus `get` on pods and deployments to find the pod). Grant it to the people who may see mail records, and to nobody
else; with `admin` tokens they see recipient addresses in full.

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: postwerk-ui, namespace: mail}
rules:
  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list]
  - apiGroups: [""]
    resources: [pods/portforward]
    verbs: [create]
  - apiGroups: [apps]
    resources: [deployments]
    verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: postwerk-ui, namespace: mail}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: postwerk-ui}
subjects:
  - {kind: Group, name: mail-operators, apiGroup: rbac.authorization.k8s.io}
```

The tunnel travels inside the API server's TLS connection. Do not add a Service port, an Ingress, a
NetworkPolicy rule or a sidecar proxy for the UI: each would make it reachable from the cluster network. Every login
and search is an event in the pod log (`ui.login`, `ui.login_failed`, `ui.search`, with the token name as `actor`).

## 8. Backups

The volume holds the queue, delivery records and events; the raw messages in it are encrypted with the store key.

- **Volume snapshots** (CSI `VolumeSnapshot`) are the simplest: SQLite in WAL mode with `synchronous=FULL` recovers a
  crash-consistent copy of the database and its WAL file like after a power loss, as long as the snapshot covers both
  files at the same moment (one volume does).
- **A file copy** needs Postwerk stopped: `kubectl scale deploy/postwerk --replicas=0`, copy `postwerk.db` (no WAL is
  left after a clean stop) with a helper pod that mounts the claim, then scale back to 1. Mails sent meanwhile get a
  connection error and are retried by the senders.
- **Keys apart from backups.** A backup is useless without the store key and holds pseudonymized addresses that the
  hash key can look up. Keep the key files in your secret store, not next to the backups.
- **Retention.** Backups keep their own copies: do not keep them longer than `retention.recordRetention` (90 days by
  default), and record that in your records of processing ([privacy-and-security.md](privacy-and-security.md)).

## 9. Upgrades

- `helm upgrade postwerk deploy/helm/postwerk -n mail -f values-prod.yaml`. With `Recreate`, the old pod stops (it
  finishes the send in progress, at most 90 s) before the new one starts, so SMTP is unavailable for a few seconds.
  Senders get "connection refused" or a timeout and retry; mails already accepted wait in the store.
- Schema migrations run at start, automatically, in a transaction each. There is no downgrade: an older Postwerk
  refuses a database with a newer schema. Take a snapshot before an upgrade, and roll back by restoring it together
  with the older image.
- Keep `terminationGracePeriodSeconds` (120) longer than `shutdownTimeout` (100 s).
- Read the release notes for new settings; an unknown `POSTWERK_*` variable stops the start, so a typo shows up at
  once in `kubectl logs`.
