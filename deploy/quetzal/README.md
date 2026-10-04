# Quetzal

Game servers, run by Kubernetes. Quetzal is a self-hosted panel for Minecraft,
Valheim and anything you can put in a container: an alternative to Pterodactyl
and Pelican that runs the servers directly on your cluster, with no agent on
the nodes, and imports the eggs you already use.

This chart installs the panel: an API server with the web UI, and a controller
that turns the servers you create into Kubernetes objects, one namespace per
server. Documentation: <https://lolozini.github.io/quetzal/>.

## Prerequisites

- Kubernetes 1.30 or later, amd64 or arm64 nodes. Many games only exist for
  amd64; see [CPU architectures](https://lolozini.github.io/quetzal/install/#cpu-architectures).
- A storage class for the panel's database and for the servers' data.
- Helm 3.8 or later.
- Optional: metrics-server, for the CPU and memory graphs.

## Install

```sh
helm install quetzal oci://ghcr.io/lolozini/charts/quetzal \
  --version <version> \
  --namespace quetzal --create-namespace \
  --set ingress.enabled=true --set ingress.host=quetzal.example.com
```

Open the panel and create the admin account. Without an Ingress, reach it with
`kubectl -n quetzal port-forward svc/quetzal 8080:8080`.

The same chart is attached to each [GitHub release](https://github.com/Lolozini/quetzal/releases)
as `quetzal-<version>.tgz`, if you'd rather install from a file.

The chart and the images are signed. To check the chart before installing it:

```sh
cosign verify ghcr.io/lolozini/charts/quetzal:<version> \
  --certificate-identity-regexp '^https://github\.com/Lolozini/quetzal/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Configuration

Every value is described in [values.yaml](https://github.com/Lolozini/quetzal/blob/main/deploy/quetzal/values.yaml),
and `values.schema.json` refuses unknown keys, so a typo fails the install
instead of being ignored. The ones most installs set:

| Value | Default | What it does |
|---|---|---|
| `ingress.enabled`, `ingress.host`, `ingress.tls` | off | Publish the panel. With TLS, also set `secureCookies=true`. |
| `db.driver` | `sqlite`, on the volume | `postgres` for production, and for more than one replica. |
| `db.host`, `db.port`, `db.name`, `db.user` | —, 5432, quetzal, quetzal | The PostgreSQL server field by field. |
| `db.password`, `db.existingPasswordSecret`, `db.existingPasswordSecretKey` | —, —, password | Its password: in a Secret the chart creates, or read from one of yours. |
| `db.sslMode`, `db.sslRootCertSecret`, `db.sslRootCertSecretKey` | prefer, —, ca.crt | TLS as libpq reads it, and the Secret holding the CA that signed the server's certificate. |
| `db.dsn` | the SQLite file | Or a whole PostgreSQL DSN, which goes into a Secret the chart creates, not into the Deployment. |
| `db.existingSecret`, `db.existingSecretKey` | — | Read that DSN from your own Secret instead (SOPS, External Secrets); `db.dsn` is then ignored. |
| `persistence.size`, `persistence.storageClass` | 1Gi, the default class | The volume that holds the SQLite database. |
| `persistence.existingClaim` | none | Mount a claim you made instead. |
| `secretKey.existingSecret` | generated | The key that encrypts stored credentials. Generated on first install and kept on upgrade; point it at your own Secret to manage it with SOPS or an external secret operator. |
| `nodePort.min`, `nodePort.max` | the cluster's range | The node ports Quetzal hands out to game servers. Narrow it to a block reserved for Quetzal and open on your firewall. |
| `egressAllow` | none | Private address ranges game servers may reach, on top of the internet. |
| `extraEnv` | none | Environment for the panel. Set `TZ`: schedules run in the panel's local time. |
| `trustProxy` | on with the Ingress | Trust `X-Forwarded-For` for rate limiting. |
| `admissionPolicy.enabled` | on | Keeps the panel's account inside the namespaces it creates. |

## Security

The pod runs as a non-root user, with a read-only root filesystem, no
privilege escalation, all capabilities dropped and the runtime's default
seccomp profile: it is admitted in a namespace that enforces the Pod Security
Standards' `restricted` level. The panel's permissions on the cluster are
split: a ClusterRole to create server namespaces, and a role bound only inside
each of them.

## Upgrade and uninstall

```sh
helm upgrade quetzal oci://ghcr.io/lolozini/charts/quetzal --version <version> -n quetzal --reuse-values
```

Read the [changelog](https://lolozini.github.io/quetzal/changelog/) first: each
release says what behaves differently. See also the
[upgrade guide](https://lolozini.github.io/quetzal/upgrade/).

`helm uninstall quetzal -n quetzal` removes the panel but keeps its database
volume and its generated encryption key, so installing again under the same
release name picks up where it left off. The game servers' namespaces stay as
well: delete the servers from the panel first if you want them gone, and
delete the `quetzal-data` claim and the `quetzal-secretkey` Secret to start
from nothing.
