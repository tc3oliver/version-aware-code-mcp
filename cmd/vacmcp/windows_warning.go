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
// `serve --managed` warns too, and for a reason that used to be argued the
// other way round here: that serve takes the server lock rather than being
// refused by one, and that a server which cannot hold it does not start. That
// is true wherever the lock is real. On Windows lockExclusive is a no-op and
// tryLockShared always succeeds (managed/lock_other.go), so serve starts having
// taken nothing, and the refusal decision-6 relies on never happens — a
// `context remove` run against the data directory it is serving is allowed
// through. The one command that holds the lock for its whole run was the one
// command saying nothing about the lock not being there.
func warnOnWindows(w io.Writer, command string) {
	if goos != "windows" {
		return
	}
	// Best effort: a warning that failed to print is not a reason to stop the
	// command it is warning about.
	_, _ = fmt.Fprintf(w, "vacmcp: warning: %s (see README.md, Managed Mode)\n", windowsLockWarning(command))
}

// windowsLockWarning is the sentence for command, which is the same limitation
// seen from whichever end command is at: a management command is not kept apart
// from anything, and a server does not keep anything away from itself.
func windowsLockWarning(command string) string {
	if command == serveManagedCommand {
		return "on Windows, `vacmcp " + serveManagedCommand + "` holds no cross-process lock on its data directory;" +
			" management commands are not refused while it runs, so a `context` or `repo` command can change" +
			" the artifacts it is serving underneath it"
	}
	return "on Windows, `vacmcp " + command + "` locks only within this process;" +
		" do not run it against the same data directory as another vacmcp management command" +
		" or a running `vacmcp " + serveManagedCommand + "`"
}

// serveManagedCommand is how the managed server names itself in the warning,
// kept in one place because both sentences above say it.
const serveManagedCommand = "serve --managed"
