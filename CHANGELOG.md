# Changelog

All notable changes to Quetzal are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once it reaches
its first tagged release.

## [Unreleased]

### Added

- **Version stamping & releases.** Binaries now embed build info (version,
  commit, date); `quetzal-apiserver version` / `quetzal-controller version`
  print it, the panel footer shows it, and `GET /api/version` exposes it. A
  `release` GitHub Actions workflow builds a version-stamped image, packages the
  Helm chart, and publishes a GitHub Release on every `v*` tag. See
  [docs/INSTALL.md](docs/INSTALL.md) and [docs/UPGRADE.md](docs/UPGRADE.md).
- **Offline file management.** The file manager now works while a server is
  stopped, via an on-demand ephemeral maintenance pod that mounts the data
  volume (suspended servers stay locked for non-admins).
- **Minecraft CurseForge modpack template** (`minecraft-curseforge`), driving the
  itzg image's `AUTO_CURSEFORGE` installer.

[Unreleased]: https://github.com/lolozini/quetzal/commits/main
