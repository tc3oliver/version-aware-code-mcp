// Package ci checks this repository's own CI wiring rather than the code it
// ships. It has no non-test files: nothing here is built into the binary.
package ci

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

// workflow is the part of a GitHub Actions workflow this check reads. Parsing
// the YAML rather than grepping the file is what keeps a `go test` written in a
// comment — there are several — from being mistaken for a step that runs one.
type workflow struct {
	Jobs map[string]struct {
		Steps []struct {
			Name string `yaml:"name"`
			Run  string `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

// TestEveryGoTestStepAssertsNoSkips is the check that keeps assert-no-skips.sh
// honest. That script protects exactly the steps that call it, and a step that
// does not is a step whose skips are invisible: most skips in this repository
// mean an engine or the fixture was missing, so a green job that skipped them
// verified nothing it claims to. ci-fast.yml's Race test step was that step
// until TASK-89 — the fix was one line, and this test is the only thing that
// stops the next step from making the same omission.
//
// A step needs both halves to be covered: the call to the script, and -v,
// without which a skip prints no `--- SKIP` line for the script to find.
func TestEveryGoTestStepAssertsNoSkips(t *testing.T) {
	dir := filepath.Join("..", "..", ".github", "workflows")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	steps := 0
	for _, entry := range entries {
		if ext := filepath.Ext(entry.Name()); ext != ".yml" && ext != ".yaml" {
			continue
		}
		path := filepath.Join(dir, entry.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		var w workflow
		if err := yaml.Unmarshal(data, &w); err != nil {
			t.Fatalf("%s: %v", path, err)
		}

		for job, j := range w.Jobs {
			for _, step := range j.Steps {
				if !strings.Contains(step.Run, "go test") {
					continue
				}
				steps++

				where := path + ": job " + job + ", step " + step.Name
				if !slices.Contains(goTestFlags(step.Run), "-v") {
					t.Errorf("%s: runs `go test` without -v, so a skip leaves no `--- SKIP` line for assert-no-skips.sh to find", where)
				}
				if !strings.Contains(step.Run, "assert-no-skips.sh") {
					t.Errorf("%s: runs `go test` without handing the output to .github/assert-no-skips.sh, so a skipped test would keep the job green", where)
				}
			}
		}
	}

	// Without this the check would report success exactly when it had stopped
	// working: a renamed directory, a workflow schema change or a typo in the
	// `jobs` mapping leaves it finding no steps at all — which is the same kind
	// of silent nothing-was-verified it exists to catch.
	if steps == 0 {
		t.Fatalf("found no `go test` step in %s, so this check verified nothing", dir)
	}
}

// goTestFlags returns the flags the `go test` invocation itself carries.
//
// It reads only as far as the first command substitution, because the command
// inside one has flags of its own: every step here selects its packages with
// `$(go list ./... | grep -v '/integration$')`, and that grep -v would answer
// for go test's missing -v. Anything a step puts after the substitution is not
// read, which is fine while -v is the only flag asked about — go test wants its
// flags before the package list anyway.
func goTestFlags(run string) []string {
	_, after, found := strings.Cut(run, "go test")
	if !found {
		return nil
	}
	line, _, _ := strings.Cut(after, "\n")
	beforeSubstitution, _, _ := strings.Cut(line, "$(")
	return strings.Fields(beforeSubstitution)
}
