//go:build unix

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/managed"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The commands decision-6 is about, run as a user types them while a managed
// server holds the lock: the artifact-changing ones fail closed and the
// read-only ones answer. The lock's own behaviour — that it is shared on the
// management side, and free again once the server stops — is in
// managed/lock_server_test.go.
//
// Unix only, because the lock between two processes is the file lock, and on
// the platforms without one there is nothing here to observe — lock_other.go
// says so, and TASK-38 is where that changes.
//
// None of them needs a repository, an index or a graph: the refusal happens
// before a command reads a single record, which is the point of it being the
// first thing each one does.

// TestManagementCommandsAreRefusedWhileAManagedServerRuns is AC #2: each of the
// four commands decision-6 names fails closed, with a message that says what to
// do about it rather than that something is locked.
func TestManagementCommandsAreRefusedWhileAManagedServerRuns(t *testing.T) {
	data := t.TempDir()
	release, err := managed.HoldServerLock(data)
	if err != nil {
		t.Fatalf("HoldServerLock: %v", err)
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
	release, err := managed.HoldServerLock(data)
	if err != nil {
		t.Fatalf("HoldServerLock: %v", err)
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
