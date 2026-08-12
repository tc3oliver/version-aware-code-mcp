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

vacmcp sits between your agent and the engines that already index your code. It
adds one thing to them: a *context*, an id that names one repository, one
branch, one revision and one call graph. Every tool call takes a context id, and
nothing it returns comes from outside that scope.

```text
  your agent
      │   MCP 2026-07-28, over STDIO or Streamable HTTP
      ▼
   vacmcp ──── list_contexts   search_code   trace_calls   get_code
      │
      │   resolve the context id against the configuration:
      │   repository, branch, revision, graph reference
      │
      ├── SearchProvider ──▶ Zoekt   (local web server, queried over HTTP)
      ├── GraphProvider  ──▶ CBM     (codebase-memory-mcp, local subprocess)
      └── SourceProvider ──▶ git     (local clone, read at the revision)
```

Neither engine is vendored or forked. Zoekt indexes several branches into one
index and picks the version at query time; codebase-memory-mcp holds one graph
per project, so each version gets a project of its own. vacmcp resolves the
context, hands that scope to the provider, and puts the context and the evidence
back on every answer.

## Features

- **Version-scoped search.** A query is confined to the repository and branch of
  the context it names; it cannot widen its own scope.
- **Version-scoped call graph.** A trace runs in the graph built for that
  context's revision, never in another version's.
- **Source read at a revision.** Lines come out of the commit the context
  declares, not out of whatever the working tree is on.
- **Evidence on every result.** Repository, branch, revision, file and line, so
  an answer can be checked instead of trusted.
- **Local-first.** Local Zoekt, local codebase-memory-mcp, local git. No LLM API
  key, no embeddings. See [Security](#security).

## Quick Start

### With Docker

Needs Docker and nothing else. The stack brings up a Zoekt web server, builds a
graph per release and serves vacmcp against a demo repository baked into the
images: `release/v1`'s `Process()` calls `LegacyHandler()`, `release/v2`'s calls
`NewHandler()`, and the two contexts `demo-v1` and `demo-v2` are configured over
them.

```bash
docker compose -f docker/compose.yaml up -d --build
docker compose -f docker/compose.yaml exec vacmcp \
        vacmcp doctor --config /etc/vacmcp/config.yaml
```

vacmcp then serves Streamable HTTP on `http://127.0.0.1:8080`, with the contexts
`demo-v1` and `demo-v2`. The tool call at the end of the next section works
against it with `demo-v2` as the context. Both published ports are bound to
loopback. Tear it down with:

```bash
docker compose -f docker/compose.yaml down -v
```

### With your own repository

You need Go 1.26, git, and the two engines vacmcp queries. Neither engine ships
with this project:

```bash
go install github.com/sourcegraph/zoekt/cmd/zoekt-git-index@latest
go install github.com/sourcegraph/zoekt/cmd/zoekt-webserver@latest
go install github.com/sourcegraph/zoekt/cmd/zoekt@latest          # optional, to inspect the index
```

For the graph, download
[codebase-memory-mcp](https://github.com/DeusData/codebase-memory-mcp/releases)
0.10.1 or newer for your platform and put the binary on your `PATH`.

Install vacmcp. Every release publishes an archive per platform — linux, macOS
and Windows, amd64 and arm64 — next to a `SHA256SUMS` file covering all of
them, on the [releases page](https://github.com/tc3oliver/version-aware-code-mcp/releases):

```bash
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf vacmcp_v0.1.0_linux_amd64.tar.gz
./vacmcp version          # v0.1.0, the release this binary was built from
```

Or build it from source, which reports `0.0.0-dev` because no release built it:

```bash
git clone https://github.com/tc3oliver/version-aware-code-mcp
cd version-aware-code-mcp
make build
```

Index the branches you want to search. One index holds them all:

```bash
zoekt-git-index -index ~/.vacmcp/zoekt-index \
        -branches release/1.x,release/2.x /path/to/backend
```

Build one graph per version. codebase-memory-mcp indexes a directory rather than
a revision, so each version needs a checkout of its own — a worktree does:

```bash
git -C /path/to/backend worktree add --detach /tmp/backend-v1 release/1.x
git -C /path/to/backend worktree add --detach /tmp/backend-v2 release/2.x
codebase-memory-mcp cli index_repository --repo-path /tmp/backend-v1 --name backend-v1
codebase-memory-mcp cli index_repository --repo-path /tmp/backend-v2 --name backend-v2
```

Write a configuration naming both — `config/example.yaml` is a working starting
point, and [Context Configuration](#context-configuration) explains the fields:

```yaml
providers:
  zoekt:
    url: http://127.0.0.1:6070
  cbm:
    command: codebase-memory-mcp

repositories:
  backend:
    path: /path/to/backend

contexts:
  backend-v1:
    repository: backend
    branch: release/1.x
    revision: 8af31e2
    graph_ref: backend-v1
  backend-v2:
    repository: backend
    branch: release/2.x
    revision: 94cb821
    graph_ref: backend-v2
```

`revision` is a commit: `git -C /path/to/backend rev-parse release/1.x` prints
the one to write down.

The `repositories` key has to be the name Zoekt indexed the repository under,
which it derives from the clone's remote and falls back to the directory name
for. Get it wrong and searches come back empty rather than failing, so check it
against the index:

```bash
zoekt -index_dir ~/.vacmcp/zoekt-index -r -l . | head -1   # prints repository/path
```

Start Zoekt, check the setup, and serve:

```bash
zoekt-webserver -index ~/.vacmcp/zoekt-index -listen 127.0.0.1:6070 -rpc &
./bin/vacmcp doctor --config vacmcp.yaml
./bin/vacmcp serve --config vacmcp.yaml
```

`doctor` reports one row per dependency and one per context, and exits non-zero
if any of them did not pass:

```text
MCP SDK       OK    go-sdk v1.7.0, 4 tools
Config        OK    vacmcp.yaml: 1 repositories, 2 contexts
Zoekt         OK    http://127.0.0.1:6070
CBM           OK    0.10.1
Git repo      OK    backend

Contexts
backend-v1    OK    backend release/1.x 68c19fdb264bad80cf220f672f89dc90478a9d66
backend-v2    OK    backend release/2.x 7b05c6aeb7544cc7fd2b69c56c89c9bcfd46a1a2
```

Point your MCP client at the server as below. To check it answers without one,
Streamable HTTP takes a JSON-RPC call over plain HTTP:

```bash
curl -sS http://127.0.0.1:8080 \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_code",
       "arguments":{"context":"backend-v2","query":"NewHandler"}}}'
```

## MCP Configuration

vacmcp speaks MCP `2026-07-28` through the official Go SDK. Streamable HTTP runs
in stateless mode, which is what the protocol version needs: there is no session
and no `initialize` handshake, and a client discovers the server with
`server/discover`. The server advertises tools and nothing else — it sends no log
notifications, so it does not claim a capability it does not have.

STDIO, for a client that runs vacmcp as a subprocess. The shape below is the
common one; use whatever your client's configuration file wants:

```json
{
  "mcpServers": {
    "vacmcp": {
      "command": "/path/to/bin/vacmcp",
      "args": ["serve", "--stdio", "--config", "/path/to/vacmcp.yaml"]
    }
  }
}
```

Streamable HTTP, for a server shared by several clients or run in a container:

```bash
vacmcp serve --config vacmcp.yaml --address 127.0.0.1:8080
```

The listen address is `--address` if you pass it, otherwise `server.address`
from the configuration file, otherwise `127.0.0.1:8080`. The default is loopback
on purpose: this server reads your source, so reaching it from the network is
something you have to ask for.

The other commands take the same configuration:

```text
vacmcp validate --config FILE    load and check the configuration
vacmcp contexts --config FILE    list the configured contexts
vacmcp doctor   --config FILE    check every dependency and context
vacmcp version                   print the version
```

## Context Configuration

A context is the unit of version isolation. It names:

| Field | What it decides |
| --- | --- |
| `repository` | which repository, by its key under `repositories` |
| `branch` | the branch `search_code` is confined to |
| `revision` | the commit `get_code` reads at, and the version the context claims |
| `graph_ref` | the codebase-memory-mcp project `trace_calls` runs in |

Two contexts over one repository is the normal case, and the reason the server
exists: the same file name means different code in `release/1.x` and
`release/2.x`, and every call has to say which one it means. `revision` may be
any revision git resolves, but a commit SHA is what actually pins a version — a
branch name moves.

`graph_ref` is a codebase-memory-mcp project, and a project is one indexed
graph. Two contexts at different revisions may not share one: that would trace
calls against the other version's graph, so the configuration is rejected rather
than loaded. The graph reference is internal — it never appears in tool output.

Parsing is strict. An unknown field, a duplicate key, a context pointing at a
repository that is not declared: each is an error naming the context and the
field, not a setting silently ignored. There are no LLM settings of any kind in
this file, by design.

Check a configuration before serving with it:

```bash
vacmcp validate --config vacmcp.yaml    # vacmcp.yaml: ok, 2 contexts
vacmcp contexts --config vacmcp.yaml    # id, repository, branch, revision
```

## Tools

| Tool | Arguments | Returns |
| --- | --- | --- |
| `list_contexts` | none | every configured context: id, repository, branch, revision |
| `search_code` | `context`, `query` | matches as path, line and snippet, from that context's branch only |
| `trace_calls` | `context`, `symbol`, `direction`, `depth` | call edges as caller, callee, path and line, from that context's graph only |
| `get_code` | `context`, `path`, `start_line`, `end_line` | those lines as they are at that context's revision |

`direction` is `callers` or `callees`; `depth` is 1 to 5. A query is wrapped in
the context's `repo:` and `branch:` filters and otherwise passed to Zoekt as
written, so `file:` and `lang:` work as they do there — and `sym:` does too, if
the index was built on a machine with universal-ctags, which is what puts
symbols into it.

Every successful result carries the context it was answered in and the evidence
backing it:

```json
{
  "content": "func Process(req string) string {\n\treturn NewHandler(req)\n}\n",
  "context": {
    "id": "backend-v2",
    "repository": "backend",
    "branch": "release/2.x",
    "revision": "7b05c6aeb7544cc7fd2b69c56c89c9bcfd46a1a2"
  },
  "end_line": 6,
  "evidence": [
    { "location": { "path": "processor.go", "start_line": 4, "end_line": 6 } }
  ],
  "path": "processor.go",
  "start_line": 4
}
```

That is `get_code` on `processor.go`. The same three lines in `backend-v1` read
`return LegacyHandler(req)`, with that context's revision on the answer.

`list_contexts` is the exception: its payload is the set of contexts, each
already carrying those four fields, and there is no single version to scope it
to. An unconfigured server answers it with an empty list rather than an error.

A failure is one shape too, with a code to branch on:

```json
{ "error": { "code": "CONTEXT_NOT_FOUND", "message": "...", "details": {} } }
```

| Code | Raised when |
| --- | --- |
| `CONTEXT_NOT_FOUND` | the context id is not configured; the server never guesses one |
| `CONTEXT_AMBIGUOUS` | reserved; contexts are keyed by a unique id, so nothing produces it yet |
| `REPOSITORY_NOT_FOUND` | the repository a context names cannot be read here |
| `REVISION_NOT_FOUND` | that repository does not have the revision |
| `SYMBOL_NOT_FOUND` | the graph has no such symbol |
| `SYMBOL_AMBIGUOUS` | several symbols match; the candidates come back instead of a guess |
| `SEARCH_PROVIDER_UNAVAILABLE` | Zoekt cannot be reached or will not serve the query |
| `GRAPH_PROVIDER_UNAVAILABLE` | codebase-memory-mcp cannot be reached, or the graph is not indexed |
| `SOURCE_MISMATCH` | the source is not the revision the context declares |
| `INVALID_ARGUMENT` | an argument is missing or outside its range |

## How It Works

1. **Resolve.** The context id is looked up in the configuration and its
   repository and revision are checked against git. An unknown id stops here as
   `CONTEXT_NOT_FOUND`: no fuzzy match, no default, no "the only configured one".
2. **Scope.** The resolved context — not the caller's arguments — decides where
   the work happens. Search is sent to Zoekt wrapped in a `repo:` and a `branch:`
   filter, and a match Zoekt reports under another repository or branch is
   dropped on the way back. A trace runs against the context's graph, which is
   the only project name the graph adapter ever passes. Source is read out of
   git's object database at the declared commit, so no working tree, and no
   `git checkout`, comes into it.
3. **Fail closed.** Where content cannot be served from the declared revision,
   the call fails with `SOURCE_MISMATCH` carrying both revisions. There is no
   warning-level variant and no way to continue past it.
4. **Cite.** The context and the evidence are attached to the result by the type
   that builds it, so a bare answer is not representable.

Search and graph degrade separately. `search_code` never touches
codebase-memory-mcp, so an absent or unindexed graph engine leaves search
working.

codebase-memory-mcp is started once and kept as a child process: the server
holds an MCP session on it instead of running the binary again for every query,
which is the difference between about 8.6 seconds and about 50 milliseconds per
`trace_calls`. The graph project still travels with every single query, so no
scope is inherited from the connection. Where that process cannot be started —
an older build, a sandbox that will not allow it — queries fall back to
`codebase-memory-mcp cli`, one process per call: slower, and answering exactly
the same.

## Version Correctness

The test suite generates a demo repository whose `release/v1` `Process()` calls
`LegacyHandler()` and whose `release/v2` `Process()` calls `NewHandler()`, and
runs four checks against it through the real engines:

| Check | Expected |
| --- | --- |
| `search_code` `NewHandler` in v1 | 0 matches |
| `search_code` `NewHandler` in v2 | at least 1 match |
| `trace_calls` `Process` in v1 | reaches `LegacyHandler` |
| `trace_calls` `Process` in v2 | reaches `NewHandler` |

They run in CI on every pull request, on every push to `main`, and again before
any release is published (`integration/release_gate_test.go`). Any cross-version
contamination fails the build; this is a release blocker rather than a test that
can be marked flaky.

## Provider Model

Tools are written against three interfaces in `provider/`, so no tool knows what
a Zoekt query or a graph project is:

```go
type SearchProvider interface {
    Search(ctx context.Context, codeCtx vacctx.CodeContext, query SearchQuery) ([]SearchResult, error)
}
type GraphProvider interface {
    TraceCalls(ctx context.Context, codeCtx vacctx.CodeContext, req TraceRequest) (*CallGraph, error)
}
type SourceProvider interface {
    Read(ctx context.Context, codeCtx vacctx.CodeContext, path string, start, end int) (*SourceContent, error)
}
```

v0.1.0 implements them in `adapters/` with Zoekt, codebase-memory-mcp and git.
Every method takes the version context as well as a cancellation context: an
implementation that ignores it answers from the wrong version, which is the one
thing this project is for. Another engine — SCIP, another graph service,
something in-house — is a fourth adapter, not a fork.

## Security

**This project does not upload source code to an LLM.** It does no reasoning of
its own: it serves search, graph, source, version and evidence to whichever
agent your MCP client runs.

- **No LLM API key.** There is no model configuration in this project, and the
  configuration file rejects one.
- **No embeddings.** Nothing is vectorised, and there is no vector store.
- **No automatic upload.** vacmcp does not send your code anywhere on its own.
- **Local engines.** Zoekt is a web server you run, codebase-memory-mcp is a
  binary vacmcp runs as a subprocess, and git repositories are read from local
  paths. The default listen address is loopback.
- **Read only.** Repositories are read, never written. Source is read out of the
  object database, so no branch is checked out and no working tree is touched.
- **Your MCP client is yours.** You choose it, you trust it, and it is outside
  this project's control.

That last point is the boundary of what this project can promise. Where your
code goes after a tool answers is decided by the client you connected, and by
where you chose to run this server: point it at a remote client and the results
travel there. What is stated here is what vacmcp itself does and does not do.

Vulnerability reports go to the address in [SECURITY.md](SECURITY.md).

## Roadmap

v0.1.0 is the four tools above, one context per call, with the version isolation
and evidence they are built around. Ahead of it:

| Version | Direction |
| --- | --- |
| v0.2.0 | `compare_code` and `compare_calls` across two contexts; automatic context verification |
| v0.3.0 | multi-repo contexts |
| v0.4.0 | operations and telemetry |
| v1.0.0 | stable tool API and provider plugin contract, production deployment |

Deliberately out of scope, at every version: chat UI, RAG, vector databases,
embeddings, issue trackers, documentation search, forge-specific APIs, agent
memory, code modification, PR review. This server supplies evidence; the
reasoning belongs to your agent.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
