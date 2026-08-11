package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

const twoContexts = `
repositories:
  example/backend:
    path: /srv/repos/backend
contexts:
  app-v2:
    repository: example/backend
    branch: release/2.x
    revision: 94cb821
    graph_ref: backend-v2
  app-v1:
    repository: example/backend
    branch: release/1.x
    revision: 8af31e2
    graph_ref: backend-v1
`

// missingRevision drops a required field, which is the shape of configuration
// error `vacmcp validate` exists to catch.
const missingRevision = `
repositories:
  example/backend:
    path: /srv/repos/backend
contexts:
  app-v1:
    repository: example/backend
    branch: release/1.x
    graph_ref: backend-v1
`

// write puts body in a temporary file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

func TestValidateAcceptsTheExampleConfig(t *testing.T) {
	var out bytes.Buffer
	// The shipped example is the configuration users copy, so it is the one
	// worth checking really validates.
	if err := run([]string{"validate", "--config", "../../config/example.yaml"}, &out); err != nil {
		t.Fatalf("validate example.yaml: %v", err)
	}
	if !strings.Contains(out.String(), "ok, 2 contexts") {
		t.Errorf("validate printed %q, want it to report 2 contexts", out.String())
	}
}

func TestValidateRejectsIncompleteConfig(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"validate", "--config", write(t, missingRevision)}, &out)
	if err == nil {
		t.Fatal("validate returned nil, want an error so the CLI exits non-zero")
	}

	// The error has to reach the user as config produced it: a validate that
	// invented its own error model would hide the vacerr code.
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("validate error = %v (%T), want *vacerr.Error", err, err)
	}
	if vErr.Code != vacerr.InvalidArgument {
		t.Errorf("validate error code = %q, want %q", vErr.Code, vacerr.InvalidArgument)
	}
	if !strings.Contains(vErr.Message, "revision") {
		t.Errorf("validate message = %q, want it to name the missing field", vErr.Message)
	}
	if out.Len() != 0 {
		t.Errorf("validate wrote %q to stdout on failure, want nothing", out.String())
	}
}

func TestValidateRequiresConfigPath(t *testing.T) {
	if err := run([]string{"validate"}, &bytes.Buffer{}); err == nil {
		t.Fatal("validate without --config returned nil, want an error")
	}
}

func TestContextsListsTheConfiguredContexts(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"contexts", "--config", write(t, twoContexts)}, &out); err != nil {
		t.Fatalf("contexts: %v", err)
	}

	want := "app-v1\texample/backend\trelease/1.x\t8af31e2\n" +
		"app-v2\texample/backend\trelease/2.x\t94cb821\n"
	if out.String() != want {
		t.Errorf("contexts output =\n%q\nwant\n%q", out.String(), want)
	}
}

func TestVersionPrintsAVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatalf("version: %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Error("version printed nothing, want a version string")
	}
}

func TestUnknownCommandFails(t *testing.T) {
	for _, args := range [][]string{{}, {"doctor"}} {
		if err := run(args, &bytes.Buffer{}); err == nil {
			t.Errorf("run(%q) returned nil, want an error", args)
		}
	}
}
