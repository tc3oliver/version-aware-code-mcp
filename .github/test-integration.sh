#!/usr/bin/env bash
# decision-7's Real Engine Gate, run locally, with codebase-memory-mcp warm.
#
# Every CBM `cli` call otherwise starts a throwaway daemon and pays for it.
# Measured on this suite against 0.11.0: 1357s with no resident daemon and
# 657s with one, a 2.07x difference for work that is identical either way. CI
# has had that daemon since .github/actions/prepare-engines started one; `make
# test-integration` did not, so a developer running the gate paid twice what
# the gate guarding the branch pays.
#
# The daemon lifecycle is the whole of what this script adds, and the reason
# it is this careful is that codebase-memory-mcp's daemon belongs to the
# account, not to a cache directory. Three things follow, each verified rather
# than assumed:
#
#   - `daemon status` reports the account-wide daemon and says nothing about
#     which store it serves. Its output is identical whether the active daemon
#     is this fixture's or a developer's own.
#   - `daemon start` does not report having started anything. Asked while
#     another daemon is up it prints "already active" and exits 0.
#   - only a real `cli` call under this store's CBM_CACHE_DIR distinguishes
#     them, because only that call comes back with the cache conflict.
#
# So CBM_CACHE_DIR is a compatibility requirement here, never an ownership
# boundary, and ownership is never inferred from a command succeeding. It is
# claimed only when this run observed the daemon go from absent to present,
# and it is bound to the pid that appeared. Everything else — a daemon that
# was already up, a start that lost a race, a daemon that was replaced while
# the tests ran — is somebody else's process and is left alone.
#
# Not `set -e`: the exit status of the gate is the point of this script, and a
# shell that exits on the first failure cannot report it.
set -uo pipefail

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)

# Fixed rather than inherited. The status probe, the start, the tests and the
# stop have to agree on one store, and a value left over in an environment is
# exactly how they stop agreeing — with the tests answering from one store and
# the daemon warming another.
export CBM_CACHE_DIR="$root/testdata/fixture/cbm-data"

# CBM_BIN for the reason testdata/prepare-fixture.sh has it: CI points it at
# whatever it downloaded.
cbm=${CBM_BIN:-codebase-memory-mcp}

# The gate itself, and the one seam in this script. internal/ci drives the
# lifecycle below with a fake command so that control flow — ownership,
# cleanup, signals, exit status — is decided in milliseconds rather than by
# waiting out a twenty-minute suite once per case. Nothing else sets it.
test_command=${VACMCP_TEST_COMMAND:-"go test -tags=integration -timeout 30m ./..."}

owned=no
owned_pid=
child=

# Read into a variable and matched there, never piped into grep. Under
# pipefail a `grep -q` exits on its first match and SIGPIPEs the writer, so a
# status whose first line matches and which then keeps printing — which is
# exactly what an active daemon's status does — comes back as a failed
# pipeline. prepare-engines carries the same warning for the same reason.
daemon_status() { "$cbm" daemon status 2>/dev/null; }

daemon_active() {
	local status
	status=$(daemon_status)
	[[ $status =~ ^daemon:\ (active|already\ active) ]]
}

# daemon_pid is the pid status reports, and the identity ownership is bound to.
# Empty when there is no daemon or when a version stops reporting one — and an
# empty pid can never equal a recorded one, so it fails towards leaving the
# daemon alone.
daemon_pid() {
	local status
	status=$(daemon_status)
	sed -n 's/^[[:space:]]*pid:[[:space:]]*\([0-9][0-9]*\).*/\1/p' <<<"$status" | head -1
}

# serves_our_store reports whether whatever daemon is up answers for
# CBM_CACHE_DIR. A real call is the only thing that answers this.
serves_our_store() { printf '{"format":"json"}' | "$cbm" cli list_projects >/dev/null 2>&1; }

refuse_foreign_daemon() {
	echo "test-integration: A CBM daemon for another cache/build is already active." >&2
	echo "test-integration: Stop/close the existing CBM sessions before running make test-integration." >&2
	echo "test-integration:   $cbm daemon stop" >&2
	echo "test-integration: It is not this run's daemon to stop, and while it holds the account" >&2
	echo "test-integration: nothing can reach $CBM_CACHE_DIR." >&2
}

# finish stops the daemon if this run started it and it is still the same one,
# then exits with the status the gate produced.
#
# The pid is re-read rather than trusted. A daemon that died and was replaced
# while the tests ran is a different process that happens to occupy the same
# role, and stopping it would be this script reaching into a run that is not
# its own — the exact accident that makes an EXIT trap dangerous.
#
# A cleanup failure may not hide a test failure: the tests are what the run is
# about, and an operator told "could not stop the daemon" when the real news is
# a broken test has been told the wrong thing. It may not be hidden by a test
# success either — a daemon left behind after a green run is a process nobody
# owns — so it becomes the status only when there was no other bad news.
finish() {
	local status=$1
	trap - INT TERM

	if [ "$owned" = yes ]; then
		local now
		now=$(daemon_pid)
		if [ -n "$now" ] && [ "$now" = "$owned_pid" ]; then
			if ! "$cbm" daemon stop >/dev/null 2>&1; then
				echo "test-integration: the daemon this run started (pid $owned_pid) could not be stopped" >&2
				if [ "$status" -eq 0 ]; then
					status=1
				fi
			fi
		else
			echo "test-integration: the daemon changed during the run (started pid $owned_pid, now ${now:-none})." >&2
			echo "test-integration: leaving it alone: it is not the process this run started." >&2
		fi
	fi
	exit "$status"
}

# interrupted ends the gate on a signal and still cleans up. The tests run in
# the background and are waited on precisely so this can happen while they are
# running rather than after they finish: a shell does not run a trap while a
# foreground child is running, which would make Ctrl-C wait out the very suite
# it was meant to stop.
interrupted() {
	local name=$1 number=$2
	echo "" >&2
	echo "test-integration: $name, stopping the tests" >&2
	if [ -n "$child" ]; then
		# SIGTERM whichever signal arrived, rather than forwarding it. With job
		# control off — which is every non-interactive shell, so every `make`
		# — bash starts asynchronous children with SIGINT ignored, so a
		# forwarded SIGINT is discarded and the wait below then blocks for as
		# long as the suite would have taken. The status reported still names
		# the signal the operator actually sent.
		kill -TERM "$child" 2>/dev/null
		wait "$child" 2>/dev/null
	fi
	finish $((128 + number))
}
trap 'interrupted INT 2' INT
trap 'interrupted TERM 15' TERM

if daemon_active; then
	# Case 1: something was already running. It is reusable or it is a
	# refusal; it is never this run's to stop.
	if ! serves_our_store; then
		refuse_foreign_daemon
		exit 1
	fi
	echo "test-integration: reusing the codebase-memory-mcp daemon already running (pid $(daemon_pid))"
else
	# Case 2: nothing was running, so this run tries to start one. What the
	# start says decides ownership: "already active" means another process won
	# the race between the status above and the start below, and a daemon this
	# run cannot prove it created is not this run's to stop.
	start_output=$("$cbm" daemon start 2>&1)
	if ! serves_our_store; then
		refuse_foreign_daemon
		exit 1
	fi
	if grep -qE 'already active' <<<"$start_output"; then
		echo "test-integration: a codebase-memory-mcp daemon appeared while this run was starting one;"
		echo "test-integration: reusing it and leaving it running (pid $(daemon_pid))"
	else
		owned=yes
		owned_pid=$(daemon_pid)
		echo "test-integration: started a codebase-memory-mcp daemon for this run (pid ${owned_pid:-unknown})"
	fi
fi

eval "$test_command" &
child=$!
wait "$child"
finish $?
