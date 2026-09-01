#!/usr/bin/env bash
# Generates testdata/versioned-demo-repo, the fixture the version-correctness
# release gate runs against: three branches whose Process() deliberately calls a
# different handler per release.
#
#   main        Process() -> LegacyHandler()
#   release/v1  Process() -> LegacyHandler()   (cut from main)
#   release/v2  Process() -> NewHandler()      (cut from main)
#
# The same two branches are what the comparison tools are tested against, so
# every outcome they can report is written into this one repository rather than
# into a repository per scenario. Comparing release/v1 to release/v2 gives:
# processor.go/handler.go/version.go modified, go.mod/README.md/shared.go
# unchanged, newonly.go added, oldonly.go removed, Process -> NewHandler added,
# Process -> LegacyHandler removed, and Keep -> SharedHandler unchanged.
#
# The repository is regenerated from scratch on every run. Author and committer
# identity and dates are pinned so the revisions are identical across runs,
# which is what makes repeated generation safe for an already indexed fixture.
#
# Every commit is made in a staging directory beside the fixture and the result
# is moved into place in one rename. The fixture is shared: while it is being
# rebuilt, other test processes are deciding whether it is usable by looking for
# its three branches. Building in place made that decision meaningless — the
# three branches exist from `checkout -b release/v2` onward, but release/v2
# still points at main's commit until two commits later, so a reader that looked
# in between saw a complete-looking fixture whose v1 and v2 held the same
# processor.go. That surfaced as a version-isolation failure in tools/get_code,
# which is the one failure this project must never report falsely. Staging plus
# rename means the fixture path only ever holds a finished repository.
set -euo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
repo="$root/testdata/versioned-demo-repo"
# Beside the fixture, so publishing is a rename within one filesystem.
building="$repo.building"
stale="$repo.stale"

export GIT_AUTHOR_NAME='vacmcp fixture'
export GIT_AUTHOR_EMAIL='fixture@example.invalid'
export GIT_AUTHOR_DATE='2026-01-01T00:00:00+00:00'
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
export GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"
export GIT_COMMITTER_DATE="$GIT_AUTHOR_DATE"

# --no-verify and gpgsign=false keep the caller's global git configuration from
# changing what this fixture commits.
commit() {
	git -C "$building" add -A
	git -C "$building" -c commit.gpgsign=false commit --no-verify -q -m "$1"
}

rm -rf "$building" "$stale"
git init -q -b main "$building"

# Pin the git directory before any other git call. Without this, a git command
# run against a $building that has no .git does not fail: it searches upward and
# finds the enclosing vacmcp repository, so `add -A` + `commit` commits the
# developer's uncommitted work and `checkout -b release/v1` switches their
# branch. That happened. It is data loss, not flakiness, so the guard is
# explicit rather than assumed.
if [ ! -d "$building/.git" ]; then
	echo "gen-versioned-demo-repo: $building/.git missing after git init; refusing to run git against the parent repository" >&2
	exit 1
fi
export GIT_DIR="$building/.git"
export GIT_WORK_TREE="$building"

cat >"$building/go.mod" <<'EOF'
module example.com/demo

go 1.26
EOF

cat >"$building/README.md" <<'EOF'
# demo

Fixture repository for version-aware-code-mcp. Process() calls a different
handler on each release branch.
EOF

cat >"$building/handler.go" <<'EOF'
package demo

// LegacyHandler serves a request the way releases up to v1 do.
func LegacyHandler(req string) string {
	return "legacy: " + req
}
EOF

cat >"$building/processor.go" <<'EOF'
package demo

// Process handles one request by delegating to the handler of this release.
func Process(req string) string {
	return LegacyHandler(req)
}
EOF

commit 'Add Process delegating to LegacyHandler'

# On main, before either release is cut, so both branches inherit this file and
# this call byte for byte: Keep -> SharedHandler is the call relation a
# comparison has to report as unchanged. Every other call in this repository
# differs between the releases, so without one that does not, "unchanged" could
# only ever be tested as an empty list.
cat >"$building/shared.go" <<'EOF'
package demo

// SharedHandler serves a request the same way on every release.
func SharedHandler(req string) string {
	return "shared: " + req
}

// Keep delegates to SharedHandler, identically on every release.
func Keep(req string) string {
	return SharedHandler(req)
}
EOF

commit 'Add Keep delegating to SharedHandler'

git -C "$building" checkout -q -b release/v1 main

cat >"$building/version.go" <<'EOF'
package demo

// Version is the release this branch carries.
const Version = "v1"
EOF

commit 'Cut release/v1'

# release/v1 and nowhere else. release/v2 is cut from main rather than from
# here, so it never sees this commit: comparing v1 to v2 is a file that was
# removed, and comparing the other way round is one that was added.
cat >"$building/oldonly.go" <<'EOF'
package demo

// OldOnly is written on release/v1 and on no other branch.
func OldOnly(req string) string {
	return "old only: " + req
}
EOF

commit 'Add OldOnly to release/v1'

git -C "$building" checkout -q -b release/v2 main

cat >"$building/handler.go" <<'EOF'
package demo

// NewHandler serves a request the way v2 does.
func NewHandler(req string) string {
	return "new: " + req
}
EOF

cat >"$building/processor.go" <<'EOF'
package demo

// Process handles one request by delegating to the handler of this release.
func Process(req string) string {
	return NewHandler(req)
}
EOF

cat >"$building/version.go" <<'EOF'
package demo

// Version is the release this branch carries.
const Version = "v2"
EOF

commit 'Switch Process to the v2 handler'

# The mirror of oldonly.go: release/v2 and nowhere else.
cat >"$building/newonly.go" <<'EOF'
package demo

// NewOnly is written on release/v2 and on no other branch.
func NewOnly(req string) string {
	return "new only: " + req
}
EOF

commit 'Add NewOnly to release/v2'

git -C "$building" checkout -q main

# Publish, by swapping directories rather than editing one. Everything above ran
# where nobody is looking, so these renames are the only moments the fixture path
# changes, and it only ever holds a finished repository or nothing at all — never
# a half-built or half-deleted one. Readers decide with internal/demorepo's
# complete(), which asks the fixture path for its three branches; there is now no
# state in which that question has a misleading answer.
#
# The old fixture is renamed aside instead of deleted in place because rm -rf on
# the fixture path is itself a window: a reader looking during it finds some of
# the branches and none of the objects. Removing the renamed copy afterwards
# costs the same and is not observable.
if [ -e "$repo" ]; then
	mv "$repo" "$stale"
fi
mv "$building" "$repo"
rm -rf "$stale"
