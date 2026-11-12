# Upgrading Quetzal

Quetzal keeps the database as its source of truth, so upgrades are normally a
matter of rolling its Deployment to a newer image. Schema migrations run
automatically on startup.

## Before you upgrade

1. **Check the [CHANGELOG](../CHANGELOG.md)** for the target version (breaking
   changes, new required settings).
2. **Back up the panel database** — it is the source of truth, and Quetzal does
   not back it up itself: its backups hold the game servers' data, not the
   panel's. For PostgreSQL, take a normal dump. For SQLite, snapshot the volume
   if your storage can (a `VolumeSnapshot`), or copy it out with the panel
   stopped — the database runs in WAL mode, so a copy of a live file can miss
   what the log still holds, and the panel's image has no shell or `tar` for
   `kubectl cp`. The claim is `<release>-data`, `quetzal-data` for a release
   named `quetzal`:

   ```sh
   kubectl -n quetzal scale deploy/quetzal --replicas=0
   kubectl -n quetzal run db-copy --image=busybox:1.37 --restart=Never --overrides='{
     "spec": {
       "securityContext": {"runAsNonRoot": true, "runAsUser": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
       "containers": [{
         "name": "db-copy", "image": "busybox:1.37", "command": ["sleep", "600"],
         "securityContext": {"allowPrivilegeEscalation": false, "capabilities": {"drop": ["ALL"]}},
         "volumeMounts": [{"name": "data", "mountPath": "/data", "readOnly": true}]
       }],
       "volumes": [{"name": "data", "persistentVolumeClaim": {"claimName": "quetzal-data"}}]
     }
   }'
   kubectl -n quetzal wait --for=condition=Ready pod/db-copy
   kubectl -n quetzal exec db-copy -- tar -C /data -cf - . > quetzal-db-$(date +%F).tar
   kubectl -n quetzal delete pod db-copy
   kubectl -n quetzal scale deploy/quetzal --replicas=1
   ```

   The pod is admitted under the Pod Security Standards' `restricted` level,
   as the panel's own is.
3. Note your current version: `curl https://<panel>/api/version` (or the panel
   footer).

## Security hardening in 0.15.0

- Upgrade the API and controller together. Let active backups and transfers
  finish first; disable scheduled chains before the upgrade. New operations
  persist their execution metadata and cancellation generation.
- The data-manager now always installs the root-confined file helper from
  `QUETZAL_IMAGE`. Helm already supplies it. Development controllers must set
  it to an image built from the same checkout and available to the cluster.
  Existing data-manager pods roll once; game pods need not restart for this.
- Stop and start servers using the transparent UDP proxy to activate its new
  flow limit immediately. By design, an already running proxy otherwise keeps
  its helper image until the server next sleeps or another change restarts it.
- Replace remote kubeconfigs that use local credential paths or authentication
  plugins with self-contained service-account configs. A rejected stored config
  cannot be used until it is replaced.
- Channels with their own SMTP host may only contact public addresses. To use
  an internal relay, configure the administrator's panel-wide SMTP settings and
  let channels use that relay without overriding its transport settings.
- If `qctl create` (the command-line client built from `cmd/qctl`, not part of the
  image) previously set variables marked secret by their template,
  **rotate those credentials**: their old values may have reached API readers,
  database copies and exports. Startup now moves values marked secret by the
  current template or the server's pinned revision into encrypted storage under
  the existing key. Already sealed values and unknown public overrides are
  preserved. Credentials whose secret metadata was removed cannot be identified
  automatically; inventory those separately. Previously disclosed copies still
  require credential rotation. Prefer the
  panel/API over `--env` for new secrets, since command-line arguments remain
  visible in process listings and shell history.
- SQLite writers now require owner-only files. API and controller must run as
  the same OS user; retain the encryption key and protect database backups.
- Do not manually delete Jobs for active backups, restores or SQL imports.
  Their completion is durable evidence. If an already observed Job disappears,
  the panel keeps the operation exclusive rather than repeating destructive
  work. An operator must inspect the recorded execution cluster, stop any
  remaining writer pods and establish the real outcome before repairing the
  operation record. Existing orphaned snapshots or previously lost backup
  history are not automatically reconstructed.

## Upgrade with Helm

```sh
helm upgrade quetzal oci://ghcr.io/lolozini/charts/quetzal \
  --version X.Y.Z \
  --namespace quetzal \
  --reset-then-reuse-values
```

The registry has the chart from 0.5.0 on; for an earlier version, use the file
attached to its release,
`https://github.com/lolozini/quetzal/releases/download/vX.Y.Z/quetzal-X.Y.Z.tgz`,
in place of the `oci://` address. The released chart already points at its own
image. From a checkout of the
repository instead, pin the image yourself:

```sh
helm upgrade quetzal ./deploy/quetzal \
  --namespace quetzal \
  --reset-then-reuse-values \
  --set image.tag=vX.Y.Z
```

- `--reset-then-reuse-values` (Helm 3.14 or later) keeps the settings you changed and
  takes the new chart's defaults for everything else. `--reuse-values` keeps the
  previous release's whole set of values, so a setting the new chart adds never reaches
  you, and the upgrade quietly runs without it. Override only what changes.
- Coming from **0.1.0**: its chart defaulted to an image tag that was never
  published, so an install made from it had `image.tag` set by hand, and
  the upgrade keeps that setting. Pass `--set image.tag=vX.Y.Z`, or
  `--set image.tag=` to follow the chart, or the upgrade stays on the old image.
- The generated `QUETZAL_SECRET_KEY` is reused across upgrades (do not rotate it
  unintentionally — existing encrypted values would become unreadable).
- A `migrate`-only init container applies schema migrations before the new
  apiserver and controller containers start, so the two never race on the
  schema.

## Verify

```sh
# One Deployment, named after the release, holding both containers.
kubectl -n quetzal rollout status deploy/quetzal
curl https://<panel>/api/version    # should report the new commit
```

Game servers are reconciled from the database, so they are re-applied to match
the new controller without manual steps. Running game pods are not restarted by
an upgrade unless their desired spec changed; the release notes say when a
version changes it.

A server's pods run small helpers out of the Quetzal image: the config file
renderer, the SFTP server and the wake-on-connect activator. A running pod keeps
the helper image it started with, and gets the new one the next time it
restarts for another reason: a stop and start, hibernation, a changed setting.
The activator is the exception: it takes the new image while its server
sleeps, when nobody goes through it but a player waking the server, who at
worst tries again. In wake-and-drop mode it only runs then, so it takes it with
the upgrade; the transparent proxy carries every player while the game is up,
and takes it the next time the server sleeps.

## Rolling back

```sh
helm rollback quetzal <REVISION> --namespace quetzal
```

Roll back only to a version whose schema is compatible with your current
database. Migrations are forward-only; if a release introduced an incompatible
schema change, restore the database backup taken before the upgrade.

**Restoring a database backup deletes the servers made since it was taken.** The
controller treats a server's namespace with no row in the database as left over
from a deleted server, and deletes it, its volume included. After a restore, the
servers created since the backup are in that case. The controller waits an hour
from the first time it sees a namespace without a row (`DefaultOrphanGrace`; a
restart of the controller starts that hour over), logs each one it is waiting
on, and refuses to collect more than three, and more than a quarter of the
namespaces, in one pass. It also says so where it will be seen: the panel shows
administrators with the `servers` permission a banner that names the namespaces
and how long they have left, and a `namespace.orphaned` event goes to the
notification channels, once each time the set changes (a channel with no filter
receives it). That is a safety net, not a plan: before restoring,
scale the controller to zero (`kubectl scale deploy/quetzal --replicas=0`, which
also stops the panel), take a backup of any server created since the one you are
restoring, and bring it back up only once you have checked that the servers you
expect are in the panel.

## Upgrading across several versions

Each release's changelog entry opens with what behaves differently for someone coming
from the version before it. Skipping releases means reading each of those blocks;
this is the same list in one place, for the releases since 0.15.0. It is a summary:
the changelog entry it points to has the reasons and the exceptions.

| Moving to | What to do or know |
|---|---|
| 0.15.1 | The hibernation idle timeout is saved with Enter or Save, no longer on every keystroke. |
| 0.15.2 | Nothing: the same code as 0.15.1, rebuilt with a newer Go. |
| 0.16.0 | Nothing required. A managed database host's network policy now closes its egress but for DNS, picked up on the controller's first resync, with no restart. Optional: `networkPolicy.enabled` fences in the panel. |
| 0.16.1 | Upgrade the chart and the image together, as `helm upgrade` does; if you pin the image, move it to 0.16.1 first. On a remote cluster registered with the manifest the panel offers, re-apply that manifest. |
| 0.16.2 | An API client that cleared a server's annotations by sending an exposure without them must send `"annotations": {}` instead. |
| 0.17.0 | Each managed database restarts once, for a few seconds: restart a game server that keeps its connections open (TeamSpeak) once the database is back. An object of a server changed by hand is put back within five minutes. Re-apply the manifest on remote clusters. |
| 0.18.0 | A game's image is looked up again at each start, so a tag that moved upstream (`:latest`) can bring a new game version at the next restart. An owner can no longer switch a server to a load balancer (403). On a cluster whose role cannot relabel namespaces, a server stops until its namespace carries a Pod Security level. |
| 0.19.0 | A set `QUETZAL_SECRET_KEY` that is not 32 raw characters, 44 of base64 or 64 of hex stops the apiserver, controller and `qctl`. Session cookies are Secure as soon as `ingress.tls` is true (set `secureCookies=true` when TLS ends at a proxy). A namespace without a server row waits an hour before it is collected: read the restore warning above before restoring a database. The controller restarts if it makes no progress for ten minutes. The activator's proxy refuses connections beyond its limits. |
| 0.20.0 | A script that changed an account's address or password, set up or switched off two-factor, or made or deleted API keys or SFTP keys with an API key gets 403 and must use a session. `PUT /api/me/email` takes `password` (and `code`). A password reset deletes the account's API keys and SFTP keys. The first start with a valid `QUETZAL_SECRET_KEY` seals secrets an instance had written in clear text. |

The per-address limit of 0.19.0 turned players behind a proxy or a NAT away: go to 0.20.0
rather than stopping at 0.19.0 if you run servers in proxy mode.

## Version pinning

Pin a released tag (`vX.Y.Z`) in production rather than `latest`/`main`, so
upgrades are deliberate and reproducible. Releases (image + packaged Helm chart)
are published on the GitHub Releases page.
