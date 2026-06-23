# Quetzal

[![CI](https://github.com/lolozini/quetzal/actions/workflows/ci.yml/badge.svg)](https://github.com/lolozini/quetzal/actions/workflows/ci.yml)

> Kubernetes-native control plane & UI to deploy and manage game servers — in the
> spirit of Pterodactyl/Pelican, **without the Docker layer**.

Quetzal runs game servers **directly on Kubernetes**: the panel talks to the
Kubernetes API, and the kubelet *is* the daemon (no per-node "Wings"). It is
**game-agnostic** — anything you can describe with a template (or import as a
Pterodactyl **egg**) can be deployed.

## Status

🚧 Early development. **Phases 0–6 implemented** (foundations, MVP, networking &
observability, scheduled tasks + backups, multi-tenant access control +
notifications + 2FA, hibernation + egg install scripts, multi-cluster); see
[ROADMAP](#roadmap).

## Install

Container images are published to the GitHub Container Registry on every push to
`main` (and tagged releases):

```
ghcr.io/lolozini/quetzal:latest      # main
ghcr.io/lolozini/quetzal:vX.Y.Z      # releases
```

Deploy with the bundled Helm chart (creates the RBAC, Deployments, Service and,
optionally, an Ingress):

```sh
helm install quetzal ./deploy/quetzal \
  --namespace quetzal --create-namespace \
  --set image.tag=latest \
  --set ingress.enabled=true --set ingress.host=quetzal.example.com
```

Then open the panel and complete the first-run admin setup. See
[deploy/quetzal/values.yaml](deploy/quetzal/values.yaml) for all options.

## Design highlights

- **DB-centric, no CRDs**: the database is the source of truth; a controller
  reconciles DB state into native Kubernetes objects (Deployment / Service / PVC
  / Secret / NetworkPolicy) and writes status back.
- **One pod per server, no per-game side pods**: live console is provided via the
  Kubernetes `attach` subresource (stdin) + log streaming (stdout) — no RCON
  server, no sidecar required.
- **Pluggable exposure**: publish a server in-cluster (ClusterIP), on node IPs
  (NodePort, from a stable control-plane port pool), or via a LoadBalancer —
  with `externalTrafficPolicy: Local` by default so the game sees the real
  player IP, and provider-neutral Service annotations (external-dns, MetalLB…).
- **Per-server observability**: live CPU/memory from metrics-server, plus
  Prometheus `/metrics` for the panel itself.
- **Scheduled tasks**: cron schedules per server (start / stop / restart /
  console command / backup), run by the leader controller.
- **Backups & restore**: per-server data backup/restore to any S3-compatible
  target via restic (encryption, dedup, retention) — one-shot Jobs, no sidecar;
  credentials stored encrypted. Deleting a server can keep or destroy its data.
- **Multi-tenant**: per-server ownership, subusers with scoped permissions,
  admin suspend, per-user quotas, an append-only audit log, and API keys
  (bearer tokens for the public API).
- **Two-factor auth**: opt-in TOTP (RFC 6238) with one-time recovery codes;
  login becomes a password + code challenge, and admins can reset a locked-out
  user. Secrets are encrypted at rest.
- **File manager**: a tree/breadcrumb file browser to edit, upload, rename,
  delete and download a server's files — including whole folders as `.tar.gz`
  archives. Served by exec-ing into the running pod (no sidecar), with paths
  confined to the data volume.
- **SFTP**: opt-in per server — a key-only SFTP sidecar (authenticated by the
  SSH public keys users register in the panel) exposes the data volume over a
  NodePort, confined to that directory and running as the server's own user.
- **Documented API**: the full REST API has an OpenAPI 3.0 spec at
  `/api/openapi.yaml` (use it with any client generator) rendered as browsable
  docs at `/api/docs`.
- **Notifications**: outbound channels — **Discord**, **generic HMAC-signed
  webhooks**, and **email/SMTP** — fired on events (server up/crash/idle-sleep,
  power, backups, …). Channels are global (catch-all) or scoped to one server,
  with per-event-type filters; their secrets are encrypted at rest. Delivery is
  driven by a durable event outbox, so controller-observed events (crashes,
  auto-hibernation) notify too.
- **Hibernation**: opt-in scale-to-zero for idle servers (no player connections),
  woken on demand or **automatically when a player connects** (a tiny per-server
  activator listens while the server sleeps) — saves resources for dormant servers.
- **Install scripts**: egg install scripts run as a one-time init container, so
  install-based eggs work out of the box.
- **Multi-cluster**: register additional clusters by kubeconfig (stored
  encrypted) and pick a deploy target per server; the controller reconciles each
  server against its own cluster. The local cluster needs no credentials.
- **Egg-compatible**: import existing Pterodactyl/Pelican eggs to ease migration,
  including **config.files rendering** at startup (properties/json/yaml/ini) so
  imported eggs configure themselves like they do under Wings.
- **Secure by default**: namespace-per-server, NetworkPolicy, hardened
  securityContext, no ServiceAccount token mounted into game pods, a per-namespace
  ResourceQuota, secrets kept out of the DB in clear text; brute-force rate
  limiting on login/2FA and CSRF protection on the API.
- **Self-hostable & generic**: nothing hardcoded to a specific homelab — SQLite
  by default (Postgres optional), storageClass *or* hostPath, MIT licensed.

## Architecture

```
Browser ──HTTP/WS──▶ api-server (UI + REST/WS + console proxy)
                        └─ writes desired state ─▶ [ DB ] ◀── source of truth
                                                     ▲ │ status / desired
                        controller (leader-elected) ─┘ ▼ reconciles ─▶ Kubernetes API
                                                         └─▶ namespace/server: Deployment+Service+PVC+Secret+NetworkPolicy
```

## Roadmap

- ✅ **Phase 0** — Foundations: data model, store, reconciler, egg importer.
- ✅ **Phase 1** — MVP: lifecycle + config + live console + minimal UI.
- ✅ **Phase 2** — Networking (ClusterIP/NodePort/LoadBalancer + node-port pool) &
  per-server observability (CPU/RAM via metrics-server).
- ✅ **Phase 3** — Scheduled tasks (cron), backups & restore (restic → S3), and
  data lifecycle (keep/destroy on delete). _Deferred to later: web file browser,
  world/modpack upload, CSI volume snapshots, online volume expansion,
  Pterodactyl data import._
- ✅ **Phase 4** — Multi-tenant: ownership + subusers/permissions, admin suspend,
  per-user quotas, audit log, API keys; **notifications** (Discord / webhook /
  email on events, via a durable event outbox); **opt-in 2FA (TOTP) with
  recovery codes**; an **OpenAPI spec + `/api/docs`** for the public API.
- ✅ **Phase 5** — Hibernation (scale-to-zero on idle) + egg install scripts +
  **wake-on-connect**, in two modes:
  - _drop_ (TCP): a tiny activator listens while hibernated and wakes the server
    on connect, then drops it (reconnect once up). Out of the data path when
    awake — no latency, the server sees the real client IP.
  - _proxy_ (TCP+UDP): an always-in-path proxy forwards traffic transparently
    (no reconnect), supports UDP, and reports activity so **UDP servers can also
    auto-hibernate**. Trade-offs: a small extra hop and the server sees the
    proxy's IP, not the client's.

  _Deferred to later: PROXY-protocol / real client IP in proxy mode, git
  template sync, sandboxed runtime._
- ✅ **Phase 6** — Multi-cluster: a kubeconfig-based cluster registry (encrypted
  at rest), per-server deploy target, per-cluster reconcile + GC + status probes,
  and read-only node listing. _Deferred to later: moving a server between
  clusters, per-cluster backup targets / node-port pools, MariaDB/MySQL._

## License

[MIT](./LICENSE).
