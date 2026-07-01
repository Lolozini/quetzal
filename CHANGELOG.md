# Changelog

All notable changes to Quetzal are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) (pre-1.0: minor
releases may include breaking changes).

## [Unreleased]

### Added

- **Port suggestions for imported eggs**: the create form now pre-fills the
  per-server ports editor from the template's port-like variables (`QUERY_PORT`,
  `RCON_PORT`, `STEAM_PORT`…, detected by name + a valid numeric default), so a
  server imported from a Pterodactyl egg starts with its extra ports already
  filled in instead of a blank row. The editor gained a per-row "primary"
  selector to pick the port players connect to (the main game port is usually the
  allocation, not a variable). Exposed as `Template.suggestedPorts` in the API.
- **Per-server ports** can be defined at creation for templates that declare none
  (imported Pterodactyl eggs allocate ports per server, not in the egg): a small
  ports editor (number + TCP/UDP, first is primary), and the network-exposure
  selector appears once a port is defined.
- **Minecraft EULA acceptance** for templates that declare the `eula` egg
  feature: an "I accept the Minecraft EULA" toggle on create/settings; when
  accepted, the controller renders `eula.txt=true` into the data volume at
  startup (and writes nothing otherwise, so the server keeps asking). Mirrors
  Pterodactyl's `eula` feature without modifying the imported egg.

### Changed

- **Storage is now always a PVC.** Removed the user-selectable `hostPath` storage
  type: it let a tenant mount arbitrary node paths (a host-escape vector for the
  untrusted code game pods run, and disallowed by the baseline/restricted Pod
  Security Standards) and had no scheduling affinity, which broke rescheduling and
  cross-cluster transfer. Single-node setups use a local provisioner (e.g.
  local-path) as the storageClass. **Breaking:** servers created with `hostPath`
  storage must be recreated.
- **storageClass is admin-controlled per cluster**, chosen from a dropdown of the
  cluster's actual storage classes (Admin → Clusters), instead of a free-text
  field at server creation. New servers inherit the target cluster's default.
- **Files and SFTP now work whether the server is running or stopped**, with no
  startup latency. A small always-on **data-manager pod** mounts the data volume
  permanently and hosts file operations and the SFTP server; the game pod is
  co-located with it (podAffinity) so both share the ReadWriteOnce volume on one
  node. Replaces the previous on-demand maintenance pod (which only ran while
  stopped) and the SFTP sidecar (which only ran while running). During a restore
  the data-manager is scaled down so the restore gets exclusive volume access.

### Fixed

- Egg startup commands now run under **bash** (falling back to sh), matching how
  Pterodactyl runs them. Many eggs use bash-only syntax — `[[ ]]` (Forge),
  process substitution and `trap`/`wait` for graceful stop and log filtering
  (Valheim and most SteamCMD eggs) — which dash (`/bin/sh`) rejected with
  "[[: not found" or "Syntax error: redirection unexpected", crashing the server
  on boot. The resolved command is passed as a positional arg, so it's parsed
  verbatim with no re-quoting.
- Imported eggs that size the JVM from `{{SERVER_MEMORY}}` (and friends) now
  start: Quetzal injects the full set of **Wings-provided globals** that eggs
  assume but never declare as variables, matching Wings' contract — `SERVER_MEMORY`
  (the memory limit in MiB), `SERVER_PORT` (the primary allocation), `SERVER_IP`
  (`0.0.0.0`), `TZ` (UTC) and `STARTUP` (the resolved invocation) — into the game,
  install and config-render containers. Previously `-Xmx{{SERVER_MEMORY}}M`
  expanded to `-Xmx M` and the JVM refused to start (affected ~25 of the official
  Minecraft eggs, e.g. Fabric, Spigot, Forge, the Technic packs and every proxy;
  Paper/Purpur were spared as they use `-XX:MaxRAMPercentage`).
- `config.files` placeholders now resolve `{{config.docker.interface}}` to the
  bind-all address (`0.0.0.0`), like `{{server.build.default.ip}}`. Wings
  substitutes its Docker bridge IP there; in Kubernetes each server has its own
  Service, so binding to all interfaces is correct. Previously the literal
  placeholder was written into the config and broke proxy binds (Waterfall,
  Travertine).
- Imported egg **install scripts that need root** now run. About half the
  official Minecraft eggs `apt-get`/`apk add` build dependencies in their
  installer image (eclipse-temurin, ghcr.io/ptero-eggs/installers), which the
  non-root runtime user can't do, so the install failed and no server jar was
  produced. The install init container now runs as root (overriding the pod's
  non-root default) and then chowns the data volume to the runtime user — the
  Wings model — which also makes the data readable by the non-root game pod on
  local-path (where `fsGroup` is a no-op).
- Signal-based stop commands are honoured. Pterodactyl encodes some stops as a
  caret token (`^C` = SIGINT, used by a few proxies/limbos); Quetzal no longer
  writes the literal `^C` to the console (a no-op) but stops the server via pod
  termination (SIGTERM + grace), which those servers handle as a clean shutdown.
- Imported eggs no longer run as **root**: a template that declares no
  securityContext (eggs don't) now defaults to a non-root uid (988, the
  yolks/Pterodactyl "container" user) with a matching fsGroup so the data volume
  stays writable. Built-in templates keep their own context.
- Imported eggs install and run correctly: their install script now (a) runs
  under a POSIX shell even when the egg export uses Windows (CRLF) line endings,
  and (b) receives the server's variables in its environment (e.g.
  `${SERVER_JARFILE}`, `${MINECRAFT_VERSION}`), as Pterodactyl runs it — without
  them an egg's installer downloaded nothing. And (c) the game container now runs
  in the data directory, so egg startup commands using relative paths (e.g.
  `java -jar server.jar`) find their files.
- Multi-node co-location for the ReadWriteOnce data volume: the data-manager now
  has a preferred affinity back to the game pod (so a data-manager-only reschedule
  returns to the volume's node), and backup Jobs co-locate with the data-manager
  (backups mount the volume while it's still held); restore Jobs run only after
  the volume is free. No effect on single-node clusters.
- Wake-on-connect: the activator pod failed to start (`container has runAsNonRoot
  and image has non-numeric user (nonroot)`) because it ran the distroless Quetzal
  image without a numeric `runAsUser`; pinned to uid 65532. Its wake callback to
  the apiserver also timed out under CNIs that enforce NetworkPolicy: the default
  policy now applies only to the untrusted workload pods (game + data-manager),
  not the Quetzal-controlled activator/backup Job, which need cluster/external
  egress the generic policy can't express.
- The per-server SFTP NodePort is now drawn from Quetzal's managed node-port pool
  (the same pool as the game ports) instead of Kubernetes' auto-assignment, so the
  two allocators can no longer pick the same port and conflict. Released back to
  the pool when SFTP is disabled or the server is deleted.
- Reject implausibly small memory limits (e.g. `4`, meaning 4 bytes for a
  missing unit) instead of producing a pod stuck on a cryptic cgroup error;
  resources are now validated on create as well as update.
- Server creation no longer fails with `variable "TYPE" is not editable` when a
  template has fixed (non-editable) variables.

## [0.1.0] - 2026-06-25

Initial public release — a Kubernetes-native control plane and web UI for hosting
game servers, with no per-node agent (Kubernetes itself runs the workloads).

### Added

- **Server lifecycle**: create from templates, power start/stop/restart/kill with
  graceful stop, editable startup variables and CPU/RAM limits, and reinstall
  (optional data wipe) guarded by an install-generation marker.
- **Console & files**: live console (log stream + `attach` stdin, no sidecar); a
  web file manager (browse/edit/upload/rename/delete, folder `.tar.gz` download,
  archive upload-and-extract) that also works while the server is stopped via an
  on-demand maintenance pod; opt-in per-server **SFTP** keyed by users' SSH keys.
- **Networking**: ClusterIP / NodePort (managed port pool) / LoadBalancer, TCP and
  UDP, real client IP by default, provider-neutral Service annotations.
- **Data & backups**: backups/restore to any S3-compatible target via restic
  (dedup, encryption, retention); keep-or-destroy data on delete; per-server
  MySQL/MariaDB provisioning against external hosts or a managed in-cluster MariaDB.
- **Automation**: cron **schedule task-chains** (power/command/backup with delays
  and continue-on-failure); **notifications** to Discord, signed webhooks, or
  email/SMTP via a durable event outbox.
- **Multi-tenant & auth**: per-server ownership and subusers, **granular admin
  roles**, admin suspend, per-user quotas, append-only audit log, API keys,
  **2FA (TOTP)** with recovery codes, and self-service password reset by email.
- **Hibernation**: scale-to-zero on idle with **wake-on-connect** in drop (TCP)
  and proxy (TCP+UDP) modes.
- **Multi-cluster**: kubeconfig-based cluster registry (encrypted), per-server
  deploy target, and **server transfer between clusters** via the backup target.
- **Egg compatibility**: import Pterodactyl/Pelican eggs (variables, startup,
  install scripts, `config.files` rendering); built-in templates for Minecraft
  (Paper and CurseForge modpacks), Valheim, and a generic process.
- **Platform**: DB-as-source-of-truth reconciler with no CRDs; SQLite or Postgres;
  leader-elected controller; OpenAPI spec + `/api/docs`; Prometheus `/metrics`;
  build-info `/api/version`; internationalized UI (English + French); version
  stamping and a tag-driven release workflow (image + Helm chart + GitHub Release).
- **Secure by default**: namespace-per-server, deny-by-default NetworkPolicy,
  hardened `securityContext`, no ServiceAccount token in game pods, per-namespace
  ResourceQuota, secrets encrypted at rest, login/2FA rate-limiting, and CSRF
  protection.

### Notes

- Licensed under **AGPL-3.0-or-later**.

[Unreleased]: https://github.com/lolozini/quetzal/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/lolozini/quetzal/releases/tag/v0.1.0
