#!/usr/bin/env bash
# Generates testdata/second-demo-repo: a second git repository, standing beside
# versioned-demo-repo and colliding with it on purpose.
#
# handler.go is the collision. versioned-demo-repo also has a handler.go, and
# on release/v1 that file declares a function named LegacyHandler; this
# repository's handler.go declares a function of the same name, at the same
# path, with different content. That is what a multi-repository workspace
# needs to prove real isolation on: search_code, get_code and trace_calls
# asked about "handler.go" or "LegacyHandler" have two candidates, and the
# repository argument is what picks between them rather than a name that
# happens to be unique.
#
# invoke.go gives the collision a caller, so trace_calls has an edge to walk:
# tracing LegacyHandler's callers here must reach Invoke and never
# versioned-demo-repo's Process, which is the graph half of the same proof.
#
# One branch, one commit, staging plus rename to publish it — the same
# discipline gen-versioned-demo-repo.sh holds to and for the same reason: a
# reader must never see this path holding anything but a finished repository.
# See that script's own comment for the failure this avoids.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
repo="$root/testdata/second-demo-repo"
building="$repo.building"
stale="$repo.stale"

export GIT_AUTHOR_NAME='vacmcp fixture'
export GIT_AUTHOR_EMAIL='fixture@example.invalid'
export GIT_AUTHOR_DATE='2026-01-01T00:00:00+00:00'
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
export GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
export GIT_COMMITTER_DATE="$GIT_AUTHOR_DATE"

rm -rf "$building" "$stale"
git init -q -b main "$building"

# Pin the git directory before any other git call, for the reason
# gen-versioned-demo-repo.sh's own comment gives: without it, a $building with
# no .git does not fail here, it silently commits to the enclosing vacmcp
# repository instead.
if [ ! -d "$building/.git" ]; then
	echo "gen-second-demo-repo: $building/.git missing after git init; refusing to run git against the parent repository" >&2
	exit 1
fi
export GIT_DIR="$building/.git"
export GIT_WORK_TREE="$building"

cat >"$building/go.mod" <<'EOF'
module example.com/second

go 1.26
EOF

cat >"$building/handler.go" <<'EOF'
package second

// LegacyHandler is second-demo-repo's own function of this name: the same
// path and the same symbol name as versioned-demo-repo's handler.go
// (release/v1), on purpose — the collision a multi-repository workspace has
// to keep apart rather than merge.
func LegacyHandler(req string) string {
	return "second: " + req
}
EOF

cat >"$building/invoke.go" <<'EOF'
package second

// Invoke is this repository's only caller of LegacyHandler, so tracing that
// symbol's callers here has an edge to find — and it must never be
// versioned-demo-repo's Process, which calls a same-named function in a graph
// of its own.
func Invoke(req string) string {
	return LegacyHandler(req)
}
EOF

git -C "$building" add -A
git -C "$building" -c commit.gpgsign=false commit --no-verify -q -m "Add LegacyHandler and its caller"

# Publish by rename, never in place: see gen-versioned-demo-repo.sh.
if [ -e "$repo" ]; then
	mv "$repo" "$stale"
fi
mv "$building" "$repo"
rm -rf "$stale"
