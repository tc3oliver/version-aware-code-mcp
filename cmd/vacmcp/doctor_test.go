package main

import (
	"bytes"
	"strings"
	"testing"
)

// The checks doctor can make without an engine: a configuration that does not
// load, and the version comparison the CBM row is decided by. The rows that
// probe a real Zoekt, a real CBM and the prepared fixture are in
// doctor_integration_test.go.

// TestDoctorSeparatesABrokenConfigFromTheEngines is the fourth thing doc-1 §9
// wants told apart. A configuration that does not load leaves the engines'
// endpoints unknown, so they are reported as not checked rather than as
// failures — reporting Zoekt as broken here would send the user to debug an
// engine that is running perfectly.
func TestDoctorSeparatesABrokenConfigFromTheEngines(t *testing.T) {
	out, err := doctorRun(t, write(t, missingRevision))
	if err == nil {
		t.Fatalf("doctor returned nil, want an error so the CLI exits non-zero:\n%s", out)
	}

	// MCP needs neither a port nor a configuration, so it stays answerable.
	if got := status(t, out, sdkCheck); got != statusOK {
		t.Errorf("%s = %s, want %s", sdkCheck, got, statusOK)
	}
	if got := status(t, out, configCheck); got != statusFail {
		t.Errorf("%s = %s, want %s", configCheck, got, statusFail)
	}
	if !strings.Contains(out, "revision") {
		t.Errorf("the Config row does not name the missing field:\n%s", out)
	}
	for _, row := range []string{zoektCheck, cbmCheck, gitCheck} {
		if got := status(t, out, row); got != statusSkip {
			t.Errorf("%s = %s, want %s", row, got, statusSkip)
		}
	}
	t.Logf("doctor with a configuration that does not load:\n%s", out)
}

func TestDoctorRequiresConfigPath(t *testing.T) {
	if err := run([]string{"doctor"}, &bytes.Buffer{}); err == nil {
		t.Fatal("doctor without --config returned nil, want an error")
	}
}

func TestAtLeastComparesReleases(t *testing.T) {
	for _, c := range []struct {
		got, want string
		pass      bool
	}{
		{"0.10.1", "0.10.1", true},
		{"0.10.2", "0.10.1", true},
		{"0.11.0", "0.10.1", true},
		{"1.0.0", "0.10.1", true},
		{"0.10.0", "0.10.1", false},
		{"0.10", "0.10.1", false},
		{"0.9.9", "0.10.1", false},
		// A plain 0.9 must not win on "9 > 10" read as strings.
		{"0.9", "0.10.1", false},
	} {
		if got := atLeast(c.got, c.want); got != c.pass {
			t.Errorf("atLeast(%q, %q) = %v, want %v", c.got, c.want, got, c.pass)
		}
	}
	// A string carrying no version is not a version, which is what makes an
	// unreadable --version distinguishable from an old one.
	if len(release("codebase-memory-mcp")) != 0 {
		t.Errorf("release(%q) = %v, want no numbers", "codebase-memory-mcp", release("codebase-memory-mcp"))
	}
}

// doctorRun runs the command and returns everything it printed, whether or not
// it succeeded: the report is the answer, and it has to be there on failure too.
func doctorRun(t *testing.T, configPath string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run([]string{"doctor", "--config", configPath}, &out)
	return out.String(), err
}

// status returns the status doctor reported for the named row.
func status(t *testing.T, out, name string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, name) {
			continue
		}
		if fields := strings.Fields(strings.TrimPrefix(line, name)); len(fields) > 0 {
			return fields[0]
		}
	}
	t.Fatalf("doctor reported no row for %q:\n%s", name, out)
	return ""
}
