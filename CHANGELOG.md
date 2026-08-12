# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
