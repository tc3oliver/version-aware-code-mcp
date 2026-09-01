package main

import (
	"bytes"
	"flag"
	"slices"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/managed"
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

// TestCreatePairsEveryRepositoryWithItsOwnRef is what `--repo A --ref a --repo
// B --ref b` means, decided before anything is resolved: the nth --ref belongs
// to the nth --repo, in the order they were typed.
//
// A count that does not match is refused rather than paired as far as it goes.
// Pinning a repository to the wrong ref would be a context that is silently a
// version nobody asked for, and there is nothing in the two lists that says
// which pairing was meant.
func TestCreatePairsEveryRepositoryWithItsOwnRef(t *testing.T) {
	got, err := pinsOf("stack", []string{"api", "web"}, []string{"main", "release/2.x"})
	if err != nil {
		t.Fatalf("pinsOf: %v", err)
	}
	want := []managed.Pin{{Repository: "api", Ref: "main"}, {Repository: "web", Ref: "release/2.x"}}
	if !slices.Equal(got, want) {
		t.Errorf("pinsOf = %+v, want %+v", got, want)
	}

	for name, args := range map[string]struct {
		id           string
		repositories []string
		refs         []string
	}{
		"no name":            {"", []string{"api"}, []string{"main"}},
		"no repository":      {"stack", nil, []string{"main"}},
		"no ref":             {"stack", []string{"api"}, nil},
		"a repository spare": {"stack", []string{"api", "web"}, []string{"main"}},
		"a ref spare":        {"stack", []string{"api"}, []string{"main", "release/2.x"}},
		"an empty ref":       {"stack", []string{"api"}, []string{" "}},
	} {
		if _, err := pinsOf(args.id, args.repositories, args.refs); err == nil {
			t.Errorf("%s: pinsOf returned nil, want an error", name)
		}
	}
}

// TestRepeatableKeepsEveryValue is why --repo is not a string flag: the flag
// package keeps only the last value of one, which would silently drop every
// repository of a context but the last.
func TestRepeatableKeepsEveryValue(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var repositories repeatable
	fs.Var(&repositories, "repo", "")
	if err := fs.Parse([]string{"--repo", "api", "--repo", "web"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !slices.Equal([]string(repositories), []string{"api", "web"}) {
		t.Errorf("--repo api --repo web = %v, want both", repositories)
	}
}
