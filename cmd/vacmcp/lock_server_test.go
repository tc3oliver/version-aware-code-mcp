//go:build unix

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The server lock, tested for the three properties decision-6 needs of it: a
// managed server shuts every artifact-changing command out, it shuts none of
// the read-only ones out, and management commands do not shut each other out.
//
// Unix only, because the lock between two processes is the file lock, and on
// the platforms without one there is nothing here to observe — lock_other.go
// says so, and TASK-38 is where that changes. What the tests below hold is the
// behaviour of the real lock, so they run where the real lock exists.
//
// None of them needs a repository, an index or a graph: the refusal happens
// before a command reads a single record, which is the point of it being the
// first thing each one does.

// TestManagementCommandsAreRefusedWhileAManagedServerRuns is AC #2: each of the
// four commands decision-6 names fails closed, with a message that says what to
// do about it rather than that something is locked.
func TestManagementCommandsAreRefusedWhileAManagedServerRuns(t *testing.T) {
	data := t.TempDir()
	release, err := holdServerLock(openStore(t, data))
	if err != nil {
		t.Fatalf("holdServerLock: %v", err)
	}
	defer release()

	// As a user types them, and with arguments that name nothing this data
	// directory has: the refusal comes before any record is read, so a command
	// that got as far as looking one up has already failed this.
	for _, command := range [][]string{
		{"context", "create", "alpha", "--repo", "demo", "--ref", "main"},
		{"context", "retry", "alpha"},
		{"context", "remove", "alpha"},
		{"repo", "remove", "demo"},
	} {
		name := strings.Join(command[:2], " ")
		t.Run(name, func(t *testing.T) {
			err := run(append(append([]string{}, command...), "--data-dir", data), io.Discard)
			if code := codeFor(t, err); code != vacerr.InvalidArgument {
				t.Fatalf("`vacmcp %s` while a server runs returned %s, want %s", name, code, vacerr.InvalidArgument)
			}
			// The message is the whole of what a user gets, so it is asserted
			// as one: the command that was refused, the server that refused it
			// and the way out of it.
			for _, want := range []string{name, "managed server is running", "serve --managed", "start the server again"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("`vacmcp %s` failed with %q, want it to mention %q", name, err, want)
				}
			}
		})
	}
}

// TestDiagnosisIsNotRefusedWhileAManagedServerRuns keeps TASK-35's principle:
// the lock is against changing a context, never against looking at one. A
// running server is exactly when somebody needs to.
func TestDiagnosisIsNotRefusedWhileAManagedServerRuns(t *testing.T) {
	data := t.TempDir()
	release, err := holdServerLock(openStore(t, data))
	if err != nil {
		t.Fatalf("holdServerLock: %v", err)
	}
	defer release()

	for _, command := range [][]string{{"context", "list"}, {"repo", "list"}} {
		if err := run(append(command, "--data-dir", data), io.Discard); err != nil {
			t.Errorf("`vacmcp %s` while a server runs: %v, want it to answer", strings.Join(command, " "), err)
		}
	}
	// The ones that take a name reach their record lookup and report on the
	// record, not on the server: any answer but CONTEXT_NOT_FOUND here would
	// mean they had been stopped before looking.
	for _, command := range [][]string{{"context", "status", "absent"}, {"context", "verify", "absent"}} {
		err := run(append(command, "--data-dir", data), io.Discard)
		if code := codeFor(t, err); code != vacerr.ContextNotFound {
			t.Errorf("`vacmcp %s` while a server runs returned %s, want %s", strings.Join(command, " "), code, vacerr.ContextNotFound)
		}
	}
}

// TestManagementCommandsDoNotExcludeEachOther is what keeps decision-4's
// per-repository concurrency: the server lock is taken shared by a management
// command, so two of them on two repositories still run at once. An exclusive
// one here would serialise the whole data directory and tell the second command
// a server is running that is not.
func TestManagementCommandsDoNotExcludeEachOther(t *testing.T) {
	s := openStore(t, t.TempDir())

	first, err := holdManagementLock(s, "context create")
	if err != nil {
		t.Fatalf("holdManagementLock: %v", err)
	}
	defer first()

	second, err := holdManagementLock(s, "repo remove")
	if err != nil {
		t.Fatalf("a second management command was refused while a first held the lock: %v", err)
	}
	second()
}

// TestTheServerLockIsFreeOnceTheServerHasStopped is the other half of AC #2:
// the refusal lasts exactly as long as the server does. The release is what a
// stopping server runs, and a killed one has the kernel do the same by closing
// its descriptor.
func TestTheServerLockIsFreeOnceTheServerHasStopped(t *testing.T) {
	data := t.TempDir()
	s := openStore(t, data)

	release, err := holdServerLock(s)
	if err != nil {
		t.Fatalf("holdServerLock: %v", err)
	}
	if _, err := holdManagementLock(s, "context remove"); err == nil {
		t.Fatal("a management command was admitted while a server held the lock, want it refused")
	}
	release()

	got, err := holdManagementLock(s, "context remove")
	if err != nil {
		t.Fatalf("a management command was still refused after the server released the lock: %v", err)
	}
	got()

	// And the lock is a file in the data directory, where an operator can see
	// what is holding one — the same place the per-repository locks are.
	if _, err := os.Stat(filepath.Join(data, "locks", ".server.lock")); err != nil {
		t.Errorf("no server lock file in the data directory: %v", err)
	}
}

// TestAServerWaitsForAManagementCommandRatherThanRefusingIt is the asymmetry
// itself. A management command holding the lock is a create or a removal that
// finishes in a moment, so the server takes its turn after it; the other
// direction cannot wait, because what it would be waiting for is a server that
// may run for weeks.
func TestAServerWaitsForAManagementCommandRatherThanRefusingIt(t *testing.T) {
	s := openStore(t, t.TempDir())

	command, err := holdManagementLock(s, "context create")
	if err != nil {
		t.Fatalf("holdManagementLock: %v", err)
	}

	served := make(chan error, 1)
	go func() {
		release, err := holdServerLock(s)
		if err == nil {
			release()
		}
		served <- err
	}()

	// Long enough that a lock that did not hold would have got through by now,
	// which is what makes the absence below mean waiting rather than slowness.
	time.Sleep(50 * time.Millisecond)
	select {
	case err := <-served:
		t.Fatalf("the server took the lock while a management command held it (err = %v), want it to wait", err)
	default:
	}

	command()
	if err := <-served; err != nil {
		t.Errorf("the server did not get the lock after the management command released it: %v", err)
	}
}
