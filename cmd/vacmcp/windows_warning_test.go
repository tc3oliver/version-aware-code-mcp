package main

import (
	"bytes"
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
