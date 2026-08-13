//go:build unix

package managed

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The server lock's own behaviour: a running server shuts every
// artifact-changing operation out, and management operations do not shut each
// other out. That the commands really are refused, and that the read-only ones
// are not, is in cmd/vacmcp's lock_server_test.go, where they are run as a user
// types them.
//
// Unix only, because the lock between two processes is the file lock, and on
// the platforms without one there is nothing here to observe — lock_other.go
// says so, and TASK-38 is where that changes. What the tests below hold is the
// behaviour of the real lock, so they run where the real lock exists.

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

	release, err := HoldServerLock(data)
	if err != nil {
		t.Fatalf("HoldServerLock: %v", err)
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
	data := t.TempDir()
	s := openStore(t, data)

	command, err := holdManagementLock(s, "context create")
	if err != nil {
		t.Fatalf("holdManagementLock: %v", err)
	}

	served := make(chan error, 1)
	go func() {
		release, err := HoldServerLock(data)
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
