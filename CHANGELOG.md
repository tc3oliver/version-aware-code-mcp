# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

Embeddable core: the query plane and the management plane are Go packages that
can be called directly, and the MCP server is one caller of them rather than the
only way in.

### Added

- `engine`: the four queries — `ListContexts`, `SearchCode`, `TraceCalls`,
  `GetCode` — as a package with no MCP, no JSON-RPC and no HTTP in it, so "this
  query ran in that version and no other" is a function call rather than a
  server test. `engine.New(contexts, search, graph, source)` builds one from a
  `ContextSource` and the three providers, any of which may be nil, and
  `Close` releases those that implement `io.Closer` and leaves those that do
  not, which is how a caller keeps ownership of a provider it shares. A request
  names its scope with a context id and nothing else, and every result type has
  unexported fields, so a result that carries its version context and its
  evidence is the only kind that exists.
- `managed`: `RepositoryManager` and `ContextManager` as a public API over a
  data directory, so a program embedding vacmcp builds the same installation
  `vacmcp repo` and `vacmcp context` do rather than a second one that drifts
  from it. It reports domain facts — a name, an id, a state, a revision — and no
  on-disk layout, record format or lock.
- `examples/embed`: a complete program embedding the engine. Its context source
  and search provider are stand-ins, so it runs with no Zoekt, no
  codebase-memory-mcp and no checkout, and CI builds and runs it on every pull
  request.
- README's Embedding Guide and Custom Provider Guide: constructing and closing
  an Engine, the ownership contract `Close` implements, and the four interfaces
  — `ContextSource`, `SearchProvider`, `GraphProvider`, `SourceProvider` — a
  different backend implements. These packages are a **supported embedding
  API**: documented, tested and meant to be used from outside this repository.
  That is not a permanent backward-compatibility guarantee, which remains
  v1.0.0's; until then a breaking change is possible and arrives with a release
  note.

- `server.Handler`: the Streamable HTTP handler `ServeHTTP` mounts, exported so
  a program embedding vacmcp can serve it on a mux of its own.

### Changed

- `tools` is a thin adapter over `engine`: it maps MCP arguments in and JSON
  out, and every resolution, check and provider call happens below it. The four
  tools answer exactly as before — same arguments, same output shape, same error
  codes.
- A client that gives up mid-call now stops the work it started. Over Streamable
  HTTP the in-flight call is tied to the POST carrying it, so an abandoned
  `search_code`, `trace_calls` or `get_code` unwinds at the provider instead of
  running to completion against Zoekt, codebase-memory-mcp and git to produce an
  answer nobody will read. STDIO already did this through the protocol's
  `notifications/cancelled`; the stateless HTTP transport has no session for
  that notification to arrive on.

## [0.2.0] - 2026-08-14

Managed Repository & Context Lifecycle: onboarding a repository no longer means
cloning, indexing, checking out and writing a configuration file by hand.

### Added

- Repository lifecycle: `vacmcp repo add/list/status/sync/remove` clones a
  repository into a data directory and keeps its refs fetched. It is
  forge-neutral — plain `git`, no host API — and holds no credential of its own:
  authentication is system git's, and a URL with a secret embedded in it is
  refused rather than stored. `repo sync` only fetches, and never moves a
  revision an existing context is pinned to.
- Context lifecycle: `vacmcp context create/list/status/verify/retry/remove`
  resolves a ref once and pins the full commit SHA. A context is immutable —
  another revision is another context — and reaches `READY` only by passing
  every stage of `CREATING` → `RESOLVING` → `PREPARING_SOURCE` →
  `INDEXING_SEARCH` → `INDEXING_GRAPH` → `VERIFYING`, with `context retry`
  rebuilding one that stopped anywhere along it. `context remove` records
  `REMOVING` before it takes anything apart, so a removal that is interrupted
  leaves a context the query plane has already stopped serving rather than one
  still claiming `READY` over artifacts that are gone, and running the same
  command again finishes it.
- Automatic search and graph provisioning: creating a context builds its Zoekt
  index and its codebase-memory-mcp graph, and removing one takes them away.
  `zoekt-git-index`, `git worktree` and `index_repository` are no longer
  commands anyone runs by hand.
- `vacmcp serve --managed [--data-dir DIR]` serves the contexts a data directory
  has ready, and `vacmcp doctor --managed` reports on its repositories and
  contexts. Serving stays local-only: it reaches no remote, and the repository
  and context commands are not exposed as MCP tools, so a connected agent cannot
  make the server clone or delete anything. A context that is not `READY` is
  absent from the query plane, answering the existing `CONTEXT_NOT_FOUND`.
- A managed server serves the contexts it read when it started, for as long as
  it runs. `context create`, `context retry`, `context remove` and `repo remove`
  are refused while one is running rather than changing what it is serving
  underneath it: stop the server, run the command, start it again. `repo sync`
  needs no restart — a fetch moves no pinned revision — and reading a data
  directory is never refused, so `context list/status/verify`, `repo
  list/status` and `doctor --managed` still work while a server is up.
- Per-repository locking, so operations on different repositories run in
  parallel while a sync, a create and a remove on one repository serialise, and
  a data-directory lock a managed server holds for its whole run.
- `integration/managed_release_gate_test.go`: doc-1 §15's four
  version-correctness checks re-run against contexts the management plane built,
  plus the lifecycle gate — a remote branch that moves after a context was
  created must not change what that context answers.

### Changed

- `vacmcp serve --config FILE` is unchanged and needs no migration. Managed mode
  is additive; a server runs on one mode or the other, and giving both flags is
  refused.

## [0.1.0] - 2026-08-12

### Added

- Project scaffolding: Go module, build/test/lint entry points, and open source
  project files.
- Release pipeline: pushing a `v*` tag runs the full CI chain and, only if it is
  green, publishes archives for linux, macOS and Windows on amd64 and arm64,
  together with `SHA256SUMS`. `.github/release-build.sh` builds them and can be
  run by hand against a test version.
- `vacmcp version` reports the release it was built from. The tag is linked in
  with `-ldflags "-X main.version=<tag>"`; a build without it still reports
  `0.0.0-dev`.
- `adapters/cbm`: a concurrency test covering parallel traces across two
  contexts, and tests for the `cli` fallback path.

### Changed

- CI now runs on pushes to `main` and on demand, as well as on pull requests.
- `trace_calls` holds one codebase-memory-mcp session instead of starting the
  binary again for every query: about 8.6 seconds per trace before, about 50
  milliseconds after. The graph project is still sent with every query, and a
  CBM that cannot serve MCP falls back to `codebase-memory-mcp cli`.
