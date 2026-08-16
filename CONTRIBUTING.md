# Contributing

## Requirements

- Go 1.26 or newer
- [golangci-lint](https://golangci-lint.run/) 2.x

## Build, test, lint

```bash
make build            # go build -o bin/vacmcp ./cmd/vacmcp
make test             # go test ./...
make test-integration # go test -tags=integration ./...
make lint             # golangci-lint run --build-tags=integration
make fmt              # gofmt -l -w .
make vet              # go vet ./...
```

### The two test runs

A test that needs a real Zoekt or a real codebase-memory-mcp to say anything is
behind `//go:build integration`, in a file named `*_integration_test.go` beside
the one holding the rest of its package's tests. So there are two runs:

| Run | What it covers | Needs |
| --- | --- | --- |
| `make test` | everything decided by this module alone: path traversal, state transitions, atomic writes, revision immutability, CLI arguments, lock semantics | git |
| `make test-integration` | that, plus every test that queries a real engine: Zoekt branch isolation, CBM project selection, `trace_calls` version isolation, the managed lifecycle end to end | git, Zoekt, CBM, a prepared fixture |

The tag adds tests to the run rather than replacing any, so the second is a
superset of the first and nothing is only ever run one way.

When you add a test, the question to answer is whether a real engine is what
makes its assertion mean something. If yes, it goes in the package's
`*_integration_test.go`; if the invariant is this module's own, it goes in the
plain `_test.go` and stays in the fast run. Do not reach for a mock engine to
avoid the tag: correctness that involves an external engine is verified against
the real engine, and a mock result does not satisfy a release gate. The tag
moves when a test runs, never what it is allowed to talk to.

The top-level `integration/` package is not tagged. It is the release gate, and
a gate that runs nothing because a tag was forgotten is worse than a slow one,
so it stays visible to a plain `go test` and skips itself when the fixture has
not been built.

## Repository layout

```text
cmd/vacmcp/     binary entry point: serve, validate, contexts, doctor, version
server/         MCP server construction and the STDIO and HTTP transports
tools/          the four MCP tool handlers, a thin adapter over engine
engine/         the four queries as a Go package, with no MCP in them
managed/        the repository and context lifecycle the CLI commands drive
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
cmd/vacmcp  ── wires the adapters into the engine and the engine to the tools;
                the only place that knows both
    │
tools/      ── MCP arguments in, JSON out; no resolution, no provider call and
                no version check of its own
    │
engine/     ── resolves a context, calls a provider, returns evidence.Output
    │
    ├── resolver/  the only thing that turns an id into a version scope
    ├── provider/  the three interfaces, in provider/provider.go
    └── evidence/  builds the result; a bare answer is unrepresentable
            │
adapters/   ── implement provider's interfaces; cmd/vacmcp is the only
                non-test package that imports them
```

An engine or a tool that imported `adapters/zoekt` would break that. `engine/`
is written against `provider.SearchProvider`, `provider.GraphProvider` and
`provider.SourceProvider`, so a different backend is a new package under
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
| `cbm-data/` | CBM's own store for the three graphs below |
| `ambiguous/` | two packages declaring one function name, indexed as `vacmcp-demo-ambiguous` |
| `config.yaml` | repositories and contexts naming both, with resolved revisions |

`cbm-data/` exists because CBM has no `--data-dir` flag; it keys its store off
`CBM_CACHE_DIR`, falling back to the developer's global
`~/.cache/codebase-memory-mcp` when unset. The script and `make
test-integration` both set it to `testdata/fixture/cbm-data`, so the three
graphs below live beside the rest of the fixture instead of accumulating in
that global directory — re-running `prepare-fixture.sh` wipes them with
everything else, the same reset a stale Zoekt index already gets. If you keep
a CBM daemon running locally for the startup-cost speedup CI's comment
describes, point it at the same directory:
`CBM_CACHE_DIR=testdata/fixture/cbm-data codebase-memory-mcp daemon start` —
running the tests without a daemon warm under that directory still passes,
just slower per `cli` call, the same tradeoff CI's comment describes.

CBM keeps only one cache directory active per account at a time: a daemon
warmed under `testdata/fixture/cbm-data` refuses a concurrent command that
asks for a different one (including the default), erroring rather than
running against the wrong store. `codebase-memory-mcp daemon stop` before
switching directories, including before using CBM against something outside
this repository.

Each release is indexed into its own CBM project, named after the `graph_ref`
of its context. Two versions sharing one project would trace calls against the
other version's graph. `vacmcp-demo-ambiguous` is no release of the repository
— it is the duplicated symbol `trace_calls` has to report both candidates for,
built here rather than by each test that asks about one.

Tests reach the result through `demorepo.Prepared`, which skips when the
fixture has not been built. Nothing `make test` runs asks for it — that is what
the build tag is for — so the fast run passes without Zoekt and CBM installed
at all, and `make test-integration` is the run that needs both. Verify the
fixture directly, never through `vacmcp` — otherwise a broken fixture looks
like a broken adapter:

```bash
zoekt -index_dir testdata/fixture/zoekt-index 'NewHandler branch:release/v2'
codebase-memory-mcp cli trace_path --project vacmcp-demo-v2 \
	--function-name Process --direction outbound --depth 3
```

## CI pipeline

CI is three tiers, each a reusable workflow of its own, so a pull request waits
for the checks that can fail it and not for the ones whose answer changes on a
different clock:

| Tier | Workflow | Runs on |
| --- | --- | --- |
| Fast CI | `.github/workflows/ci-fast.yml` | every pull request, every push to `main`, on demand, and every release |
| Real Engine Gate | `.github/workflows/engine-gate.yml` | the same four |
| Full Stress Gate | `.github/workflows/full-gate.yml` | nightly at 21:17 UTC, every release, and on demand from the Actions tab |

`.github/workflows/ci.yml` is what the first two are called from — it runs no
step itself — and `.github/workflows/release.yml` calls `ci.yml` plus the full
gate. A merge is a commit nobody ran the chain against, which is why `main` is
checked again after one.

Fast CI installs no engine, which is the point of it: nothing it runs needs an
index or a graph, so it answers in a minute or two.

| Step | Command |
| --- | --- |
| `go fmt` | `gofmt -l .`, non-empty output fails |
| `go vet` | `make vet` |
| staticcheck | `make lint` (golangci-lint, staticcheck enabled) |
| unit test | `go test -v` on everything but `integration/` |
| race test | `go test -race` on the same packages |
| license check | `.github/license-check.sh` |
| build | `make build` |

The Real Engine Gate builds the fixture first, then runs the half of the suite
that a real Zoekt and a real CBM are the whole point of:

| Step | Command |
| --- | --- |
| real engine test | `go test -tags=integration -v` on everything but `integration/` |
| integration test | `go test -v ./integration/...` |
| MCP conformance | `.github/mcp-conformance.sh` |

The Full Stress Gate is the only run where the tag and `-race` meet — Fast CI
races the build without the engines, the engine gate drives the engines without
the detector — and it is where `govulncheck` lives, because its answer changes
when its database does rather than when the tree does:

| Step | Command |
| --- | --- |
| race test against the real engines | `go test -tags=integration -race -v ./...` |
| `govulncheck` | `govulncheck ./...` |
| run summary | parses the test log into the run's page |

`./...` here has nothing filtered out of it, unlike the two tiers above, so this
run is a superset of both: every test either of them compiles, plus the tagged
ones, plus `integration/`'s release gates, all under `-race`. The nightly is
what makes that worth having — the crash-injection and concurrency cases and
`govulncheck` all answer questions that can change while the tree does not.

The summary step is the one exception to the rule below: it writes the run's
duration, its ten slowest tests, the engine versions it ran against and each
release gate's result to the run's page. It carries `if: always()` because the
failed run is the one worth reading, it reports rather than gates, and every
command in it exits 0 so it cannot fail a run of its own accord.

Every other step is a hard gate: none carries `continue-on-error` or
`if: always()`, so a job stops at the first failure, and `release` needs all
three tiers.

The `-v` test steps also run `.github/assert-no-skips.sh` over their output. A
test that skips is a test that verified nothing, and every skip there means a
missing engine or an unbuilt fixture — on an unprepared checkout `go test ./...`
reports ten skips and still exits 0. CI treats that as a failure.

Zoekt and codebase-memory-mcp are pinned to exact versions in
`.github/actions/prepare-engines/action.yml`, the composite action both
engine-running tiers share, and the CBM archive is checked against the SHA-256
recorded there as well as against its release's `checksums.txt`. It is one file
rather than one copy per tier so that a bump cannot be applied to half of CI.
Do not replace either pin with a floating version.

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

## Releases

A release is a tag. Pushing one that starts with `v` runs
`.github/workflows/release.yml`, which does two things in order:

1. `verify` calls `ci.yml` and `full` calls `full-gate.yml` — the same
   workflows a pull request and the Actions tab run, not a second copy of them
   that can drift. That is all three tiers; the nightly runs the third one too,
   but a release is the only thing that waits on its answer. One red step in
   any of them and the workflow stops: no archives, no GitHub release.
2. `release` runs `.github/release-build.sh <tag> dist` and publishes what it
   produced, titled with the tag.

`release-build.sh` cross-compiles the five platforms, packs each with `LICENSE`,
`THIRD_PARTY_NOTICES.md` and `README.md`, and writes `SHA256SUMS` over the
archives:

```text
vacmcp_<tag>_linux_amd64.tar.gz    vacmcp_<tag>_darwin_amd64.tar.gz
vacmcp_<tag>_linux_arm64.tar.gz    vacmcp_<tag>_darwin_arm64.tar.gz
vacmcp_<tag>_windows_amd64.zip     SHA256SUMS
```

The version is linked in rather than written down: `cmd/vacmcp` holds
`0.0.0-dev`, and a release build passes `-ldflags "-X main.version=<tag>"`. The
script runs the one binary it built for its own platform and fails if that
binary does not report the version it was asked for, so renaming the variable
stops the release instead of shipping five archives that all call themselves
development builds. `TestVersionComesFromTheBuild` asks the same question from
the other end on every CI run.

The version is an argument, so a release can be rehearsed without tagging
anything:

```bash
.github/release-build.sh v0.1.0-test /tmp/dist
```

A tag carrying a pre-release part — `v0.1.0-rc.1` — is published as a GitHub
pre-release, so it does not become the release a first-time visitor downloads.

## Pull requests

- Keep a pull request to one reviewable change.
- Include tests for behavior changes.
- Update documentation affected by the change in the same pull request.
- Run `make fmt`, `make vet`, `make lint`, `make test` and `make build` before
  opening it, and `make test-integration` too if you have the engines and the
  fixture. The pull request template asks for each of them, and CI runs every
  one of them against real engines whether or not you did.
- Every step of the pipeline is a hard gate. A red run is something to fix, not
  something to re-run.

Commit subjects follow `type(scope): imperative summary` — `feat(tools):`,
`fix(cmd):`, `test(integration):`, `docs:`, `chore:`. The subject says what
changed; the body is for why, which is the part a reader a year from now cannot
reconstruct from the diff.

## License

Contributions are accepted under the Apache License 2.0.
