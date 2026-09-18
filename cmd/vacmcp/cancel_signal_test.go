//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The managed layer has taken a context for as long as it has existed; what was
// missing was anyone giving it one that could be cancelled. Every `repo` and
// `context` command called it with context.Background(), so exec.CommandContext
// had nothing to cancel on and Ctrl-C reached the CLI and stopped there — the
// operator's terminal came back while git carried on cloning behind it.
//
// This drives the real CLI as a subprocess and sends it a real signal, because
// nothing smaller observes that: the managed tests cancel a context directly,
// which proves the layer below this one.

// TestSIGINTDuringRepoAddStopsTheClone is the whole defect in one test: the
// command exits, and the git it started is gone rather than left behind it.
func TestSIGINTDuringRepoAddStopsTheClone(t *testing.T) {
	data := t.TempDir()
	stubDir, pidFile := gitStub(t)

	child := exec.Command(os.Args[0], "repo", "add", "demo",
		"--url", "https://example.invalid/demo.git", "--data-dir", data)
	child.Env = append(os.Environ(),
		cliCommandEnv+"=1",
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	child.Stdout = os.Stderr
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatalf("start `vacmcp repo add`: %v", err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })

	clone := waitForPid(t, pidFile)

	if err := child.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("`vacmcp repo add` did not exit after SIGINT")
	}

	// The command returning is not the point; the clone stopping is. Before this
	// change the process below outlived the command that started it.
	for range 300 {
		if err := syscall.Kill(clone, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(clone, syscall.SIGKILL)
	t.Fatalf("the git clone (pid %d) was still running after `vacmcp repo add` was interrupted", clone)
}

// gitStub writes a `git` that records its pid and parks, and returns the
// directory to put in front of PATH. `exec sleep` so the pid recorded is the one
// exec.CommandContext will kill rather than a shell holding a sleeping child.
func gitStub(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "git.pid")
	script := fmt.Sprintf("#!/bin/sh\necho $$ >%q\nexec sleep 300\n", pidFile)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil {
		t.Fatalf("write the git stub: %v", err)
	}
	return dir, pidFile
}

func waitForPid(t *testing.T, pidFile string) int {
	t.Helper()

	for range 600 {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the git stub at %s never started", pidFile)
	return 0
}
