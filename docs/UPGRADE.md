# Upgrading Quetzal

Quetzal keeps the database as its source of truth, so upgrades are normally a
matter of rolling its Deployment to a newer image. Schema migrations run
automatically on startup.

## Before you upgrade

1. **Check the [CHANGELOG](../CHANGELOG.md)** for the target version (breaking
   changes, new required settings).
2. **Back up the panel database** — it is the source of truth. For SQLite, snapshot
   the PVC (or copy the file); for PostgreSQL, take a normal dump. Quetzal can also
   back up the panel DB to your configured S3 target.
3. Note your current version: `curl https://<panel>/api/version` (or the panel
   footer).

## Upgrade with Helm

```sh
helm upgrade quetzal \
  https://github.com/lolozini/quetzal/releases/download/vX.Y.Z/quetzal-X.Y.Z.tgz \
  --namespace quetzal \
  --reuse-values
```

The released chart already points at its own image. From a checkout of the
repository instead, pin the image yourself:

```sh
helm upgrade quetzal ./deploy/quetzal \
  --namespace quetzal \
  --reuse-values \
  --set image.tag=vX.Y.Z
```

- `--reuse-values` keeps your existing settings; override only what changes.
- Coming from **0.1.0**: its chart defaulted to an image tag that was never
  published, so an install made from it had `image.tag` set by hand, and
  `--reuse-values` keeps that setting. Pass `--set image.tag=vX.Y.Z`, or
  `--set image.tag=` to follow the chart, or the upgrade stays on the old image.
- The generated `QUETZAL_SECRET_KEY` is reused across upgrades (do not rotate it
  unintentionally — existing encrypted values would become unreadable).
- A `migrate`-only init container applies schema migrations before the new
  apiserver and controller containers start, so the two never race on the
  schema.

## Verify

```sh
# One Deployment, named after the release, holding both containers.
kubectl -n quetzal rollout status deploy/quetzal
curl https://<panel>/api/version    # should report the new commit
```

Game servers are reconciled from the database, so they are re-applied to match
the new controller without manual steps. Running game pods are not restarted by
an upgrade unless their desired spec changed.

## Rolling back

```sh
helm rollback quetzal <REVISION> --namespace quetzal
```

Roll back only to a version whose schema is compatible with your current
database. Migrations are forward-only; if a release introduced an incompatible
schema change, restore the database backup taken before the upgrade.

## Version pinning

Pin a released tag (`vX.Y.Z`) in production rather than `latest`/`main`, so
upgrades are deliberate and reproducible. Releases (image + packaged Helm chart)
are published on the GitHub Releases page.
