## Additive. Nothing breaks.

Every default is unchanged, every existing call site compiles untouched, and a
deployment that changes nothing behaves exactly as v0.7.0 did. Upgrading is safe
without reading further.

## The six operation budgets are now adjustable by a Go embedder

v0.7.0 gave every query-plane operation a budget reported as `OPERATION_TIMEOUT`.
Those budgets were package-private variables that only in-package tests could
reach, and a caller's own context deadline can only ever *shorten* an operation —
so an embedder whose repository was bigger than the defaults assume had no
workaround at all, only the option of waiting for a new release.

```go
src   := git.New(cfg, git.WithHistoryBudget(10*time.Minute))
graph := cbm.New(cfg, cbm.WithTraceBudget(5*time.Minute))
search := zoekt.New(cfg, zoekt.WithRequestBudget(time.Minute))
res   := resolver.New(cfg, resolver.WithResolveBudget(time.Minute))
```

| Option | Default (unchanged) |
| --- | --- |
| `git.WithReadBudget` | 30s |
| `git.WithDiffBudget` | 30s |
| `git.WithHistoryBudget` | 2m |
| `zoekt.WithRequestBudget` | 30s |
| `cbm.WithTraceBudget` | 2m |
| `cbm.WithConnectTimeout` | 2m |
| `resolver.WithResolveBudget` | 30s |

`git.WithHistoryBudget` is the one most likely to need raising: `git log -S` is a
pickaxe over every commit in range, and a history large enough to outrun two
minutes is a real repository asking a real question, not a wedged git.

`cbm.WithConnectTimeout` bounds a session start rather than a query, but it is
spent in full before the first trace can begin, so an embedder whose
codebase-memory-mcp indexes on startup has the same reason to raise it.

## These options are not an entry point to unlimited

A duration of zero is rejected, and so is a negative one. Only a positive
duration is accepted, and the rejection is a panic from the constructor, at the
moment the bad value is passed rather than at the first call that would have used
it — the argument comes from an embedder's own source, very nearly always a
literal, so a non-positive one is a bug in that source and not a state the
running server can be in.

Zero is called out because it is the most common misreading of an API shaped like
this, and because internally it very nearly works: the budget machinery treats a
duration of zero or less as *no budget at all*, so a silently accepted
`WithHistoryBudget(0)` would be the documented way to run an unbounded
`git log -S`. An unbounded query is precisely what the v0.7.0 budgets exist to
prevent. If a budget is too small, raise it to a number; there is deliberately no
way to remove it.

## No configuration surface

This is the Go API only. No environment variable, no YAML key, no `timeouts:`
block — how a downstream exposes these to *its* operators is the downstream's
decision, and baking a config shape into this module would prejudge it.

## Tests take the same path

Nothing assigns to a package-level budget any more; the old variables are
unexported constants and the test helpers that used to mutate them are gone.
Every test that needs a short budget constructs its provider with the same option
an embedder would, so the path an embedder takes is the one CI exercises on every
run, rather than a test-only path beside it that could drift.

---

Full entry in
[CHANGELOG.md](https://github.com/tc3oliver/version-aware-code-mcp/blob/v0.8.0/CHANGELOG.md#080---2026-09-21).

**Full changelog:** https://github.com/tc3oliver/version-aware-code-mcp/compare/v0.7.0...v0.8.0
