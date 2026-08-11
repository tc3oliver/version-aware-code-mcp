# version-aware-code-mcp

Version-aware code intelligence for AI coding agents. Combines branch-aware code
search with structural code graph analysis through MCP.

## Why

Agents search the right code in the wrong version.

Ask an agent to fix a bug in release 2.x and it will happily grep its way into a
file from `main`, or into a checkout that stopped tracking the branch three
merges ago. The snippet looks plausible. The function name matches. Nothing in
the result says which version it came from, so the mistake is invisible until it
ships.

This server makes the version explicit and enforced. Every request names a
context — a repository, a branch, and a revision — and every result is confined
to it:

- Search is restricted to that context's branch.
- The call graph queried is the one built for that context's revision.
- Source is read at that revision, and a mismatch fails the request instead of
  quietly returning the wrong file.
- Every result carries the repository, branch, revision, file, and line it came
  from, so the answer can be checked rather than trusted.

The server does not answer questions. It supplies search, graph, source,
version, and evidence. Your agent does the reasoning.

## Architecture

## Features

- Version-scoped search
- Branch-aware search
- Call graph
- Evidence
- Local-first

## Quick Start

## MCP Configuration

## Context Configuration

## Tools

## How It Works

## Version Correctness

## Provider Model

## Security

## Roadmap

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
