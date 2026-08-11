#!/bin/sh
# Builds the code graphs the stack traces calls against, then exits.
#
# One CBM project per release, named after the context's graph_ref. Sharing one
# project across releases would answer trace_calls from the other version's
# graph, which is the cross-version contamination this server exists to prevent.
#
# CBM indexes a directory, not a revision, so each release needs a real
# checkout. The worktrees are local to this container; only the graph store,
# which CBM writes under $HOME/.cache, is shared with vacmcp.
#
# A store that already holds the graphs of these revisions is left alone, and
# not only to save the work. CBM coordinates its processes across the store, and
# it refuses to run while it considers another generation active — which is what
# a running vacmcp container, holding the same store, looks like from here. So a
# second `docker compose up` must not re-index: it would fail with "a
# pre-coordination or unverified CBM generation is active" and take the stack
# down with it. Rebuild the graphs with `down -v` and then `up`, which starts
# from an empty store with nothing else attached to it.
set -eu

repo=/srv/versioned-demo-repo
trees=/srv/worktrees
stamp=${HOME}/.cache/codebase-memory-mcp/.vacmcp-indexed

# The revisions, not the branch names: a branch that moved is a graph that no
# longer describes what the contexts point at.
built=$(git -C "$repo" rev-parse refs/heads/release/v1 refs/heads/release/v2 | tr '\n' ' ')
if [ -f "$stamp" ] && [ "$(cat "$stamp")" = "$built" ]; then
	echo "cbm-index: the store already holds the graphs of $built"
	exit 0
fi

# Re-running has to land on the same graphs as the first run, so the worktrees
# are rebuilt from scratch rather than added to whatever a previous run left.
rm -rf "$trees"
git -C "$repo" worktree prune

for release in v1 v2; do
	project="vacmcp-demo-$release"
	git -C "$repo" worktree add --detach -q "$trees/$project" "refs/heads/release/$release"
	codebase-memory-mcp cli index_repository --repo-path "$trees/$project" --name "$project"
done

codebase-memory-mcp cli --json list_projects

# Written last, so a run that did not get all the way through leaves no claim
# that the store is ready.
printf '%s' "$built" >"$stamp"
