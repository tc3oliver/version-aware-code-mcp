package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The per-repository lock, tested for the two properties it is there for: one
// repository's operations take turns, and two repositories' do not wait for
// each other. Both are asserted on the lock itself rather than through a
// command, so what they measure is the lock and not how long a git fetch or a
// CBM index happens to take. The commands are exercised against it in
// context_test.go, where real creates run concurrently.

// TestRepositoryLockAdmitsOneOperationPerRepository is AC #2: every operation on
// one repository is serialised.
//
// The counter is deliberately not atomic. Under -race an unsynchronised
// increment is what the detector reports, so this test fails twice over if the
// lock stops holding: once on the count it observed and once on the race.
func TestRepositoryLockAdmitsOneOperationPerRepository(t *testing.T) {
	s := openStore(t, t.TempDir())

	inside, most := 0, 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := withRepositoryLock(s, "demo", func() error {
				inside++
				if inside > most {
					most = inside
				}
				// Long enough that a lock that did not hold would have the
				// goroutines overlap rather than merely interleave.
				time.Sleep(2 * time.Millisecond)
				inside--
				return nil
			})
			if err != nil {
				t.Errorf("withRepositoryLock: %v", err)
			}
		}()
	}
	wg.Wait()

	if most != 1 {
		t.Errorf("%d operations were inside the lock of one repository at once, want 1", most)
	}
	if inside != 0 {
		t.Errorf("%d operations were left inside the lock, want none", inside)
	}
}

// TestRepositoryLockLetsDifferentRepositoriesRunAtOnce is AC #3: the lock is per
// repository, so two repositories overlap instead of taking the sum of their
// times. The margin is wide — half the serial time would already fail — so what
// this measures is whether they waited for each other at all, not how fast the
// machine is.
func TestRepositoryLockLetsDifferentRepositoriesRunAtOnce(t *testing.T) {
	s := openStore(t, t.TempDir())

	const hold = 300 * time.Millisecond
	started := time.Now()

	var wg sync.WaitGroup
	for _, name := range []string{"repo-a", "repo-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := withRepositoryLock(s, name, func() error {
				time.Sleep(hold)
				return nil
			})
			if err != nil {
				t.Errorf("withRepositoryLock(%s): %v", name, err)
			}
		}()
	}
	wg.Wait()

	if elapsed := time.Since(started); elapsed >= 2*hold {
		t.Errorf("two repositories took %v, want them to overlap rather than the %v serialising them would cost", elapsed, 2*hold)
	}
}

// TestRepositoryLockFileIsInTheDataDirectory keeps the lock where an operator
// can see what is holding one, and keeps a name that is not a usable path
// element from becoming a file outside the data directory.
func TestRepositoryLockFileIsInTheDataDirectory(t *testing.T) {
	data := t.TempDir()
	s := openStore(t, data)

	if err := withRepositoryLock(s, "demo", func() error { return nil }); err != nil {
		t.Fatalf("withRepositoryLock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "locks", "demo.lock")); err != nil {
		t.Errorf("no lock file for demo: %v", err)
	}

	for _, name := range []string{"../escape", "a/b", "..", ""} {
		called := false
		err := withRepositoryLock(s, name, func() error {
			called = true
			return nil
		})
		if err == nil {
			t.Errorf("withRepositoryLock(%q) returned nil, want an error", name)
		}
		if called {
			t.Errorf("withRepositoryLock(%q) ran the operation, want it refused before anything happened", name)
		}
	}
}
