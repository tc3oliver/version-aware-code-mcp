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
branch, one revision and one call graph. Every tool call takes a context id —
two, when it compares two versions — and nothing it returns comes from outside
that scope.

```text
  your agent
      │   MCP 2026-07-28, over STDIO or Streamable HTTP
      ▼
   vacmcp ──── list_contexts   search_code    trace_calls   get_code
      │        compare_code    compare_calls
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
- **Comparison between two versions.** One file or one symbol's call graph, read
  in two contexts and reported as two sides, so neither version's evidence is
  mixed into the other's.
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
MCP SDK       OK    go-sdk v1.7.0, 6 tools
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

### Managed Mode

Everything above is the static mode: you index, you check out, you write the
configuration, and `vacmcp serve --config FILE` serves what you wrote. Managed
mode does that part for you. `vacmcp repo add` clones a repository into a data
directory, `vacmcp context create` pins a ref to a commit and builds that
commit's index and graph, and `vacmcp serve --managed` serves the contexts that
came out ready. Same engines, same six tools, same isolation — no configuration
file and no `zoekt-git-index`, `git worktree` or `index_repository` by hand.

```bash
vacmcp repo add backend --url git@git.example.com:team/backend.git
vacmcp context create backend-v1 --repo backend --ref release/1.x
vacmcp context create backend-v2 --repo backend --ref release/2.x
```

`context create` resolves the ref once and pins the full commit SHA it found.
That pin never moves: `vacmcp repo sync` fetches new commits, and the contexts
you already have keep answering out of the revisions they were created with. To
serve a newer commit, create another context — a context is immutable, so a
second version is a second name, never an edit of the first.

A context can name several repositories, one `--repo`/`--ref` pair each, and
each of them is pinned to its own commit:

```bash
vacmcp context create stack-v1 --repo backend --ref release/1.x \
                               --repo frontend --ref release/3.x
```

The nth `--ref` belongs to the nth `--repo`. Such a context is built and removed
as one thing: it reaches `READY` only when every repository in it has been
checked out, indexed, given its graph and verified, and one that fails anywhere
is `FAILED` whole rather than half servable.

The query tools answer in such a context by taking a `repository` argument, one
of the ones `list_contexts` reports for it. `search_code` covers every repository
of the context without it and one of them with it; the other four answer in
exactly one repository, so they require it and refuse the call with
`INVALID_ARGUMENT` naming the selectable repositories rather than answering out
of the first one and leaving the rest silently outside the scope. A `repository`
the context does not name is refused too — it selects one of the versions the
configuration granted, it never reaches past them.

Everything lives under `~/.vacmcp` unless `--data-dir` says otherwise, including
the search index. Start Zoekt over it, check the installation, and serve:

```bash
zoekt-webserver -index ~/.vacmcp/zoekt -listen 127.0.0.1:6070 -rpc &
vacmcp doctor --managed
vacmcp serve --managed
```

```text
$ vacmcp context list
backend-v1    backend     8af31e2c8d0a4f1b6e5d3c2a90b7f4e1d6c8a3b5    READY
backend-v2    backend     94cb821f7a6e5d4c3b2a1908f7e6d5c4b3a29187    READY
stack-v1      backend     8af31e2c8d0a4f1b6e5d3c2a90b7f4e1d6c8a3b5    READY
stack-v1      frontend    1d0c9b8a7f6e5d4c3b2a19087f6e5d4c3b2a1908    READY
```

One row per repository a context names, so a context over one reads as it always
has and one over several says which commit each of its repositories is pinned to.

The commands, in full:

| Command | What it does |
| --- | --- |
| `vacmcp repo add NAME --url URL` | clone a repository into the data directory |
| `vacmcp repo list` / `status NAME` | report the managed repositories |
| `vacmcp repo sync NAME` / `--all` | fetch remote refs, moving no pinned revision |
| `vacmcp repo remove NAME` | forget a repository and delete its clone |
| `vacmcp context create NAME --repo REPO --ref REF [--repo REPO --ref REF ...]` | pin one ref per repository to a commit and build its index and graph |
| `vacmcp context list` / `status NAME` | report the managed contexts and their state |
| `vacmcp context verify NAME` | re-run the readiness checks, changing nothing |
| `vacmcp context retry NAME` | rebuild a context that did not reach `READY` |
| `vacmcp context remove NAME` | forget a context, its worktree and its graph |
| `vacmcp serve --managed [--data-dir DIR] [--zoekt-url URL] [--cbm-command CMD]` | serve the ready contexts |
| `vacmcp doctor --managed [--data-dir DIR]` | check the managed repositories and contexts |

Only these commands reach a network, and only from your shell: creating,
syncing and removing repositories and contexts is not exposed as an MCP tool, so
an agent connected to the server cannot make it clone or delete anything.
vacmcp holds no credential either — `git` authenticates the clone the way it
always does, through your SSH agent, `~/.ssh/config` or a credential helper, and
a URL with a secret embedded in it is refused rather than stored.

A running `vacmcp serve --managed` serves the contexts it read when it started,
and nothing changes them underneath it. `context create`, `context retry`,
`context remove` and `repo remove` are refused while one is running rather than
done behind its back — a server left answering out of a worktree, an index or a
graph that has been deleted is exactly what this project must never do. To put a
change into service, stop the server, run the command, and start it again:

```bash
# stop the running `vacmcp serve --managed`
vacmcp context create backend-v3 --repo backend --ref release/3.x
vacmcp serve --managed
```

`vacmcp repo sync` is not refused and needs no restart: it fetches, and a fetch
moves no pinned revision, so a running server keeps answering exactly as it did.
The commits it brings in reach a server through the next `context create` and the
restart that follows. Reading is never refused either — `context list`, `status`,
`verify`, `repo list`, `status` and `doctor --managed` all run while a server
does, so nothing stops you diagnosing one.

On Unix-like systems both of these hold across processes: repository lifecycle
operations take turns, and a management command really is refused while a
managed server runs.

On Windows, Managed Mode currently provides in-process locking only. Do not run
vacmcp management commands against a data directory that another vacmcp
management command, or a running `vacmcp serve --managed`, is using.

Static Mode is unaffected.

**Already using `vacmcp serve --config FILE`? Nothing changes for you.** Managed
mode is additive and entirely optional: static configuration files are still
supported exactly as before, there is no migration to do, and the two modes are
chosen per server with `--config` or `--managed`.

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

## Multi-Repo Context

A context can name one repository or several. One naming several is a
*workspace*: each repository it lists — a *member* — is pinned to its own
revision, exactly as a single-repository context pins one. A context over one
repository is a workspace with exactly one member, and answers exactly as it
did before this feature existed: same arguments, same wire shape, same error
codes. Nothing about that case changes; multi-repo is what a context does once
it names a second repository.

```yaml
contexts:
  stack-v1:
    members:
      - repository: example/backend
        branch: release/1.x
        revision: 8af31e2
        graph_ref: backend-v1
      - repository: example/frontend
        branch: release/3.x
        revision: 1a2b3c4
        graph_ref: frontend-v3
```

`config/example.yaml` has this alongside the single-repository contexts above
it, and `vacmcp validate` loads both the same way. Managed Mode builds the same
shape from the CLI, with one `--repo`/`--ref` pair per member:

```bash
vacmcp context create stack-v1 --repo backend --ref release/1.x \
                               --repo frontend --ref release/3.x
```

`list_contexts` reports `stack-v1` as `{"id": "stack-v1", "members": [...]}`
instead of the flat `repository`/`branch`/`revision` a single-member context
carries — the wire shape follows the member count, one member emitting exactly
what v0.4.0 did and several emitting the array instead, never both at once.
Every result's `context` block and every citation follow the same rule: a
single-member answer's evidence carries no `repository` or `revision` of its
own, as before, and a several-member answer's evidence — and `search_code`'s
matches — each carry the repository and revision they were found in. The
[Tools](#tools) examples below show both shapes.

`repository` is the one argument a request may use to say which member it
means, and it only narrows what the context already names — a repository the
context does not have is refused, never reached:

| Tool | One member | Several members |
| --- | --- | --- |
| `search_code` | unchanged | searches every member unless `repository` narrows it to one |
| `get_code` | unchanged | `repository` required — two members could both have the path, so nothing else says which one was meant |
| `trace_calls` | unchanged | `repository` required — a call graph is one repository's own, so there is no walk without it |
| `compare_code` / `compare_calls` | unchanged | `repository` required on both sides, and it must name a repository both contexts have |

Nothing is inferred even when only one member happens to have a match today:
the same request would silently change meaning the day a second member
acquired the same path or symbol, which is the ambiguity this server always
refuses rather than guesses at.

### Limitations

v0.5.0 does not:

- **Build, infer or claim a call edge between two repositories.** `trace_calls`
  walks the one member's graph `repository` names and nothing else — there is
  no attempt to connect a caller in one repository to a callee in another.
- **Compare a whole workspace to another.** `compare_code` and `compare_calls`
  compare one repository at a time, picked out by `repository` on both sides;
  there is no member-added/member-removed report and no workspace-level diff.
- **Deduplicate artifacts two workspaces share.** Two Managed Mode workspaces
  naming the same repository at the same revision each get their own index and
  graph.
- **Detect a rename or move.** Unchanged since v0.4.0: a comparison matches
  paths literally, in a multi-repo workspace exactly as in a single-repository
  one.
- **Resolve symbol identity semantically.** Unchanged since v0.4.0: a
  comparison matches the symbol name that was asked for, textually, one
  repository at a time.

## Tools

| Tool | Arguments | Returns |
| --- | --- | --- |
| `list_contexts` | none | every configured context: id, and either repository, branch and revision, or a `members` array of them |
| `search_code` | `context`, `query`, `repository`? | matches as path, line and snippet, from that context's branch only |
| `trace_calls` | `context`, `symbol`, `direction`, `depth`, `repository`? | call edges as caller, callee, path and line, from that context's graph only |
| `get_code` | `context`, `path`, `start_line`, `end_line`, `repository`? | those lines as they are at that context's revision |
| `compare_code` | `from_context`, `to_context`, `path`, `repository`? | what happened to that file between the two revisions — `ADDED`, `REMOVED`, `MODIFIED` or `UNCHANGED` — with the changed regions as hunks |
| `compare_calls` | `from_context`, `to_context`, `symbol`, `direction`, `depth`, `repository`? | which versions had the symbol, and the call relations added, removed and unchanged between them |

`repository` is marked `?` because it is optional to the schema, not because it
is always optional: a context naming one repository never needs it, and a context
naming several requires it in every tool but `search_code`, which searches all of
them without it.

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

A context naming several repositories carries them as a `members` array instead
of those four fields, in `list_contexts` and in the `context` block of a result
alike — one entry per repository, each with its own `branch` and `revision`, and
no `id` of its own, since the context's id names the whole set. Each citation in
`evidence` then carries the `repository` and `revision` it was found in, and so
does each `search_code` match. The shape follows the number of repositories: a
context naming one answers exactly as it does above, so a client written against
that shape keeps reading it.

The two comparison tools take two contexts instead of one and answer in both, so
they carry two of those blocks rather than one merged pair. `from` and `to` each
hold the version that side was answered in and the evidence backing it, and the
side of a version that does not have the file or the symbol is `null`. There is
no combined context or evidence beside them: a citation only means anything at
the revision it was read at, so one flattened list could not say which version
an entry came from.

```json
{
  "from": {
    "context": {
      "id": "backend-v1",
      "repository": "backend",
      "branch": "release/1.x",
      "revision": "68c19fdb264bad80cf220f672f89dc90478a9d66"
    },
    "evidence": []
  },
  "to": {
    "context": {
      "id": "backend-v2",
      "repository": "backend",
      "branch": "release/2.x",
      "revision": "7b05c6aeb7544cc7fd2b69c56c89c9bcfd46a1a2"
    },
    "evidence": []
  },
  "change": "MODIFIED",
  "path": "processor.go",
  "binary": false,
  "hunks": [
    {
      "old_start": 2,
      "old_lines": 5,
      "new_start": 2,
      "new_lines": 5,
      "lines": [
        { "kind": "CONTEXT", "content": "" },
        { "kind": "CONTEXT", "content": "// Process handles one request." },
        { "kind": "CONTEXT", "content": "func Process(req string) string {" },
        { "kind": "REMOVED", "content": "\treturn LegacyHandler(req)" },
        { "kind": "ADDED", "content": "\treturn NewHandler(req)" },
        { "kind": "CONTEXT", "content": "}" }
      ]
    }
  ]
}
```

That is `compare_code` on the file `get_code` read above, across the two
contexts: the one line that differs between the versions, as one hunk, with the
lines it covers in each of them. A binary file is marked `binary` and has no
hunks, which is what tells it apart from a file with nothing to show.
`compare_calls` answers in the same two sides, with `presence` — `BOTH`,
`FROM_ONLY` or `TO_ONLY` — saying which versions had the symbol at all, and the
relations around it as `added`, `removed` and `unchanged`, each carrying its
call sites in each version separately. A relation is caller, callee and path
without the line, so code moving down a file is not reported as one call
disappearing and another appearing.

**What a comparison does not do.** Paths and symbol names are matched literally.
There is no rename or move detection: a file renamed between the two revisions
is one path removed and another added rather than one path that moved, and a
symbol renamed between versions is not recognised as the same symbol. Each side
resolves the name that was asked for on its own — `requested_symbol` is what was
asked for, `from_resolved_symbol` and `to_resolved_symbol` are what each version
matched it to — and a version with no such symbol is the absent side rather than
a failure, while a symbol neither version has is `SYMBOL_NOT_FOUND`. Identity
here is textual, not semantic or AST-aware, so a comparison is not an impact
analysis. Comparison also stays inside one repository: two repositories have no
shared history, and their call graphs are not two versions of one, so two
contexts naming different repositories are `INVALID_ARGUMENT`. In a multi-repo
workspace this is `repository`'s job to prevent — it picks one repository on
both sides of a comparison, so two repositories are never compared against
each other there either. `search_code` is the one tool that does span every
member of a workspace; see [Multi-Repo Context](#multi-repo-context) and its
[Limitations](#limitations).

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
| `SOURCE_DIFF_UNAVAILABLE` | the source this server has reads one version at a time and cannot compare two |

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

## Embedding Guide

The six tools are a thin MCP layer over a Go package you can call directly.
`engine.Engine` answers all six queries with no server, no transport and no
wire schema in the way, so a gateway of your own holds the same version
isolation this server does — and can be tested without a server in front of it.

```go
cfg, err := config.Load("vacmcp.yaml")
if err != nil {
    return err
}

eng := engine.New(resolver.New(cfg), zoekt.New(cfg), cbm.New(cfg), git.New(cfg))

result, err := eng.SearchCode(ctx, engine.SearchCodeRequest{
    Context: "backend-v2",
    Query:   "NewHandler",
})
if err != nil {
    return errors.Join(err, eng.Close())
}

result.Context()   // the vacctx.Workspace it was answered in — one member here
result.Evidence()  // where the answer can be checked, one list per member
result.Matches()   // the payload
```

`engine.New(contexts, search, graph, source)` starts nothing, so it cannot fail.
Any of the three providers may be nil: the query needing an absent one fails and
the others are unaffected, so an engine built with a search provider and no
graph still searches. `SearchCode` and `TraceCalls` name what is missing —
`SEARCH_PROVIDER_UNAVAILABLE` and `GRAPH_PROVIDER_UNAVAILABLE`. `GetCode` with
no source provider fails with `REPOSITORY_NOT_FOUND` instead: the code set has
no source equivalent, and adding one would change the public tool API.
`CompareCalls` names an absent graph provider exactly as `TraceCalls` does, and
`CompareCode` with no source provider fails exactly as `GetCode` does. A source
provider that is there but cannot compare two versions is a different answer
again, `SOURCE_DIFF_UNAVAILABLE`, and the next section is where that capability
is implemented.

A request names its scope with context ids: no request type has a branch or
revision field, and the `Repository` field some of them have —
`SearchCodeRequest.Repository`, and its equivalent on `TraceCallsRequest`,
`GetCodeRequest`, `CompareCodeRequest` and `CompareCallsRequest` — narrows a
query to one of the repositories its context already names rather than
reaching outside them; a context naming one repository never needs it, and a
context naming several has no query without it except `SearchCode`, which
spans every member unless narrowed. Every successful result carries the
version it was answered in together with its evidence, because the result
types have no exported fields and only a method on the engine builds one. The
version guarantee is inherited by embedding rather than re-implemented.

**Lifecycle.** `Close` releases the dependencies that can be released: one
implementing `io.Closer` is closed, one that does not is left untouched, and
every dependency gets its turn whether or not an earlier one failed. That
feature detection *is* the ownership contract — handing over a provider that can
be closed is what says "close this one for me". A provider you keep owning — one
CBM session shared between two engines, say — is handed over without a `Close`
method, or wrapped in a type that does not promote one:

```go
type keepOpen struct{ provider.GraphProvider }
```

Before `New` returns, cleanup is the caller's: a program that starts a CBM
subprocess and then fails while building the next provider must close what it
already started, because there is no Engine yet to do it. From then until
`Close`, it is the Engine's. `engine.Engine.Close`'s doc comment is the
authority on both.

`examples/embed` is a complete program doing all of the above. It runs with no
Zoekt, no codebase-memory-mcp and no checkout — its providers are stand-ins — so
`go run ./examples/embed` prints two versions' answers straight away.

**Supported embedding API.** `engine`, `provider`, `vacctx`, `vacerr`,
`evidence`, `config`, `resolver`, `adapters/*` and `managed` are supported for
embedding: documented, tested, and meant to be called from outside this
repository. Supported is not a permanent backward-compatibility guarantee — that
is v1.0.0's, in the [Roadmap](#roadmap) below. Until then a breaking change to
these packages is possible and arrives with a release note, not silently.
Anything under `internal/` is not part of this and may change in any release.

## Custom Provider Guide

Four interfaces are the whole required extension surface. Implement them and the
engine answers out of your backend — an SCIP index, another graph service,
something in-house — with no fork of anything here. `SearchProvider`,
`GraphProvider` and `SourceProvider` are each handed one member's version scope
(`vacctx.CodeContext`) alongside a cancellation `context.Context`, whether the
context it came from names one repository or several: an implementation that
ignores the scope answers from the wrong version, which is the one thing this
project is for. `ContextSource` is where those scopes come from rather than
something handed one, and answers with a `vacctx.Workspace` — the set of
repositories a context id names, one member for a single-repository context and
several for a multi-repo one, with no separate method or type for either.
`Contexts(ctx)` lists the workspaces that exist, and `Resolve(ctx, id)` turns
an id into the one it names; both take a `context.Context` now, so an
implementation reaching a backend of its own to answer either has somewhere to
carry cancellation and a deadline.

| Interface | Package | Responsible for |
| --- | --- | --- |
| `ContextSource` | `engine` | which workspaces exist, and resolving an id to one |
| `SearchProvider` | `provider` | matches confined to one member's repository and branch |
| `GraphProvider` | `provider` | a traversal of the graph one member's `graph_ref` names, and no other |
| `SourceProvider` | `provider` | the lines of a file as they are at one member's revision |

```go
type ContextSource interface {
    Contexts(ctx context.Context) ([]vacctx.Workspace, error)
    Resolve(ctx context.Context, id string) (vacctx.Workspace, error)
}
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

`ContextSource` is a compile-breaking change from the interface v0.4.0 shipped:
both methods returned `vacctx.CodeContext` then, and `Contexts` took no
argument. A single-repository implementation needs no new logic to catch up —
wrap each existing `vacctx.CodeContext` in a one-member
`vacctx.Workspace{ID: id, Members: []vacctx.CodeContext{c}}` and add the
`context.Context` parameter — but every implementation has to be touched
before it compiles again, which is deliberate: the alternative, adding a
`Members` field to `CodeContext` instead of changing the return type, would
let an unfixed implementation keep compiling while its `Repository` field
silently stayed empty — a wrong version served rather than a build caught
before it ships.

**Optional: comparing two versions.** A `SourceProvider` may additionally
implement `provider.SourceDiffer`, and that is what `compare_code` is answered
by. It is a capability of its own rather than a fifth method on `SourceProvider`
because not every source backend has a second revision to compare against, and a
backend that cannot compare should be a type assertion that fails rather than a
`Read`-shaped promise returning an apology:

```go
type SourceDiffer interface {
    Diff(ctx context.Context, from, to vacctx.CodeContext, req SourceDiffRequest) (*SourceDiff, error)
}
```

The two contexts are the entire scope of the comparison, exactly as the single
one is for `Read`, and the answer is structured — added, removed, modified or
unchanged, with the changed regions as hunks — never diff text for someone else
to parse. A `SourceProvider` that does not implement it is not a broken one:
every other query is unaffected, `compare_calls` included, since that walks each
version's graph through the `GraphProvider` you already wrote. `compare_code` is
then the only thing that fails, with `SOURCE_DIFF_UNAVAILABLE` — a fact about
this server's capability rather than about the file, the revisions or the
repository, so the tool is still there and still says exactly why it cannot
answer. `adapters/git` implements it, over the same repository it reads.

Two rules are yours to hold rather than the engine's to enforce. A
`SourceProvider` must fail closed: content that is not the revision the context
declares is a `vacerr` `SOURCE_MISMATCH` and never a warning or a best effort.
And a `ContextSource` refuses an id it does not hold with `CONTEXT_NOT_FOUND` —
the engine re-checks that a resolved context has all four of its fields, but it
cannot check that the version you returned is the version that was asked for.

Validating what a caller sent is yours as well. Those four fields of the
resolved context are the whole of what the engine checks: the `path` handed to
`SourceProvider.Read` and the query handed to `SearchProvider.Search` reach you
exactly as the caller wrote them. A provider serving files from a checkout is
therefore the one that has to refuse a path traversing out of it, and a provider
building a backend query out of one is the one that has to make it safe.

`engine/extension_test.go`'s
`TestEngineRunsOnImplementationsOfNothingButItsInterfaces` is a tested
implementation of all four from scratch: a map, and three types returning canned
answers, reaching all four queries with no Zoekt, no codebase-memory-mcp and no
git anywhere in it.

v0.1.0's own implementations are these same interfaces — `adapters/` for Zoekt,
codebase-memory-mcp and git, `resolver/` for the configuration file — so another
engine is a fourth adapter, not a fork.

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

v0.1.0 is the first four tools above, one context per call, with the version
isolation and evidence they are built around. v0.2.0 is the managed lifecycle
that puts a repository behind them without anyone assembling one by hand. v0.3.0
is the same query plane as a Go package, so a program of your own can call it
directly instead of running this server and talking MCP to it. v0.4.0 is the
other two tools, the first queries that name two versions and answer in both.
v0.5.0 is multi-repo contexts: one context id can name several repositories, a
search spans all of them, and the graph and the comparisons stay scoped to one
repository at a time — see [Multi-Repo Context](#multi-repo-context). Ahead of
those:

| Version | Direction |
| --- | --- |
| v0.1.0 | version-aware query plane: the four tools, context isolation, evidence — done |
| v0.2.0 | managed repository and context lifecycle: `repo` and `context` commands, automatic Zoekt and graph provisioning, readiness verification — done |
| v0.3.0 | embeddable core / extension API: a supported, documented Go API to embed this core in your own gateway, not yet under a compatibility guarantee — done |
| v0.4.0 | version intelligence: `compare_code`, `compare_calls`, revision and graph diff within one repository — done |
| v0.5.0 | multi-repo contexts: a workspace of several repositories under one context id, search spanning every member, and multi-repo graph query — each member keeps its own revision-scoped graph, so there is no cross-repository call edge — this release |
| v0.6.0 | operations: metrics, OpenTelemetry, garbage collection, scheduled sync primitives |
| v1.0.0 | stable public API and compatibility contract |

Deliberately out of scope, at every version: chat UI, RAG, vector databases,
embeddings, issue trackers, documentation search, forge-specific APIs, agent
memory, code modification, PR review. This server supplies evidence; the
reasoning belongs to your agent.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
