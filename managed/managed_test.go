package managed

import (
	"errors"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The helpers the tests of this package share. What each of them tests is in
// the file named after it: the lifecycle's own rules in context_test.go, the
// locks in lock_test.go and lock_server_test.go, and what a remote URL is
// allowed to carry in repository_test.go. The commands built on all of it are
// exercised against the real engines from cmd/vacmcp.

// openStore returns a store on dataDir, for the assertions that read records
// rather than results.
func openStore(t *testing.T, dataDir string) *store.Store {
	t.Helper()
	s, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open(%s): %v", dataDir, err)
	}
	return s
}

// codeFor returns the vacerr code err carries.
func codeFor(t *testing.T, err error) vacerr.Code {
	t.Helper()
	return errorFor(t, func() error { return err }).Code
}

// errorFor returns the *vacerr.Error a call failed with.
func errorFor(t *testing.T, call func() error) *vacerr.Error {
	t.Helper()
	err := call()
	if err == nil {
		t.Fatal("got no error, want one")
	}
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		t.Fatalf("error = %v (%T), want *vacerr.Error", err, err)
	}
	return vErr
}
