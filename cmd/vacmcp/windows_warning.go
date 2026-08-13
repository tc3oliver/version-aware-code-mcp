package main

import (
	"fmt"
	"io"
	"runtime"
)

// goos is runtime.GOOS, held in a var so a test can stand on the Windows
// branch below without a Windows runner to run it on.
var goos = runtime.GOOS

// warnOnWindows tells w once that the Management Plane's per-repository lock
// (cmd/vacmcp/lock.go) is not cross-process on this platform.
//
// The lock is a mutex and a file lock together everywhere else, because the
// mutex alone protects nothing between two separate `vacmcp` invocations —
// see lock.go. Windows has no such file lock yet (TASK-38), so on Windows it
// is the mutex alone, good for nothing outside this one process. That is a
// real limitation on a real safety property, not an implementation detail, so
// it is said here rather than left for README.md alone to carry: a warning at
// the moment a management command runs is read by someone who is about to be
// affected by it, which a paragraph of documentation is not.
//
// It is only for `repo` and `context`, the Management Plane commands the lock
// protects. `serve` never takes this lock and is unaffected by this limitation
// on every platform, so it never calls this.
func warnOnWindows(w io.Writer, command string) {
	if goos != "windows" {
		return
	}
	fmt.Fprintf(w, "vacmcp: warning: on Windows, `vacmcp %s` locks only within this process; do not run concurrent vacmcp management commands against the same repository from separate processes (see README.md, Managed Mode)\n", command)
}
