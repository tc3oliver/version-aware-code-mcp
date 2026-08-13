package main

import (
	"bytes"
	"testing"
)

// The context commands are exercised end to end in context_integration_test.go,
// against real git, Zoekt and CBM. The lifecycle's own rules — which state may
// follow which, and which records the query plane may read — belong to the
// managed package and are checked in managed/context_test.go, so what is left
// here is the dispatch this file does.

func TestContextRejectsUnknownSubcommands(t *testing.T) {
	for _, args := range [][]string{{"context"}, {"context", "update"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%q) returned nil, want an error", args)
		}
	}
}
