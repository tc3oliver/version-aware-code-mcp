#!/usr/bin/env bash
# Fails when a `go test -v` log contains a skipped test.
#
# Every skip in this repository means an engine or the fixture was missing:
# demorepo.Prepared skips when testdata/prepare-fixture.sh has not run, and the
# Zoekt and CBM tests skip when their binary is not on PATH. That is right for a
# developer without them installed, and wrong for CI — measured on an unprepared
# checkout, `go test ./...` reports ten skips and still exits 0, so the whole
# integration layer can go unrun while the job stays green. decision-3 says a
# mock may not stand in for an integration result; a silent skip is the same
# claim with less to show for it.
set -euo pipefail

log=${1:?usage: assert-no-skips.sh <go test -v log>}

if grep -q -- '--- SKIP' "$log"; then
	echo "::error::tests were skipped, so nothing they cover was verified; the fixture or an engine is missing" >&2
	grep -B1 -- '--- SKIP' "$log" >&2
	exit 1
fi

echo "assert-no-skips: no test skipped in $log"
