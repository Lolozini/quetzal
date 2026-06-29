# Changelog

All notable changes to Quetzal are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project follows
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) (pre-1.0: minor
releases may include breaking changes).

## [Unreleased]

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
