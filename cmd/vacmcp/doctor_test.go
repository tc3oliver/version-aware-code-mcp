package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
)

// The rows doctor is asked about here. The dependencies come first, then the
// contexts of the prepared fixture.
var everyRow = []string{sdkCheck, configCheck, zoektCheck, cbmCheck, gitCheck, "demo-v1", "demo-v2"}

// TestDoctorReportsEverythingAvailable is AC #1: with a real Zoekt web server
// over the fixture's index, the installed CBM, and the demo repository on disk,
// every dependency and every context is reported OK and the command exits zero.
func TestDoctorReportsEverythingAvailable(t *testing.T) {
	out, err := doctorRun(t, fixtureConfig(t, zoektAt(t)))
	if err != nil {
		t.Fatalf("doctor returned %v, want a clean run:\n%s", err, out)
	}
	for _, row := range everyRow {
		if got := status(t, out, row); got != statusOK {
			t.Errorf("%s = %s, want %s", row, got, statusOK)
		}
	}
	t.Logf("doctor with everything available:\n%s", out)
}

// TestDoctorReportsZoektDownWithoutHidingTheRest is AC #2, and the reason this
// command exists: a user has to be able to tell which dependency is broken, so
// an unreachable Zoekt may not stop CBM, git and the contexts from being
// checked and reported. The condition is real rather than simulated — the
// address is one nobody is listening on.
func TestDoctorReportsZoektDownWithoutHidingTheRest(t *testing.T) {
	out, err := doctorRun(t, fixtureConfig(t, "http://"+closedAddress(t)))
	if err == nil {
		t.Fatalf("doctor returned nil, want an error so the CLI exits non-zero:\n%s", out)
	}

	if got := status(t, out, zoektCheck); got != statusFail {
		t.Errorf("%s = %s, want %s", zoektCheck, got, statusFail)
	}
	// The row has to say what is wrong with it, not only that something is.
	if !strings.Contains(out, "SEARCH_PROVIDER_UNAVAILABLE") {
		t.Errorf("the Zoekt row does not name the failure:\n%s", out)
	}

	for _, row := range everyRow {
		if row == zoektCheck {
			continue
		}
		if got := status(t, out, row); got != statusOK {
			t.Errorf("%s = %s, want %s: one failing check must not suppress the others", row, got, statusOK)
		}
	}
	t.Logf("doctor with Zoekt unreachable:\n%s", out)
}

// TestDoctorReportsAnOldCBM is AC #3: a CBM below the 0.10.1 decision-3 pins is
// reported as a version mismatch, and nothing else is affected by it.
//
// The installed binary is 0.10.1 and cannot report anything else, so the
// configuration points at a stand-in that prints an older version string. That
// fakes no integration — doctor traces no graph — it supplies the one input
// this check parses.
func TestDoctorReportsAnOldCBM(t *testing.T) {
	out, err := doctorRun(t, fixtureConfig(t, zoektAt(t), fakeCBM(t, "0.9.9", "daemon: active")))
	if err == nil {
		t.Fatalf("doctor returned nil, want an error so the CLI exits non-zero:\n%s", out)
	}

	if got := status(t, out, cbmCheck); got != statusFail {
		t.Errorf("%s = %s, want %s", cbmCheck, got, statusFail)
	}
	for _, want := range []string{"0.9.9", minCBM} {
		if !strings.Contains(out, want) {
			t.Errorf("the CBM row does not mention %s:\n%s", want, out)
		}
	}

	for _, row := range everyRow {
		if row == cbmCheck {
			continue
		}
		if got := status(t, out, row); got != statusOK {
			t.Errorf("%s = %s, want %s", row, got, statusOK)
		}
	}
	t.Logf("doctor with CBM 0.9.9:\n%s", out)
}

// TestDoctorReportsACBMWithNoResidentDaemon covers the cost a working CBM hides.
// Without a resident daemon it cold-starts a temporary one per call and the
// first trace_calls takes over ten seconds, which reads as a hang; a user who
// is not told that debugs a CBM that is fine. The row stays OK — it is slow,
// not broken — so this is reported in the detail and not as a failure.
func TestDoctorReportsACBMWithNoResidentDaemon(t *testing.T) {
	cbmPath := fakeCBM(t, minCBM, "daemon: inactive")
	out, err := doctorRun(t, fixtureConfig(t, zoektAt(t), cbmPath))
	if err != nil {
		t.Fatalf("doctor returned %v, want a clean run: a cold CBM works\n%s", err, out)
	}
	if got := status(t, out, cbmCheck); got != statusOK {
		t.Errorf("%s = %s, want %s", cbmCheck, got, statusOK)
	}
	if !strings.Contains(out, cbmPath+" daemon start") {
		t.Errorf("the CBM row does not say how to keep a daemon warm:\n%s", out)
	}
	t.Logf("doctor with a cold CBM:\n%s", out)
}

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

// fixtureConfig writes a copy of the prepared fixture's configuration with its
// Zoekt URL, and optionally its CBM command, replaced.
//
// The fixture names the fixed port a long-running Zoekt would listen on, so a
// test that brings its own server up — or that needs one genuinely unreachable
// — has to rewrite that value rather than assume it.
func fixtureConfig(t *testing.T, zoektURL string, cbmCommand ...string) string {
	t.Helper()
	fixture := demorepo.Prepared(t)

	cfg, err := config.Load(fixture.Config)
	if err != nil {
		t.Fatalf("config.Load(%s) error = %v", fixture.Config, err)
	}
	data, err := os.ReadFile(fixture.Config)
	if err != nil {
		t.Fatalf("read %s: %v", fixture.Config, err)
	}

	body := string(data)
	replacements := map[string]string{cfg.Providers.Zoekt.URL: zoektURL}
	if len(cbmCommand) > 0 {
		replacements[cfg.Providers.CBM.Command] = cbmCommand[0]
	}
	for old, new := range replacements {
		replaced := strings.Replace(body, old, new, 1)
		if replaced == body {
			t.Fatalf("%s does not contain %q", fixture.Config, old)
		}
		body = replaced
	}
	return write(t, body)
}

// zoektAt starts a Zoekt web server over the fixture's index for the duration
// of the test and returns its URL. AC #1 is about the real engine: a stub could
// not show that doctor's probe reaches the JSON API zoekt-webserver only serves
// with -rpc.
func zoektAt(t *testing.T) string {
	t.Helper()
	return demorepo.StartZoekt(t, demorepo.Prepared(t).ZoektIndex)
}

// fakeCBM writes a stand-in for codebase-memory-mcp answering --version and
// `daemon status` as told, and returns its path.
//
// It fakes no integration: doctor traces no graph, it reads these two strings.
// They are the inputs the installed binary cannot produce — it is the version
// decision-3 pins, and stopping its daemon to observe the cold state would stop
// it for everything else on the machine.
func fakeCBM(t *testing.T, reported, daemon string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codebase-memory-mcp")
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"--version) echo 'codebase-memory-mcp " + reported + "' ;;\n" +
		"daemon) echo '" + daemon + "' ;;\n" +
		"*) exit 1 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// closedAddress returns a loopback address that was free and is now unbound: a
// port guaranteed to refuse a connection.
func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing %s: %v", address, err)
	}
	return address
}
