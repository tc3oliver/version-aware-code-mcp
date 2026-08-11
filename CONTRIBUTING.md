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

## Repository layout

```text
cmd/vacmcp/     binary entry point: serve, validate, contexts, doctor, version
server/         MCP server construction and the STDIO and HTTP transports
tools/          the four MCP tool handlers
provider/       SearchProvider / GraphProvider / SourceProvider interfaces
adapters/       the v0.1.0 implementations: zoekt, cbm, git
resolver/       context id to CodeContext, and the fail-closed worktree check
vacctx/         CodeContext, the version scope everything else is written against
config/         configuration loading and validation, plus example.yaml
evidence/       the output contract: context and evidence on every result
vacerr/         the error model and its ten codes
integration/    tests against real engines, including the release gate
internal/       code that must not be imported from outside this module
docker/         Compose stack for vacmcp, Zoekt and CBM
testdata/       fixture generation scripts
```

Exported packages live in the root package hierarchy; implementation that must
not be imported from outside this module goes under `internal/`.

## Layering

Dependencies point one way, and the direction is what keeps version isolation
enforceable in one place:

```text
cmd/vacmcp  ── wires the adapters to the tools; the only place that knows both
    │
tools/      ── resolves a context, calls a provider, returns evidence.Output
    │
    ├── resolver/  the only thing that turns an id into a version scope
    ├── provider/  the three interfaces, in provider/provider.go
    └── evidence/  builds the result; a bare answer is unrepresentable
            │
adapters/   ── implement provider's interfaces; cmd/vacmcp is the only
                non-test package that imports them
```

A tool that imported `adapters/zoekt` would break that. The tools are written
against `provider.SearchProvider`, `provider.GraphProvider` and
`provider.SourceProvider`, so a different engine is a new package under
`adapters/` plus one line in `cmd/vacmcp`, and no change anywhere else. Tests
are the exception and import adapters freely — several of the tool tests run
against a real engine, which is the point of them.

Two rules hold across all of it. Every provider method takes a
`vacctx.CodeContext` and must confine its work to it — the branch for search,
the graph reference for the graph, the revision for source. And a successful
result is built by `evidence.New`, which refuses a context missing any of id,
repository, branch or revision, so a result that cannot say which version it
came from cannot be returned.

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
- Run `make fmt`, `make vet`, `make lint`, `make test` and `make build` before
  opening it. The pull request template asks for each of them, and CI runs the
  same checks against real engines on top.
- Every step of the pipeline is a hard gate. A red run is something to fix, not
  something to re-run.

Commit subjects follow `type(scope): imperative summary` — `feat(tools):`,
`fix(cmd):`, `test(integration):`, `docs:`, `chore:`. The subject says what
changed; the body is for why, which is the part a reader a year from now cannot
reconstruct from the diff.

## License

Contributions are accepted under the Apache License 2.0.
