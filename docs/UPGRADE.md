# Upgrading Quetzal

Quetzal keeps the database as its source of truth, so upgrades are normally a
matter of rolling the two Deployments to a newer image. Schema migrations run
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
helm upgrade quetzal ./deploy/quetzal \
  --namespace quetzal \
  --reuse-values \
  --set image.tag=vX.Y.Z
```

- `--reuse-values` keeps your existing settings; override only what changes.
- The generated `QUETZAL_SECRET_KEY` is reused across upgrades (do not rotate it
  unintentionally — existing encrypted values would become unreadable).
- A `migrate`-only init container applies schema migrations before the new
  apiserver/controller start, so the two Deployments never race on the schema.

## Verify

```sh
kubectl -n quetzal rollout status deploy/quetzal-apiserver
kubectl -n quetzal rollout status deploy/quetzal-controller
curl https://<panel>/api/version    # should report the new version
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
