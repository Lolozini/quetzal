# Quetzal

[![CI](https://github.com/lolozini/quetzal/actions/workflows/ci.yml/badge.svg)](https://github.com/lolozini/quetzal/actions/workflows/ci.yml)
[![License: AGPL v3](https://img.shields.io/badge/License-AGPL%20v3-blue.svg)](./LICENSE)
[![Image](https://img.shields.io/badge/ghcr.io-lolozini%2Fquetzal-2496ED?logo=docker&logoColor=white)](https://github.com/lolozini/quetzal/pkgs/container/quetzal)

**Quetzal is a Kubernetes-native control plane and web UI for hosting game
servers** — a self-hosted alternative to Pterodactyl/Pelican that runs your
servers *directly on Kubernetes*, with no node agent to install.

Point it at a cluster, open the panel, and deploy Minecraft, Valheim, or
anything you can describe with a template — or import the
[Pterodactyl/Pelican egg](https://github.com/pelican-eggs) you already use.

---

## Why Quetzal?

Pterodactyl (and its fork Pelican) run a **Wings** daemon on every node to drive
Docker, manage lifecycles, and proxy consoles. Quetzal deletes that whole layer:

> **Kubernetes *is* the daemon.** The panel talks to the Kubernetes API; the
> kubelet runs the containers, the `attach`/`logs` subresources are the console,
> and a controller reconciles your intent into native objects (Deployments,
> Services, PVCs, Secrets, NetworkPolicies). No Wings to install, patch, or babysit.

That gives you, for free, what a bespoke daemon has to reimplement: scheduling
and bin-packing across nodes, self-healing, rollouts, storage classes, RBAC,
network policy, and a multi-cluster API.

- **Game-agnostic.** Minecraft is just one example. Anything that runs in a
  container image and can be configured by environment + files is a first-class
  citizen — described by a **template** (Quetzal's "egg").
- **A migration path, not a rewrite.** Import your existing **Pterodactyl/Pelican
  eggs** as-is (variables, startup, install scripts, `config.files`) — paste the
  JSON, fetch from a URL, or browse an **egg catalog** and install in one click —
  and bring worlds/modpacks/backups in by uploading an archive.
- **Multi-tenant and secure by default.** Namespace-per-server, NetworkPolicy,
  hardened `securityContext`, encrypted secrets, scoped subusers and admin roles.
- **Self-hostable, no lock-in.** SQLite or Postgres, storageClass *or* hostPath,
  any S3-compatible backup target, AGPL-3.0. Nothing hardcoded to one environment.

---

## Features

**Deploy & run**
- Create servers from built-in or imported templates; start / stop / restart /
  kill, with graceful stop via a template's stop command.
- Edit startup variables and CPU/RAM limits after creation (validated against the
  template; secrets preserved).
- **Reinstall** on demand (optionally wiping data) without surprise re-installs on
  normal restarts.
- **Hibernation**: scale idle servers to zero and **wake them on connect** — a
  lightweight TCP "wake-and-drop" mode (no latency, real client IP when awake) or
  an always-in-path TCP+UDP proxy mode (so UDP games can auto-sleep too).

**Console, files & SFTP**
- Live **console** over WebSocket — log stream + stdin via the Kubernetes
  `attach` subresource (no RCON server, no sidecar).
- **File manager**: browse, edit, upload, rename, delete, download folders as
  `.tar.gz`, and upload an archive (world / modpack / Pterodactyl backup) that's
  extracted into the volume. **Works while the server is stopped** via an
  on-demand maintenance pod.
- Opt-in **SFTP** per server, authenticated by users' SSH public keys.

**Networking**
- Publish in-cluster (ClusterIP), on node IPs (**NodePort** from a managed port
  pool), or via a **LoadBalancer** — TCP **and** UDP.
- `externalTrafficPolicy: Local` by default so the game sees the real player IP;
  provider-neutral Service annotations (external-dns, MetalLB, …).

**Data & backups**
- **Backups & restore** to any S3-compatible target via **restic** (dedup,
  encryption, retention) — one-shot Jobs, credentials encrypted at rest.
- Choose to **keep or destroy** a server's data on deletion.
- **Per-server databases**: provision a MySQL/MariaDB database + scoped user from
  the panel, against a registered **external** host *or* a **managed MariaDB**
  Quetzal deploys and owns in-cluster.

**Automation**
- **Scheduled tasks** (cron) as ordered **chains** — e.g. *warn players → wait
  30s → stop → backup → start* — with per-step delays and continue-on-failure.
- **Notifications** to **Discord**, **HMAC-signed webhooks**, or **email/SMTP**
  on events (up / crash / idle-sleep / power / backups), global or per-server,
  delivered from a durable event outbox.

**Multi-tenant & access control**
- Per-server **ownership** and **subusers** with scoped permissions.
- **Granular admin roles**: delegate management of servers, users, templates,
  clusters, database hosts, notifications, settings, or the audit log — without
  handing out full control.
- Admin **suspend**, per-user **quotas**, an append-only **audit log**, and
  **API keys** for the documented REST API.
- **Two-factor auth** (TOTP + recovery codes) and **self-service password reset**
  by email.

**Security by default**
- Namespace-per-server, deny-by-default **NetworkPolicy**, hardened
  `securityContext`, **no ServiceAccount token** in game pods, per-namespace
  **ResourceQuota**.
- Secrets (S3/SMTP creds, server secret env, 2FA, kubeconfigs) **encrypted at
  rest**; never stored in clear text.
- Login/2FA **rate-limiting**, **CSRF** protection, secure cookies.

**Operations**
- **Multi-cluster**: register clusters by kubeconfig (encrypted) and pick a
  deploy target per server; the controller reconciles each against its own
  cluster. **Transfer a server between clusters** (data moves via the backup
  target, with clean rollback on failure).
- Controller is **leader-elected**; Prometheus `/metrics`, health probes, and a
  `GET /api/version` build-info endpoint.
- **Documented API**: OpenAPI 3.0 at `/api/openapi.yaml`, browsable at `/api/docs`.
- **Internationalized UI** (English + French; adding a language is one file).

---

## Quickstart

> **Prerequisites:** a Kubernetes cluster + `kubectl`, [Helm](https://helm.sh) v3,
> and a storage class (or use `hostPath` for single-node). Optional:
> metrics-server for CPU/RAM graphs.

```sh
helm install quetzal ./deploy/quetzal \
  --namespace quetzal --create-namespace \
  --set image.tag=latest \
  --set ingress.enabled=true --set ingress.host=quetzal.example.com
```

Open the panel, complete the first-run admin setup, and create your first server.

Images are published to GHCR — `ghcr.io/lolozini/quetzal:latest` (rolling `main`)
and `:vX.Y.Z` (releases; pin one in production).

Full guides: **[Install](docs/INSTALL.md)** · **[Upgrade](docs/UPGRADE.md)** ·
**[Changelog](CHANGELOG.md)** · all chart options in
[deploy/quetzal/values.yaml](deploy/quetzal/values.yaml).

---

## How it works

The **database is the source of truth.** The API server writes your *desired
state* to the DB; a leader-elected controller reconciles it into native
Kubernetes objects and writes *observed status* back. There are **no CRDs** — the
control plane owns its own schema and stays portable across clusters.

```
Browser ──HTTP/WS──▶  api-server  (UI · REST/WebSocket · console proxy)
                          │ writes desired state
                          ▼
                    ┌───────────┐   reconcile    ┌──────────────────────┐
                    │    DB     │ ◀────────────▶ │  controller (leader) │
                    │ (truth)   │   status back  └──────────┬───────────┘
                    └───────────┘                           │ Kubernetes API
                                                            ▼
                       namespace per server:
                       Deployment · Service · PVC · Secret · NetworkPolicy
```

- **One pod per server**, no per-game side pods. The live console is the
  Kubernetes `attach` (stdin) + `logs` (stdout) subresources.
- **Templates are eggs.** A template declares images, variables (env), startup,
  ports, lifecycle, install script, and `config.files`. Importing a Pterodactyl
  egg maps it onto this model; `config.files` are rendered at startup
  (properties/json/yaml/ini) so imported eggs configure themselves.
- Ships with templates for **Minecraft (Paper)**, **Minecraft (CurseForge
  modpacks)**, **Valheim**, and a **generic process** — import eggs for the rest.

---

## Project status

Quetzal is **young but feature-complete across its roadmap** (phases 0–6:
foundations → MVP → networking/observability → schedules/backups → multi-tenant
→ hibernation → multi-cluster, plus the features listed above). It has a unit +
end-to-end test suite that runs on a real `kind` cluster in CI.

It is **pre-1.0 and not yet battle-tested in production** — expect rough edges,
and pin a released image tag rather than `latest`. Issues, feedback, and eggs are
very welcome.

---

## License

Copyright (C) 2026 Lolozini.

Quetzal is free software under the **GNU Affero General Public License v3.0 or
later** ([AGPL-3.0-or-later](./LICENSE)). In particular, if you run a modified
version to provide a service over a network, you must offer that service's users
the corresponding source code of your modified version.
