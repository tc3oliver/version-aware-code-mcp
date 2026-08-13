package main

import (
	"fmt"
	"io"
	"runtime"
)

// goos is runtime.GOOS, held in a var so a test can stand on the Windows
// branch below without a Windows runner to run it on.
var goos = runtime.GOOS

// warnOnWindows tells w once that the Management Plane's locks
// (cmd/vacmcp/lock.go) are not cross-process on this platform.
//
// Both of them are file locks everywhere else, because nothing inside one
// process is a lock between two separate `vacmcp` invocations — see lock.go.
// Windows has no such file lock yet (TASK-38), so on Windows neither of them
// holds outside the process that took it: two management commands are not kept
// apart from each other, and neither is a management command kept away from a
// running `vacmcp serve --managed`, which decision-6 has it refuse. Those are
// real limitations on real safety properties, not implementation details, so
// they are said here rather than left for README.md alone to carry: a warning
// at the moment a management command runs is read by someone who is about to
// be affected by it, which a paragraph of documentation is not.
//
// It is only for `repo` and `context`, the Management Plane commands the locks
// protect. `serve` takes the server lock rather than being refused by one, and
// a server that cannot hold it does not start at all, so it never calls this.
func warnOnWindows(w io.Writer, command string) {
	if goos != "windows" {
		return
	}
	// Best effort: a warning that failed to print is not a reason to stop the
	// command it is warning about.
	_, _ = fmt.Fprintf(w, "vacmcp: warning: on Windows, `vacmcp %s` locks only within this process; do not run it against the same data directory as another vacmcp management command or a running `vacmcp serve --managed` (see README.md, Managed Mode)\n", command)
}
