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

## Verify

```sh
kubectl -n quetzal get pods           # apiserver + controller Running
curl https://quetzal.example.com/api/healthz   # {"status":"ok"}
curl https://quetzal.example.com/api/version   # build info
```

The panel footer and `GET /api/version` both report the running build, so you can
confirm the deployed version at a glance.
