# Contributing

## Requirements

- Go 1.26 or newer
- [golangci-lint](https://golangci-lint.run/) 2.x

## Build, test, lint

```bash
make build   # go build -o bin/vacmcp ./cmd/vacmcp
make test    # go test ./...
make lint    # golangci-lint run
make fmt     # gofmt -l -w .
make vet     # go vet ./...
```

Run `make lint` and `make test` before opening a pull request.

## Repository layout

Packages are added as the code that belongs in them lands, so not every
directory below exists yet.

```text
cmd/vacmcp/     binary entry point
server/         MCP server and transports
context/        CodeContext type, resolver, config loading
provider/       SearchProvider / GraphProvider / SourceProvider interfaces
adapters/       provider implementations (zoekt, cbm, git)
tools/          MCP tool handlers
evidence/       evidence and location types
config/         example configuration
testdata/       test fixtures
integration/    integration tests
docker/         Docker Compose setup
```

Exported packages live in the root package hierarchy; implementation that must
not be imported from outside this module goes under `internal/`.

## Versioned demo repository

`testdata/versioned-demo-repo/` is the fixture the version-correctness checks
run against: a git repository with three branches whose `Process()` calls a
different handler per release.

| Branch | `Process()` calls |
| --- | --- |
| `main` | `LegacyHandler()` |
| `release/v1` | `LegacyHandler()` |
| `release/v2` | `NewHandler()` |

It is generated, never committed — a nested `.git` directory cannot be tracked
by this repository, so the path is in `.gitignore`:

```bash
./testdata/gen-versioned-demo-repo.sh
```

The script rebuilds the fixture from scratch on every run and pins the commit
identity and dates, so repeated runs produce the same commit hashes and an
already indexed fixture stays valid. Go tests reach it through
`internal/demorepo`, which generates it on demand and resolves each branch's
revision with `git rev-parse`; never hard-code a commit hash.

### Indexes and graphs

Integration tests query real engines, so the fixture has to be indexed before
they can run. That needs [Zoekt](https://github.com/sourcegraph/zoekt)
(`zoekt-git-index` to build the index and `zoekt-webserver` to serve it, both
on `PATH`) and
[codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp) 0.10.1
or newer (on `PATH`, or pointed at with `CBM_BIN`):

```bash
./testdata/prepare-fixture.sh
```

It wipes and rebuilds everything under `testdata/fixture/`, which is in
`.gitignore` like the repository itself:

| Path | What it holds |
| --- | --- |
| `zoekt-index/` | one index carrying `main`, `release/v1` and `release/v2` |
| `worktrees/` | a checkout per release, the directories CBM indexes |
| `config.yaml` | repositories and contexts naming both, with resolved revisions |

Each release is indexed into its own CBM project, named after the `graph_ref`
of its context. Two versions sharing one project would trace calls against the
other version's graph.

Tests reach the result through `demorepo.Prepared`, which skips when the
fixture has not been built, so `make test` still passes without Zoekt and CBM
installed. Verify the fixture directly, never through `vacmcp` — otherwise a
broken fixture looks like a broken adapter:

```bash
zoekt -index_dir testdata/fixture/zoekt-index 'NewHandler branch:release/v2'
codebase-memory-mcp cli trace_path --project vacmcp-demo-v2 \
	--function-name Process --direction outbound --depth 3
```

## CI pipeline

`.github/workflows/pr.yml` runs on every pull request. It builds the fixture
first, then runs, in order:

| Step | Command |
| --- | --- |
| `go fmt` | `gofmt -l .`, non-empty output fails |
| `go vet` | `make vet` |
| staticcheck | `make lint` (golangci-lint, staticcheck enabled) |
| unit test | `go test -v` on everything but `integration/` |
| race test | `go test -race` on the same packages |
| integration test | `go test -v ./integration/...` |
| MCP conformance | `.github/mcp-conformance.sh` |
| `govulncheck` | `govulncheck ./...` |
| license check | `.github/license-check.sh` |
| build | `make build` |

Every step is a hard gate: none carries `continue-on-error` or `if: always()`,
so the job stops at the first failure.

The two test steps also run `.github/assert-no-skips.sh` over their output. A
test that skips is a test that verified nothing, and every skip here means a
missing engine or an unbuilt fixture — on an unprepared checkout `go test ./...`
reports ten skips and still exits 0. CI treats that as a failure.

Zoekt and codebase-memory-mcp are pinned to exact versions in the workflow's
`env:` block, and the CBM archive is checked against the SHA-256 recorded there
as well as against its release's `checksums.txt`. Do not replace either with a
floating version.

The three scripts run outside CI too, against a fixture prepared as above:

```bash
./.github/mcp-conformance.sh   # MCP Inspector against the built binary
./.github/license-check.sh     # THIRD_PARTY_NOTICES.md vs. the linked modules
```

`license-check.sh` compares the table in `THIRD_PARTY_NOTICES.md` against
`go list -deps ./cmd/vacmcp`, so adding, removing or upgrading a dependency
that reaches the binary means updating that table in the same pull request.

`mcp-conformance.sh` drives the built binary with the MCP Inspector CLI: it
asserts the session negotiated protocol `2026-07-28` through `server/discover`,
and invokes all four tools for real.

## Pull requests

- Keep a pull request to one reviewable change.
- Include tests for behavior changes.
- Update documentation affected by the change in the same pull request.

## License

Contributions are accepted under the Apache License 2.0.
