# Security policy

Quetzal runs with broad rights on the clusters it manages, and it hosts servers
for people who don't administer those clusters. Security reports matter here.

## Supported versions

Security fixes land on `main` and ship as a patch release of the latest minor
version. Before 1.0, older minor versions don't get backports: upgrade to the
[latest release](https://github.com/Lolozini/quetzal/releases/latest).

## Reporting a vulnerability

**Don't open a public issue.** Report it privately on GitHub:
[Security → Report a vulnerability](https://github.com/Lolozini/quetzal/security/advisories/new).

Please include:

- the Quetzal version (in the panel's footer, or from `GET /api/version`);
- how it is deployed (the Helm chart and its non-default values, the Kubernetes
  distribution and version);
- the steps to reproduce, or a proof of concept;
- the impact as you see it.

What happens next:

1. You get an acknowledgement within a week.
2. The fix is prepared in a private security advisory. You're welcome to review it.
3. It ships in a patch release, with an entry under *Security* in the
   [changelog](CHANGELOG.md), and the advisory is published. You're credited
   unless you'd rather not be.

## Scope

In scope: the API server and web UI, the controller, the SFTP server, the
activator, the Helm chart in `deploy/quetzal`, and the images published at
`ghcr.io/lolozini/quetzal`.

An administrator with full rights is trusted with the cluster. Escalation from
a scoped admin role, a server owner, a subuser, an API key or a game server is
in scope. So is reaching the cluster's network from a game server.

Out of scope: vulnerabilities in game server images and in third-party eggs you
import. Report those to their authors.

## Verifying the images

From the release after 0.3.1 on, the images are signed with
[cosign](https://github.com/sigstore/cosign), keylessly, by the GitHub Actions
workflow that built them, and so is the Helm chart attached to each release.
Each image also carries a software bill of materials (SBOM) and its build
provenance.

```sh
cosign verify ghcr.io/lolozini/quetzal:<version> \
  --certificate-identity-regexp '^https://github\.com/Lolozini/quetzal/\.github/workflows/(release|ci)\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# The chart, with the .sigstore.json bundle downloaded next to it from the release
cosign verify-blob quetzal-<version>.tgz --bundle quetzal-<version>.tgz.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/Lolozini/quetzal/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

# The image's SBOM, in SPDX
docker buildx imagetools inspect ghcr.io/lolozini/quetzal:<version> --format '{{ json .SBOM }}'
```
