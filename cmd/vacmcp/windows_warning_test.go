package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// onGOOS stands run on goos as if it were g, and restores it after, so the
// Windows branch of warnOnWindows is reachable without a Windows runner.
func onGOOS(t *testing.T, g string) {
	t.Helper()
	previous := goos
	goos = g
	t.Cleanup(func() { goos = previous })
}

func TestWarnOnWindowsOnlyWarnsOnWindows(t *testing.T) {
	for _, g := range []string{"linux", "darwin"} {
		onGOOS(t, g)
		var out bytes.Buffer
		warnOnWindows(&out, "repo")
		if out.Len() != 0 {
			t.Errorf("GOOS=%s: warnOnWindows wrote %q, want nothing", g, out.String())
		}
	}

	onGOOS(t, "windows")
	var out bytes.Buffer
	warnOnWindows(&out, "repo")
	if !strings.Contains(out.String(), "vacmcp repo") || !strings.Contains(out.String(), "Windows") {
		t.Errorf("GOOS=windows: warnOnWindows wrote %q, want a warning naming the command and Windows", out.String())
	}
}

func TestWarnOnWindowsNamesTheCommandItWasCalledFor(t *testing.T) {
	onGOOS(t, "windows")
	var out bytes.Buffer
	warnOnWindows(&out, "context")
	if !strings.Contains(out.String(), "vacmcp context") {
		t.Errorf("warnOnWindows(context) wrote %q, want it to name `vacmcp context`", out.String())
	}
}

// TestServeManagedWarnsOnWindows is the gap this file did not cover: `serve
// --managed` is the command that holds the lock for its whole run, and on
// Windows it holds nothing — so it is the command that most needs to say so,
// and it was the one saying nothing.
func TestServeManagedWarnsOnWindows(t *testing.T) {
	onGOOS(t, "windows")
	var out bytes.Buffer
	warnOnWindows(&out, serveManagedCommand)

	got := out.String()
	// The sentence is the server's own, not the management command's: what a
	// reader needs here is that nothing is kept away from the directory being
	// served, which the "do not run it against" phrasing does not say.
	for _, want := range []string{
		"vacmcp serve --managed",
		"no cross-process lock",
		"not refused",
		"README.md",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the serve --managed warning is %q, want it to mention %q", got, want)
		}
	}
	if strings.Contains(got, "locks only within this process") {
		t.Errorf("the serve --managed warning is %q, want the server's sentence rather than a management command's", got)
	}
}

// TestServeManagedIsSilentOffWindows keeps the warning from becoming noise on
// the platforms where the lock is real.
func TestServeManagedIsSilentOffWindows(t *testing.T) {
	for _, g := range []string{"linux", "darwin"} {
		onGOOS(t, g)
		var out bytes.Buffer
		warnOnWindows(&out, serveManagedCommand)
		if out.Len() != 0 {
			t.Errorf("GOOS=%s: serve --managed wrote %q, want nothing", g, out.String())
		}
	}
}

// TestManagementWarningStillNamesItsCommand pins the other sentence, so the
// split above did not quietly change what `repo` and `context` say.
func TestManagementWarningStillNamesItsCommand(t *testing.T) {
	onGOOS(t, "windows")
	for _, command := range []string{"repo", "context"} {
		var out bytes.Buffer
		warnOnWindows(&out, command)
		got := out.String()
		if !strings.Contains(got, "vacmcp "+command) || !strings.Contains(got, "locks only within this process") {
			t.Errorf("the %s warning is %q, want it to name the command and its in-process locking", command, got)
		}
	}
}

// TestServeManagedCallsTheWarning is the one that would catch the call site
// being dropped. Testing the sentence alone proves a function nobody calls.
//
// serve is made to fail immediately by pointing --data-dir at a file, so this
// never starts a server: the warning is printed before the store is opened, for
// the same reason it has to exist at all — on Windows nothing later fails, so
// there is no other moment to attach it to.
func TestServeManagedCallsTheWarning(t *testing.T) {
	onGOOS(t, "windows")

	notADirectory := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(notADirectory, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := captureStderr(t, func() {
		// The error is the point of the path, not the subject of the test:
		// whatever serve does with a data directory it cannot open, it must
		// already have warned.
		_ = run([]string{"serve", "--managed", "--data-dir", notADirectory}, io.Discard)
	})

	if !strings.Contains(got, "vacmcp serve --managed") || !strings.Contains(got, "no cross-process lock") {
		t.Errorf("serve --managed wrote %q to stderr, want the Windows locking warning", got)
	}
}

// captureStderr runs f with os.Stderr replaced by a pipe and returns what was
// written to it. The warning goes to os.Stderr by name, so this is the only way
// to observe the call site rather than the function.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	previous := os.Stderr
	os.Stderr = write
	defer func() { os.Stderr = previous }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, read)
		done <- buf.String()
	}()

	f()

	if err := write.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	out := <-done
	if err := read.Close(); err != nil {
		t.Fatalf("close read: %v", err)
	}
	return out
}
