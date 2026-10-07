# Contributing to Quetzal

Thanks for helping. Bug reports, eggs that don't import cleanly, fixes and
features are all welcome. Everyone taking part follows the
[code of conduct](CODE_OF_CONDUCT.md).

- **A bug or a broken egg**: open an issue with the matching template.
- **A question**: ask in [Discussions](https://github.com/Lolozini/quetzal/discussions).
- **A vulnerability**: don't open an issue. Report it privately; see the
  [security policy](SECURITY.md).
- **A larger change** (a new feature, a new dependency, anything that touches
  the database schema or the chart's values): open an issue or a discussion
  first, so we agree on the approach before you write the code.

## Development setup

You need:

- Go, at the version in `go.mod`;
- Node.js 22;
- Helm 3, for chart work;
- a disposable Kubernetes cluster for anything that creates servers.
  [kind](https://kind.sigs.k8s.io) works: `make e2e-kind-up` creates one.

> [!WARNING]
> The controller acts on the cluster of your **current kubeconfig context**.
> Point it at a throwaway cluster, never at one that runs real servers.

Run the three processes in separate terminals from the repository root. They
share an SQLite file, `quetzal.db`, and one persistent encryption key. Generate
that key **once**, not independently in each terminal:

```sh
install -d -m 700 "$HOME/.config/quetzal"
test -f "$HOME/.config/quetzal/dev.env" || (
  umask 077
  printf 'export QUETZAL_SECRET_KEY=%s\n' "$(openssl rand -base64 32)" \
    > "$HOME/.config/quetzal/dev.env"
)
```

Build the helper image and make it available to your disposable cluster.
For the kind cluster created by `make e2e-kind-up`:

```sh
docker build -t quetzal:dev .
kind load docker-image quetzal:dev --name quetzal-e2e
```

In **each** terminal, source the same key file before starting its process:

```sh
. "$HOME/.config/quetzal/dev.env"

# 1. The API server, on :8080
QUETZAL_DEV_ORIGIN=true go run ./cmd/apiserver

# 2. The controller, against your current kubeconfig context
QUETZAL_IMAGE=quetzal:dev go run ./cmd/controller

# 3. The web UI with hot reload, on :5173; it proxies /api to :8080
npm --prefix web ci
npm --prefix web run dev
```

Open http://localhost:5173 and create the admin account.

## Before you open a pull request

The CI runs the same checks as these commands:

```sh
make lint                   # gofmt + go vet
make test                   # unit tests, with the race detector
make test-postgres          # the same on PostgreSQL, if you touched the store
make test-mariadb           # the database dump/load/import scripts, in Docker
npm --prefix web run build  # type check, translation coverage, production build
helm lint deploy/quetzal    # if you touched the chart
```

`make test-postgres` needs a PostgreSQL server to create its test databases
on, named by `QUETZAL_TEST_POSTGRES`; the Makefile shows a one-line one in
Docker. `make test-mariadb` needs Docker: it starts a MariaDB and runs the
scripts in the same image (`MARIADB=mariadb:11.4 MARIADB_CLIENT=mariadb:12.3 make test-mariadb` for
an older server dumped with the panel's client). `make e2e` runs the end-to-end suite against the cluster in your
kubeconfig (the CI uses kind).

- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org):
  `fix(console): …`, `feat(web): …`, `docs: …`. Explain the why in the body.
- **The changelog**: a change that users will notice gets an entry under
  `## [Unreleased]` in the [changelog](CHANGELOG.md), written for someone who is
  upgrading. The release notes are taken from it.
- **UI text**: every string goes through `t()`. Add its French translation in
  `web/src/locales/fr.ts`; the build fails on a missing key.
- **The look**: use the CSS variables at the top of `web/src/styles.css` rather
  than hard-coded colours; the [brand guide](docs/brand/README.md) describes
  the rest.
- **Tests**: a fix comes with a test that fails without it, when the code allows it.

## License

Quetzal is licensed under the [AGPL-3.0-or-later](LICENSE). By contributing, you
agree that your contribution is licensed under the same terms.
