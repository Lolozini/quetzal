# Installing Quetzal

Quetzal runs as two Deployments (an API server and a controller) in your
cluster, backed by a database. The bundled Helm chart wires up RBAC, the
Deployments, a Service, and an optional Ingress.

## Prerequisites

- A Kubernetes cluster (v1.29+ recommended) and `kubectl` access.
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
| `nodePort.min` / `nodePort.max` | Control-plane pool for NodePort game ports. |
| `systemImage` (`QUETZAL_IMAGE`) | Quetzal image used for config-render / SFTP / activator helpers. Set it to enable those features. |

### Secret key

Quetzal encrypts application secrets (S3 creds, SMTP, server secret env) at rest
with a key from `QUETZAL_SECRET_KEY`. The chart generates one on first install
and reuses it across upgrades. If you manage it yourself, keep it stable — losing
it makes existing encrypted values unreadable.

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

## Verify

```sh
kubectl -n quetzal get pods           # apiserver + controller Running
curl https://quetzal.example.com/api/healthz   # {"status":"ok"}
curl https://quetzal.example.com/api/version   # build info
```

The panel footer and `GET /api/version` both report the running build, so you can
confirm the deployed version at a glance.
