# Contributing

Thanks for contributing to `ovn-network-agent`. This guide covers the
conventions the project (and its release pipeline) depend on.

## Development setup

Requires Go (see the version in [`go.mod`](./go.mod)).

```bash
make build     # build the linux binary
make test      # run the unit tests
make fmt vet   # format and vet
```

## Commit conventions

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org):
a `type: summary` subject, imperative mood, wrapped at 72 characters.

The type feeds [`cliff.toml`](./cliff.toml), which git-cliff uses to
generate the release notes and [`CHANGELOG.md`](./CHANGELOG.md). Recognized
types and the section they land in:

| Type       | Changelog section |
| ---------- | ----------------- |
| `feat`     | Features          |
| `fix`      | Bug Fixes         |
| `perf`     | Performance       |
| `refactor` | Refactor          |
| `docs`     | Documentation     |
| `build`    | Build             |
| `chore`    | Miscellaneous     |
| `ci`       | CI                |
| `test`     | Testing           |

Anything else (for example a bare `agent:` or `config:` subsystem prefix)
lands under **Other** — still recorded, but not semantically grouped, so
prefer a recognized type.

Sign off every commit (`git commit -s`) so it carries a
`Signed-off-by` trailer.

## Generated documentation

`docs/reference/{configuration,cli,metrics}.md` are generated from the
canonical Go declarations. After touching `config.go`, `metrics.go`, or the
agent's flag / env / YAML surface, regenerate them:

```bash
make docs-gen   # or: go generate ./...
```

CI fails a PR when `docs/reference/` is stale, and when
`ovn-network-agent.default` or `ovn-network-agent.yaml.sample` is missing an
option the agent consumes.

## Test tiers

| Tier            | Command                              | Notes |
| --------------- | ------------------------------------ | ----- |
| Unit            | `make test`                          | CI adds `-race` and enforces a coverage floor. |
| Integration     | `make test-integration`              | Linux + root (`CAP_NET_ADMIN`); see [Integration tests](https://osism.github.io/ovn-network-agent/contributing/integration-tests). |
| E2E             | `make e2e-up` then `make e2e-<scenario>` | containerlab lab; see [Containerlab E2E harness](https://osism.github.io/ovn-network-agent/contributing/e2e-tests). |
| Package smoke   | `test/package/smoke.sh`              | Runs on every PR via the Package workflow (see below). |

Run the package smoke test locally the way CI does. Keep the `nfpm` version
below in sync with the pin in [`.github/workflows/`](./.github/workflows) —
CI is the source of truth:

```bash
go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.46.3
for v in ci1 ci2; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -ldflags "-s -w -X main.version=0.0.0~$v" -o ovn-network-agent-linux-amd64 .
  GOARCH=amd64 VERSION="0.0.0~$v" nfpm package --packager deb \
    --target "ovn-network-agent_${v}_amd64.deb"
done
docker run --rm -v "$PWD:/work" -w /work ubuntu:24.04 \
  ./test/package/smoke.sh ovn-network-agent_ci1_amd64.deb ovn-network-agent_ci2_amd64.deb
rm -f ovn-network-agent-linux-amd64 ovn-network-agent_ci*.deb
```

## Documentation site

The docs under `docs/` follow the [Diátaxis](https://diataxis.fr)
framework (tutorials, how-to guides, reference, explanation). Preview them
locally:

```bash
npm install
npm run docs:dev     # serve at http://localhost:5173
npm run docs:build   # static build; also the CI dead-link check
```

## Releases

Release notes and `CHANGELOG.md` are generated from the commit history.
Regenerate the cumulative changelog with:

```bash
git cliff -o CHANGELOG.md
```

## Security

Do not report vulnerabilities through public issues or pull requests.
Follow the private disclosure process in [SECURITY.md](./SECURITY.md).
