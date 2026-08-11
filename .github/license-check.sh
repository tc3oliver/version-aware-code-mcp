#!/usr/bin/env bash
# Fails when THIRD_PARTY_NOTICES.md and the modules actually linked into the
# vacmcp binary disagree.
#
# The notices file exists to tell a user what they are running and under which
# licenses. A dependency that appears in the binary but not in the file is an
# unattributed license; an entry left in the file after its module is dropped is
# a claim about code that is no longer shipped. Both are wrong, so both fail.
#
# "Actually linked" is `go list -deps ./cmd/vacmcp`, not `go.mod`: go.mod also
# carries modules reached only from tests, which are not distributed. The module
# itself is dropped — the project is not its own third party.
set -euo pipefail

# Both sides go through sort, so they have to collate the same way on a
# developer's machine and on the CI runner alike.
export LC_ALL=C

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
notices="$root/THIRD_PARTY_NOTICES.md"
module=$(cd "$root" && go list -m)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

(cd "$root" && go list -deps -f '{{if and .Module (not .Standard)}}{{.Module.Path}} {{.Module.Version}}{{end}}' ./cmd/vacmcp) |
	grep -v "^$module " |
	sort -u >"$work/linked"

# The bundled-dependency table rows are `| [<module path>](<url>) | <version> | <license> |`.
# Anything else in the file — prose, the external-services table, headings — has
# no module link in the first column and is skipped.
sed -n 's/^| \[\([^]]*\)\]([^)]*) | \([^ |]*\) |.*/\1 \2/p' "$notices" | sort -u >"$work/declared"

if diff -u "$work/declared" "$work/linked" >"$work/diff"; then
	echo "license-check: THIRD_PARTY_NOTICES.md matches the $(wc -l <"$work/linked" | tr -d ' ') modules linked into vacmcp"
	exit 0
fi

echo "license-check: THIRD_PARTY_NOTICES.md does not match the modules linked into vacmcp." >&2
echo "  -declared: listed in THIRD_PARTY_NOTICES.md but not linked into the binary" >&2
echo "  +linked:   linked into the binary but missing from THIRD_PARTY_NOTICES.md" >&2
sed '1,2d' "$work/diff" >&2
exit 1
