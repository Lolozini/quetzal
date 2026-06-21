# Quetzal

> Kubernetes-native control plane & UI to deploy and manage game servers — in the
> spirit of Pterodactyl/Pelican, **without the Docker layer**.

Quetzal runs game servers **directly on Kubernetes**: the panel talks to the
Kubernetes API, and the kubelet *is* the daemon (no per-node "Wings"). It is
**game-agnostic** — anything you can describe with a template (or import as a
Pterodactyl **egg**) can be deployed.

## Status

🚧 Early development. **Phases 0–5 implemented** (foundations, MVP, networking &
observability, scheduled tasks + backups, multi-tenant access control,
hibernation + egg install scripts); see [ROADMAP](#roadmap).

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
- **Hibernation**: opt-in scale-to-zero for idle servers (no player connections),
  woken on demand — saves resources for dormant servers.
- **Install scripts**: egg install scripts run as a one-time init container, so
  install-based eggs work out of the box.
- **Egg-compatible**: import existing Pterodactyl/Pelican eggs to ease migration.
- **Secure by default**: namespace-per-server, NetworkPolicy, hardened
  securityContext, secrets kept out of the DB in clear text.
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
  per-user quotas, audit log, API keys. _Deferred to later: OIDC/SSO, 2FA/TOTP,
  email/Discord notifications, webhooks._
- ✅ **Phase 5** — Hibernation (scale-to-zero on idle) + egg install scripts.
  _Deferred to later: wake-on-connect proxy, git template sync, sandboxed runtime._
- **Phase 6** — Multi-cluster.

## License

[MIT](./LICENSE).
