//go:build unix

package ci

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The daemon lifecycle .github/test-integration.sh owns, driven with a fake
// codebase-memory-mcp and a fake gate so the control flow is decided in
// milliseconds.
//
// The real thing cannot answer these questions. Asking whether a run leaves a
// developer's daemon alone by running the actual suite costs eleven minutes a
// case, and the interesting cases are the ones where the run fails, is
// interrupted, or races — precisely when a twenty-minute suite is least
// deterministic and least welcome. What is under test here is shell control
// flow: which daemon gets stopped, by whom, and with what exit status.
//
// The rule every case below is a face of: a daemon is stopped only when this
// run watched it come into existence and it is still the same process. CBM's
// daemon belongs to the account rather than to a cache directory, so nothing
// else — not a successful status, not a successful start, not a working
// probe — is evidence of ownership.

// TestTheGateStartsAndStopsADaemonItOwns is the ordinary path: nothing was
// running, so the run warms one and takes it away again.
func TestTheGateStartsAndStopsADaemonItOwns(t *testing.T) {
	cbm := fakeCBM(t, noDaemon, servesOurStore)

	out, status := runGate(t, cbm, "exit 0")

	if status != 0 {
		t.Fatalf("the gate exited %d, want 0\n%s", status, out)
	}
	if cbm.running() {
		t.Error("the daemon this run started is still running")
	}
	if !cbm.did("start") || !cbm.did("stop") {
		t.Errorf("daemon calls were %v, want a start and a stop", cbm.calls())
	}
}

// TestTheGateLeavesADaemonItDidNotStart is the rule that matters most day to
// day: a developer who keeps a daemon warm gets it back.
func TestTheGateLeavesADaemonItDidNotStart(t *testing.T) {
	cbm := fakeCBM(t, daemonAt(100), servesOurStore)

	out, status := runGate(t, cbm, "exit 0")

	if status != 0 {
		t.Fatalf("the gate exited %d, want 0\n%s", status, out)
	}
	if !cbm.running() {
		t.Error("the gate stopped a daemon it did not start")
	}
	if cbm.did("stop") {
		t.Errorf("daemon calls were %v, want no stop at all", cbm.calls())
	}
	if !strings.Contains(out, "reusing") {
		t.Errorf("the gate did not say it was reusing the daemon:\n%s", out)
	}
}

// TestTheGateRefusesAForeignDaemon: status cannot tell whose daemon is up and
// start reports success either way, so another store's daemon is a refusal.
// Neither stopping it nor running the tests through it would work.
func TestTheGateRefusesAForeignDaemon(t *testing.T) {
	cbm := fakeCBM(t, daemonAt(100), servesAnotherStore)

	out, status := runGate(t, cbm, "exit 0")

	if status == 0 {
		t.Fatalf("the gate succeeded against another store's daemon\n%s", out)
	}
	if !cbm.running() {
		t.Error("the gate stopped a daemon belonging to another store")
	}
	if cbm.did("start") {
		t.Errorf("daemon calls were %v, want no start: a start would report success and adopt it", cbm.calls())
	}
	for _, want := range []string{"another cache/build is already active", "Stop/close the existing CBM sessions"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
}

// TestTheGateDoesNotClaimADaemonItLostTheRaceToStart: between the status that
// said nothing was running and the start that followed, another process can
// have started one. The start says "already active" rather than failing, so
// that string is the only evidence this run did not create it — and a daemon
// this run cannot prove it created is not this run's to stop.
func TestTheGateDoesNotClaimADaemonItLostTheRaceToStart(t *testing.T) {
	cbm := fakeCBM(t, noDaemon, servesOurStore)
	cbm.loseTheStartRace(200)

	out, status := runGate(t, cbm, "exit 0")

	if status != 0 {
		t.Fatalf("the gate exited %d, want it to reuse a compatible daemon and pass\n%s", status, out)
	}
	if !cbm.running() {
		t.Error("the gate stopped a daemon that appeared while it was starting one")
	}
	if cbm.did("stop") {
		t.Errorf("daemon calls were %v, want no stop: ownership was never established", cbm.calls())
	}
	if !strings.Contains(out, "appeared while this run was starting one") {
		t.Errorf("the gate did not report the lost race:\n%s", out)
	}
}

// TestTheGateDoesNotStopADaemonThatWasReplaced binds ownership to the process
// rather than to the role. Our daemon dying and another taking its place is
// not our daemon, and an EXIT trap that stopped it anyway would reach into
// somebody else's run.
func TestTheGateDoesNotStopADaemonThatWasReplaced(t *testing.T) {
	cbm := fakeCBM(t, noDaemon, servesOurStore)

	// The fake gate replaces the daemon while "the tests" run.
	out, status := runGate(t, cbm, cbm.replaceDaemonWith(200))

	if status != 0 {
		t.Fatalf("the gate exited %d, want 0\n%s", status, out)
	}
	if !cbm.running() {
		t.Error("the gate stopped the daemon that replaced its own")
	}
	if cbm.did("stop") {
		t.Errorf("daemon calls were %v, want no stop once the pid changed", cbm.calls())
	}
	if !strings.Contains(out, "changed during the run") {
		t.Errorf("the gate did not report that the daemon changed:\n%s", out)
	}
}

// TestTheGateKeepsTheTestStatusAndStillCleansUp: the gate's own exit status is
// what the run is about, and cleanup must not overwrite it.
func TestTheGateKeepsTheTestStatusAndStillCleansUp(t *testing.T) {
	cbm := fakeCBM(t, noDaemon, servesOurStore)

	out, status := runGate(t, cbm, "exit 3")

	if status != 3 {
		t.Fatalf("the gate exited %d, want the tests' own 3\n%s", status, out)
	}
	if cbm.running() {
		t.Error("a failing run left its daemon behind")
	}
}

// TestACleanupFailureIsReportedOnlyWhenNothingElseWentWrong is the other half
// of that rule. A daemon left running after a green run is a process nobody
// owns, so it has to be news — but only when it is the only news.
func TestACleanupFailureIsReportedOnlyWhenNothingElseWentWrong(t *testing.T) {
	t.Run("tests passed", func(t *testing.T) {
		cbm := fakeCBM(t, noDaemon, servesOurStore)
		cbm.failStop()

		out, status := runGate(t, cbm, "exit 0")

		if status == 0 {
			t.Fatalf("a daemon that could not be stopped was reported as success\n%s", out)
		}
	})

	t.Run("tests failed", func(t *testing.T) {
		cbm := fakeCBM(t, noDaemon, servesOurStore)
		cbm.failStop()

		out, status := runGate(t, cbm, "exit 3")

		if status != 3 {
			t.Fatalf("the gate exited %d, want the tests' own 3 rather than the cleanup's\n%s", status, out)
		}
	})
}

// TestASignalEndsTheTestsAndStillCleansUp covers both signals an operator or a
// supervisor sends, and checks that the signal's own status survives cleanup.
func TestASignalEndsTheTestsAndStillCleansUp(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		signal syscall.Signal
	}{
		{"SIGINT", syscall.SIGINT},
		{"SIGTERM", syscall.SIGTERM},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cbm := fakeCBM(t, noDaemon, servesOurStore)
			marker := filepath.Join(t.TempDir(), "gate-started")

			// The fake gate announces itself and parks, so the signal lands
			// while it is running rather than after it has finished.
			cmd := gateCommand(t, cbm, fmt.Sprintf("touch %q; sleep 120", marker))
			// Its own process group, so the signal reaches the script
			// deliberately rather than everything sharing this terminal.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting the gate: %v", err)
			}

			waitFor(t, func() bool { _, err := os.Stat(marker); return err == nil },
				"the fake gate never started")
			if err := cmd.Process.Signal(testCase.signal); err != nil {
				t.Fatalf("signalling the gate: %v", err)
			}

			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			var waitErr error
			select {
			case waitErr = <-done:
			case <-time.After(30 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				t.Fatal("the gate did not return on the signal; it waited for the tests instead")
			}

			if cbm.running() {
				t.Error("an interrupted run left its daemon behind")
			}
			// The signal's status, not cleanup's: an interrupted run is not a
			// successful one, and it is not a cleanup failure either.
			var exit *exec.ExitError
			if !errors.As(waitErr, &exit) {
				t.Fatalf("the gate exited with %v, want a non-zero status naming the signal", waitErr)
			}
			if want := 128 + int(testCase.signal); exit.ExitCode() != want {
				t.Errorf("the gate exited %d, want %d", exit.ExitCode(), want)
			}
		})
	}
}

// --- the fake codebase-memory-mcp -------------------------------------------

const (
	noDaemon           = 0
	servesOurStore     = true
	servesAnotherStore = false
)

// daemonAt is a daemon already running under the given pid.
func daemonAt(pid int) int { return pid }

type stubCBM struct {
	t    *testing.T
	dir  string
	pid  string // holds the running daemon's pid; absent means none
	log  string
	path string
}

// fakeCBM writes a codebase-memory-mcp answering the three daemon subcommands
// and the one `cli` call the script makes, recording what it was asked so a
// test can assert on the calls rather than only on the end state.
//
// The daemon's pid lives in a file, which is what lets a test start one, race
// one, or replace one mid-run — the three ways ownership can be wrong.
func fakeCBM(t *testing.T, running int, ours bool) *stubCBM {
	t.Helper()

	dir := t.TempDir()
	stub := &stubCBM{
		t:    t,
		dir:  dir,
		pid:  filepath.Join(dir, "daemon.pid"),
		log:  filepath.Join(dir, "calls"),
		path: filepath.Join(dir, "codebase-memory-mcp"),
	}

	conflict := ""
	if !ours {
		conflict = `>&2 echo "CBM could not start because the active account daemon uses a different cache directory"; exit 1`
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$1 $2" >>%[2]q
case "$1 $2" in
"daemon status")
	if [ -f %[1]q ]; then
		echo "daemon: active (permanent)"
		echo "  pid: $(cat %[1]q)"
	else
		echo "daemon: not running"
	fi ;;
"daemon start")
	if [ -f %[5]q ]; then
		# Another process won the race between status and start.
		cat %[5]q >%[1]q
		echo "daemon: already active (permanent, pid $(cat %[1]q))"
		exit 0
	fi
	if [ -f %[1]q ]; then
		echo "daemon: already active (permanent, pid $(cat %[1]q))"
		exit 0
	fi
	echo 100 >%[1]q
	echo "daemon: active (permanent)" ;;
"daemon stop")
	if [ -f %[3]q ]; then echo "refusing to stop" >&2; exit 1; fi
	rm -f %[1]q; echo "daemon: stopping" ;;
"cli list_projects")
	cat >/dev/null
	%[4]s
	printf '{"projects":[]}\n' ;;
esac
`, stub.pid, stub.log, filepath.Join(dir, "stop.fails"), conflict, filepath.Join(dir, "race.pid"))

	if err := os.WriteFile(stub.path, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the CBM stub: %v", err)
	}
	if running != noDaemon {
		stub.write(stub.pid, fmt.Sprint(running))
	}
	return stub
}

// loseTheStartRace makes the next `daemon start` find a daemon already there,
// as it does when another process starts one first.
func (s *stubCBM) loseTheStartRace(pid int) {
	s.t.Helper()
	s.write(filepath.Join(s.dir, "race.pid"), fmt.Sprint(pid))
}

// replaceDaemonWith returns a fake gate command that swaps the running daemon
// for a different process while the tests are notionally running.
func (s *stubCBM) replaceDaemonWith(pid int) string {
	return fmt.Sprintf("echo %d > %q", pid, s.pid)
}

// failStop makes `daemon stop` refuse, which is how the cleanup-failure cases
// are reached without breaking anything else.
func (s *stubCBM) failStop() {
	s.t.Helper()
	s.write(filepath.Join(s.dir, "stop.fails"), "")
}

func (s *stubCBM) write(path, content string) {
	s.t.Helper()
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		s.t.Fatalf("writing %s: %v", path, err)
	}
}

func (s *stubCBM) running() bool {
	_, err := os.Stat(s.pid)
	return err == nil
}

func (s *stubCBM) calls() []string {
	raw, err := os.ReadFile(s.log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func (s *stubCBM) did(subcommand string) bool {
	for _, call := range s.calls() {
		if call == "daemon "+subcommand {
			return true
		}
	}
	return false
}

// --- running the script ------------------------------------------------------

func gateCommand(t *testing.T, cbm *stubCBM, gate string) *exec.Cmd {
	t.Helper()

	// Two levels up: the tests of this package run in internal/ci.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locating the repository root: %v", err)
	}
	script := filepath.Join(root, ".github", "test-integration.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("%s: %v", script, err)
	}

	cmd := exec.Command(script)
	cmd.Env = append(os.Environ(), "CBM_BIN="+cbm.path, "VACMCP_TEST_COMMAND="+gate)
	return cmd
}

func runGate(t *testing.T, cbm *stubCBM, gate string) (string, int) {
	t.Helper()

	out, err := gateCommand(t, cbm, gate).CombinedOutput()
	status := 0
	var exit *exec.ExitError
	if err != nil {
		if !errors.As(err, &exit) {
			t.Fatalf("running the gate: %v\n%s", err, out)
		}
		status = exit.ExitCode()
	}
	return string(out), status
}

func waitFor(t *testing.T, ready func() bool, whenNot string) {
	t.Helper()
	for range 600 {
		if ready() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(whenNot)
}
