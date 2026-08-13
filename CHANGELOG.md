# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Per-repository locking, so operations on different repositories run in
  parallel while a sync, a create and a remove on one repository serialise.
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
