//go:build !unix

package managed

import "os"

// lockExclusive is the in-process lock alone where there is no flock.
//
// The mutex in lock.go still serialises this process's own operations, so the
// lifecycle is correct for one vacmcp at a time. Two of them on one data
// directory are not serialised here, and the managed lifecycle is not supported
// on such a platform: release-build.sh publishes a windows binary so `vacmcp
// serve --config` runs there, which is the query plane and touches none of this.
func lockExclusive(*os.File) error { return nil }

// tryLockShared says the lock was free, because on a platform with no file
// lock nothing here can find out otherwise.
//
// That makes the server lock of lock.go, and with it the refusal to change a
// context while a managed server is running, a guarantee this platform does
// not have: two processes are not held apart by anything. It is reported at
// the moment it matters rather than left to be discovered — `vacmcp repo` and
// `vacmcp context` print it (see windows_warning.go) — and TASK-38 is where
// the platform gets a real lock.
func tryLockShared(*os.File) (bool, error) { return true, nil }
