#!/usr/bin/env bash
# Fails when a `go test -v` log contains a skipped test, with one exception.
#
# Every other skip in this repository means an engine or the fixture was
# missing: demorepo.Prepared skips when testdata/prepare-fixture.sh has not
# run, and the Zoekt and CBM tests skip when their binary is not on PATH. That
# is right for a developer without them installed, and wrong for CI — measured
# on an unprepared checkout, `go test ./...` reports ten skips and still exits
# 0, so the whole integration layer can go unrun while the job stays green.
# decision-3 says a mock may not stand in for an integration result; a silent
# skip is the same claim with less to show for it.
#
# TestCrashWriterChild (store/store_test.go) is not that: it is the body of
# the process TestKilledWriteLeavesNoHalfWrittenRecord re-execs and kills
# mid-write, and it skips itself with exactly that message when it is run any
# other way — which `go test ./...` always does, once, in addition to running
# it for real inside the re-exec. That skip is the test working as designed,
# not a missing engine, so it is the one line this check accepts rather than
# failing a build that verified everything it claims to.
set -euo pipefail

log=${1:?usage: assert-no-skips.sh <go test -v log>}

skips=$(grep -- '--- SKIP' "$log" | grep -v 'TestCrashWriterChild' || true)

if [ -n "$skips" ]; then
	echo "::error::tests were skipped, so nothing they cover was verified; the fixture or an engine is missing" >&2
	echo "$skips" >&2
	exit 1
fi

echo "assert-no-skips: no test skipped in $log (TestCrashWriterChild's self-skip is expected and excluded)"
