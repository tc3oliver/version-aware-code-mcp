#!/usr/bin/env bash
# Generates testdata/versioned-demo-repo, the fixture the version-correctness
# release gate runs against: three branches whose Process() deliberately calls a
# different handler per release.
#
#   main        Process() -> LegacyHandler()
#   release/v1  Process() -> LegacyHandler()   (cut from main)
#   release/v2  Process() -> NewHandler()      (cut from main)
#
# The repository is regenerated from scratch on every run. Author and committer
# identity and dates are pinned so the revisions are identical across runs,
# which is what makes repeated generation safe for an already indexed fixture.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
repo="$root/testdata/versioned-demo-repo"

export GIT_AUTHOR_NAME='vacmcp fixture'
export GIT_AUTHOR_EMAIL='fixture@example.invalid'
export GIT_AUTHOR_DATE='2026-01-01T00:00:00+00:00'
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
export GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
export GIT_COMMITTER_DATE="$GIT_AUTHOR_DATE"

# --no-verify and gpgsign=false keep the caller's global git configuration from
# changing what this fixture commits.
commit() {
	git -C "$repo" add -A
	git -C "$repo" -c commit.gpgsign=false commit --no-verify -q -m "$1"
}

rm -rf "$repo"
git init -q -b main "$repo"

cat >"$repo/go.mod" <<'EOF'
module example.com/demo

go 1.26
EOF

cat >"$repo/README.md" <<'EOF'
# demo

Fixture repository for version-aware-code-mcp. Process() calls a different
handler on each release branch.
EOF

cat >"$repo/handler.go" <<'EOF'
package demo

// LegacyHandler serves a request the way releases up to v1 do.
func LegacyHandler(req string) string {
	return "legacy: " + req
}
EOF

cat >"$repo/processor.go" <<'EOF'
package demo

// Process handles one request by delegating to the handler of this release.
func Process(req string) string {
	return LegacyHandler(req)
}
EOF

commit 'Add Process delegating to LegacyHandler'

git -C "$repo" checkout -q -b release/v1 main

cat >"$repo/version.go" <<'EOF'
package demo

// Version is the release this branch carries.
const Version = "v1"
EOF

commit 'Cut release/v1'

git -C "$repo" checkout -q -b release/v2 main

cat >"$repo/handler.go" <<'EOF'
package demo

// NewHandler serves a request the way v2 does.
func NewHandler(req string) string {
	return "new: " + req
}
EOF

cat >"$repo/processor.go" <<'EOF'
package demo

// Process handles one request by delegating to the handler of this release.
func Process(req string) string {
	return NewHandler(req)
}
EOF

cat >"$repo/version.go" <<'EOF'
package demo

// Version is the release this branch carries.
const Version = "v2"
EOF

commit 'Switch Process to the v2 handler'

git -C "$repo" checkout -q main
