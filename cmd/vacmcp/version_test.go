package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionComesFromTheBuild pins the contract .github/release-build.sh and
// the release workflow depend on, from both ends.
//
// A release binary is built with `-X main.version=<tag>`, and nothing else in
// the source says which release it is. Rename the variable, move the package,
// or turn it back into a const, and the linker flag silently does nothing:
// every archive of every release would then report 0.0.0-dev, and a user
// holding a binary would have no way to tell which one they have. Asking the
// built binary is the only check that catches that, so it is asked here rather
// than left to the release itself.
func TestVersionComesFromTheBuild(t *testing.T) {
	const injected = "v9.9.9-injection-test"

	tests := map[string]struct {
		ldflags string
		want    string
	}{
		"a release build reports the injected version": {"-X main.version=" + injected, injected},
		// The other half: a build nobody linked a version into has to say so
		// rather than claim a release it is not.
		"a developer build reports the development version": {"", "0.0.0-dev"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "vacmcp")

			args := []string{"build", "-o", binary}
			if tc.ldflags != "" {
				args = append(args, "-ldflags", tc.ldflags)
			}
			if out, err := exec.Command("go", append(args, ".")...).CombinedOutput(); err != nil {
				t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, out)
			}

			out, err := exec.Command(binary, "version").Output()
			if err != nil {
				t.Fatalf("vacmcp version: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != tc.want {
				t.Errorf("vacmcp version = %q, want %q", got, tc.want)
			}
		})
	}
}
