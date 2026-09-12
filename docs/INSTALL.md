# Installing Quetzal

Quetzal runs as two Deployments (an API server and a controller) in your
cluster, backed by a database. The bundled Helm chart wires up RBAC, the
Deployments, a Service, and an optional Ingress.

## Prerequisites

- A Kubernetes cluster and `kubectl` access. Quetzal is tested on **1.31** (its
  end-to-end suite runs there on every change) and **1.33**. Nothing it uses is
  newer than 1.23 — there are no native sidecars, no admission webhooks, no
  custom resources — so older clusters are likely to work, but "likely" is all
  anyone can honestly say about a version nobody tests. **1.29 or later** is the
  version to be on.

  One optional hardening layer wants a newer cluster than the rest: the
  admission policy that keeps the control plane's account inside its own
  namespaces needs **1.30+**, where ValidatingAdmissionPolicy went GA. Without
  it Quetzal runs exactly the same; you lose that one guard, not a feature. See
  the security notes below.
- [Helm](https://helm.sh/) v3.
- A storage class for persistent volumes. Single-node / homelab setups can use a
  local provisioner such as [local-path](https://github.com/rancher/local-path-provisioner).
- For per-server CPU/RAM graphs: [metrics-server](https://github.com/kubernetes-sigs/metrics-server)
  (optional; the panel degrades gracefully without it).

## Container images

Images are published to the GitHub Container Registry:

```
ghcr.io/lolozini/quetzal:latest      # rolling build of main
ghcr.io/lolozini/quetzal:vX.Y.Z      # tagged releases (recommended)
```

Pin a released tag in production rather than `latest`.

## Install with Helm

```sh
helm install quetzal ./deploy/quetzal \
  --namespace quetzal --create-namespace \
  --set image.tag=vX.Y.Z \
  --set ingress.enabled=true \
  --set ingress.host=quetzal.example.com
```

See [deploy/quetzal/values.yaml](../deploy/quetzal/values.yaml) for every option.
Common ones:

| Setting | Purpose |
| --- | --- |
| `image.tag` | Image version to run (pin a release). |
| `ingress.enabled` / `ingress.host` | Expose the panel over an Ingress. |
| `persistence.*` | PVC for the SQLite database (the source of truth). |
| `persistence.existingClaim` | Mount a claim you made yourself instead (a pre-provisioned volume, or an existing install you are moving onto this chart). |
| `secretKey.existingSecret` | Take the encryption key from your own Secret rather than one the chart generates. |
| `extraEnv` | Extra environment for every container. `TZ` sets the zone shown in logs and used by a schedule that names none of its own — each schedule can carry its own IANA zone instead. |
| `nodePort.min` / `nodePort.max` | Control-plane pool for NodePort game ports. |
| `retention.eventDays` | How long delivered events are kept (default 30; 0 keeps everything). The event table is written on every power action, crash and restart. |
| `retention.auditDays` | How long audit entries are kept. **0 by default — nothing is deleted**; set a number of days if you would rather bound the table. |
| `replicaCount` | Control-plane replicas. More than one needs `db.driver=postgres` and `persistence.enabled=false`; the chart refuses the other combinations rather than let two pods share one SQLite file. |
| `image.repository` / `image.tag` | Also the image used for the config-render, SFTP and wake-on-connect helpers (`QUETZAL_IMAGE`); the chart derives it, there is nothing to set. |

### Secret key

Quetzal encrypts application secrets (S3 creds, SMTP, server secret env) at rest
with a key from `QUETZAL_SECRET_KEY`. The chart generates one on first install
and reuses it across upgrades.

To hold the key yourself — in SOPS, or an external secret operator — point the
chart at your own Secret and it will generate nothing:

```sh
--set secretKey.existingSecret=my-quetzal-key \
--set secretKey.existingSecretKey=QUETZAL_SECRET_KEY
```

Whichever way you manage it, keep the value stable. Everything already encrypted
becomes unreadable under a new key, so an install that holds data wants the old
key migrated across, not a fresh one issued.

### Database

- **SQLite** (default): single file on a PVC; simplest for homelab/single-node.
- **PostgreSQL**: set `QUETZAL_DB_DRIVER=postgres` and `QUETZAL_DB_DSN`
  accordingly for multi-replica / production.

Schema migrations run automatically (a `migrate`-only init container runs before
the app starts, avoiding a schema race between the two Deployments).

## First run

Open the panel and complete the first-run admin setup (create the initial admin
account). From there you can register clusters, import templates/eggs, and create
servers.

## Backup target (S3)

Backups run `restic` against an S3-compatible bucket, configured in the panel
under the backup settings. Restic is not an upload-only client: it reads its own
index and lock files on every run, and prunes old data when a snapshot is
forgotten. The key you give Quetzal therefore needs full object access to the
prefix, not just write access:

| Operation | Needed for |
|---|---|
| `s3:ListBucket` (on the bucket, limited to the prefix) | finding the repository and its snapshots |
| `s3:GetObject` | reading the index, config and lock files |
| `s3:PutObject` | writing snapshots |
| `s3:DeleteObject` | retention (`--keep-last`) and deleting a snapshot |

A write-only key — the sane choice for a one-way backup cronjob, and a common
thing to have lying around — fails on the very first run with
`create key in repository … failed: Stat: Access Denied`. If you see that, the
credentials are the thing to check, not the endpoint or the bucket name.

Point Quetzal at its own prefix rather than sharing one with other backups: it
creates a separate repository per server underneath, and retention deletes
inside it.

The bucket has to exist already. Quetzal checks it when you save the target and
refuses one it cannot find, because restic would otherwise create it: a typo in
the name would not fail, it would quietly start a second bucket and send the
backups there.

Deleting a server purges its snapshots along with its volume. Nothing in the
panel could reach them afterwards — no row references them — so leaving them
would mean paying to store data that can no longer be listed or restored. Take a
copy first if you want to keep a deleted server's history.

## Registering another cluster

Quetzal reaches a remote cluster with a kubeconfig you paste into the panel
(stored encrypted). The easiest kubeconfig to hand over is your admin one, and
that is the thing to avoid: it gives the control plane — and anyone who reaches
it — everything on that cluster.

The cluster form carries the alternative. Open **Prepare the remote cluster**
before registering: it shows a manifest to apply there, creating a service
account with the access Quetzal actually needs and nothing else, followed by the
script that prints a kubeconfig for it. Paste that one instead.

The permissions in that manifest are the same set the chart grants on the
cluster Quetzal runs on, less leader election, which only happens where the
control plane lives. A test keeps the two from drifting apart.

## Security notes

Worth knowing before you hand accounts to other people.

**The control plane is scoped to the namespaces it creates.** It holds
cluster-wide access only to what is genuinely cluster-scoped — namespaces,
nodes, volumes, storage classes. Everything a server needs is a role it grants
itself inside each namespace it makes, so it cannot read a Secret in
kube-system, exec into a pod that is not a game server, or run a workload in
somebody else's namespace.

Handing that role out requires creating RoleBindings, and RBAC can only grant
that cluster-wide — which on its own would let the account bind its own role
anywhere and read everything after all. `admissionPolicy.enabled` closes that:
an admission policy confines those bindings, and namespace deletions, to
namespaces Quetzal owns. It needs 1.30; turn it off below that and the scoping
still stands, but the escalation becomes possible again — noisily, since
creating a RoleBinding in kube-system is an audited write where reading a Secret
is not.

Still treat the namespace it runs in with care: it holds the database, the
encryption key and every stored credential. If you host for others and want a
hard boundary, register a second cluster and put the game servers there; the
panel keeps running where it is.

**Game servers themselves are confined.** Their pods mount no service account
token, run with every capability dropped and no privilege escalation, and their
NetworkPolicy allows DNS and the public internet only — not the cluster network,
not the node, not your LAN. A managed database is reachable because it is
granted explicitly; anything else on a private address needs `egressAllow`.

**The `templates` admin permission is the powerful one.** An install script runs
as root, because that is what Pterodactyl egg scripts expect (they run `apt` and
`apk`), and Wings does the same. It keeps the capabilities package managers need
and drops the ones they never use — `NET_RAW` above all, so a compromised script
cannot spoof or sniff on the node's network — and it has no API credentials and
no cluster network. It is root in its own container and nowhere else, but grant
the permission accordingly, and do not import eggs you have no reason to trust.

An egg's install script runs as its own process, the way Wings runs it, and not
inlined into the logic around it. That matters for a reason worth knowing if you
write eggs: a top-level `exit` in the script -- including a bare `exit`, which
takes the status of the line before it and so is usually 0 -- would otherwise end
that logic too, skipping the record that the install happened and the handover of
the files to the server's user. The script decides its exit status; Quetzal
decides what that means.

An install also has a deadline: six hours, after which it is stopped and the
server goes to Error with the reason in its setup log. That is far longer than
any install should take — a SteamCMD download of a large game legitimately runs
for hours — and exists so a script that hangs (a download from a host that no
longer answers) ends somewhere instead of leaving the server in Installing for
good. A stopped install is not recorded as done, so it runs again on the next
start.

**Brute-force counters live in the database**, so they survive a restart and are
shared by every replica: the configured limit is the limit, not the limit times
the number of pods. If the database is unreachable the limiter falls back to
counting in memory — still limited, just per-process.

**Delegated admin roles stop short of the privilege system.** A scoped admin
never reaches an account that outranks it: granting admin status, assigning
roles, and resetting, deleting or clearing the two-factor of any account with
admin standing are all superadmin-only. So is changing the SMTP settings and the
public URL, which the `settings` permission can read but not write -- whoever
picks the mail relay reads every password reset link the panel sends, which
would otherwise be a way to take the superadmin's account.

**You can require a second factor.** Admin → Two-factor policy sets it to off,
administrators only, or everyone. Turning it on locks nobody out, including the
superadmin who turns it on: a covered account still logs in and keeps its
session, but reaches only its own profile, the enrolment endpoints and logout
until it has a factor, and the panel shows the enrolment page rather than a wall
of refusals. Changing the policy is superadmin-only, for the same reason as the
mail relay — it decides who gets in.

**Changing a password ends the account's other sessions.** Both the self-service
change and an admin reset, keeping only the client that asked. It is the thing
people do when they believe a session was stolen, so it has to be the thing that
works. API keys are separate credentials and survive it: delete them too if they
may be compromised.

**A subuser's permissions bound what their schedules may do.** A scheduled task
is checked against the permission the action itself needs — `console` for a
command, `power` for start/stop/restart, `backups` for a backup — so `schedules`
is not a way around them. Switching a schedule off needs nothing more than
`schedules`, so anyone who can manage them can stop one misbehaving. Note that a
schedule keeps the permissions it was written with: revoking someone's console
access does not disable the schedules they already made, so review a server's
schedules when you take a permission away.

**An API key carries everything its owner can do.** There is no per-key scope
today, so a key minted by an admin is an admin key. Treat one as the account
itself and delete keys you no longer use.

**Metrics are on a port the Ingress does not publish.** The apiserver serves
`/metrics` on container port 9091 and the controller on 9090, and the Service
carries neither — the panel's own port is published at `/` prefix, so anything
served beside the panel is readable by anyone with the URL. Scrape them
in-cluster (a PodMonitor, or Prometheus pod annotations). There is no
authentication on those ports: keep them pod-local.

**SFTP drops a connection that does not authenticate**, within 15 seconds, and
caps how many may be mid-handshake at once. The SFTP server is a sidecar inside
the game server's own pod and shares its memory limit, published on a NodePort,
so silent connections are otherwise a way to have a pod OOM-killed from the
internet without any credentials. A flood can still make SFTP itself unreachable
for as long as it lasts; the game server keeps running, which is the trade.

**Registering another cluster**: use the manifest the cluster form offers rather
than an admin kubeconfig. See above.

## Verify

```sh
kubectl -n quetzal get pods           # apiserver + controller Running
curl https://quetzal.example.com/api/healthz   # {"status":"ok"}
curl https://quetzal.example.com/api/version   # build info
```

The panel footer and `GET /api/version` both report the running build, so you can
confirm the deployed version at a glance.
