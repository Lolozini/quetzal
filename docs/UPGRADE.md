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

## Security hardening in the unreleased version

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
- If `qctl create` previously set variables marked secret by their template,
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
  --reuse-values
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
  --reuse-values \
  --set image.tag=vX.Y.Z
```

- `--reuse-values` keeps your existing settings; override only what changes.
- Coming from **0.1.0**: its chart defaulted to an image tag that was never
  published, so an install made from it had `image.tag` set by hand, and
  `--reuse-values` keeps that setting. Pass `--set image.tag=vX.Y.Z`, or
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

## Version pinning

Pin a released tag (`vX.Y.Z`) in production rather than `latest`/`main`, so
upgrades are deliberate and reproducible. Releases (image + packaged Helm chart)
are published on the GitHub Releases page.
