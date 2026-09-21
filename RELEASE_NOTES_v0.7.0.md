## Read this before upgrading: an error code changed meaning

A Zoekt request that runs out of time now reports **`OPERATION_TIMEOUT`**. It
reported `SEARCH_PROVIDER_UNAVAILABLE`, and had since v0.1.0, because the budget
was an `http.Client` timeout — an error indistinguishable from a connection
failure. A Zoekt that accepted the request and went quiet was therefore reported
as a Zoekt that could not be reached.

```
connection refused, bad response, HTTP failure  → SEARCH_PROVIDER_UNAVAILABLE
vacmcp's own request budget expired             → OPERATION_TIMEOUT   (was SEARCH_PROVIDER_UNAVAILABLE)
the caller cancelled or its deadline expired    → the raw context error
```

The duration is unchanged at 30 seconds; what changed is who owns it. **A client
that branches on `SEARCH_PROVIDER_UNAVAILABLE` to mean "slow or absent" has to be
updated.** This is made now, before v1.0.0, rather than leaving Zoekt permanently
inconsistent with git and codebase-memory-mcp.

`OPERATION_TIMEOUT` is also a failure mode every query-plane operation can now
produce where none of them could before. Handle it wherever you handle errors.

| Operation | Tools | Budget |
| --- | --- | --- |
| Context resolution | every tool call | 30s |
| Source read | `get_code` | 30s |
| Diff | `compare_code` | 30s |
| History walk | `search_history` | 2m |
| Graph traversal | `trace_calls`, `compare_calls` | 2m |
| Search request | `search_code` | 30s |

Budgets compose: a `search_history` call spends the resolve budget and then the
walk budget, on separate clocks. They are compile-time constants with no
configuration surface in this release. Management-plane operations — `repo add`,
`repo sync`, `context create` — get no budget at all and still run as long as
they need.

## Also required: codebase-memory-mcp 0.11.0

The oldest supported codebase-memory-mcp is now **0.11.0**; `vacmcp doctor`
reports anything older as a version mismatch. The rule from here is that the
supported floor equals the real-engine baseline CI pins. Upgrade the engine
together with vacmcp, not after.

## A wrong answer this release fixes

A graph past the fiftieth project was reported as missing. The management plane
confirmed a codebase-memory-mcp graph by listing projects and looking for the one
it had just built, but read only the first page of that answer. 0.11.0 paginates
`list_projects` at 50, so on an installation holding more than fifty projects the
verification of a graph that existed and was queryable returned "not found" — a
wrong answer, not a slow one. The call now walks every page. 0.10.1 did not
paginate, so the defect was unreachable before the engine upgrade and is fixed in
the same release that could first expose it.

---

The full entry, including the shutdown and cancellation fixes, the new
`search_history` MCP tool and the test and documentation work, is in
[CHANGELOG.md](https://github.com/tc3oliver/version-aware-code-mcp/blob/v0.7.0/CHANGELOG.md#070---2026-09-21).

**Full changelog:** https://github.com/tc3oliver/version-aware-code-mcp/compare/v0.6.0...v0.7.0
