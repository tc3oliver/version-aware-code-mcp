package cbm

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The two backends and the rule for choosing between them are tested here,
// inside the package, because the choice is not visible from outside it: both
// modes answer identically by design. What is visible from outside — that the
// answer is right, and comes from the right graph — is in the cbm_test files,
// against a real codebase-memory-mcp.

// TestABrokenSessionFallsBackToTheCLI drives the case a running server hits
// rather than a starting one: the session was up, and then CBM went away. The
// call must not fail on the dead connection while a working `cli` mode sits
// there unused, and the dead session must not be handed to the next caller.
func TestABrokenSessionFallsBackToTheCLI(t *testing.T) {
	// A command that cannot exist, so the fallback is unmistakable: if the
	// error names this path, the CLI was reached; if it names the transport,
	// it was not.
	absent := filepath.Join(t.TempDir(), "codebase-memory-mcp")
	p := &Provider{command: absent}
	p.session = deadSession(t)
	broken := p.session

	_, err := p.call(t.Context(), "search_graph", map[string]any{"project": "vacmcp-demo-v1"})
	if err == nil {
		t.Fatal("call() succeeded against a dead session and an absent binary")
	}
	if !strings.Contains(err.Error(), absent) {
		t.Errorf("call() failed with %v, want the failure of the cli fallback at %s", err, absent)
	}

	if p.session == broken {
		t.Error("the dead session is still installed; the next call would fail on it too")
	}
}

// TestACancelledCallLeavesTheSessionAlone is why cancellation is checked
// before the connection is blamed. One client giving up fails its own call
// however healthy CBM is; if that counted as a broken connection, that client
// would retire the session every other caller is sharing and make all of them
// wait for a new CBM to start.
func TestACancelledCallLeavesTheSessionAlone(t *testing.T) {
	p := &Provider{command: filepath.Join(t.TempDir(), "codebase-memory-mcp")}
	p.session = deadSession(t)
	shared := p.session

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := p.call(ctx, "search_graph", map[string]any{"project": "vacmcp-demo-v1"}); err == nil {
		t.Fatal("call() succeeded on a cancelled context")
	}
	if p.session != shared {
		t.Error("a cancelled call retired the shared session")
	}
}

// TestAFailedStartIsNotRetriedForever pins the other half of the choice: a CBM
// that cannot be started as a server is not started again before every call.
// The failed attempt costs whatever the operating system takes to refuse it,
// and paying that in front of each of the three calls a trace makes would make
// the fallback slower than the mode it is falling back to.
func TestAFailedStartIsNotRetriedForever(t *testing.T) {
	p := &Provider{command: filepath.Join(t.TempDir(), "codebase-memory-mcp")}

	if session := p.persistent(t.Context()); session != nil {
		t.Fatal("persistent() returned a session for a binary that does not exist")
	}
	if !p.cliOnly {
		t.Error("a failed start did not switch the provider to the cli mode")
	}
	if session := p.persistent(t.Context()); session != nil {
		t.Error("persistent() tried again after giving up")
	}
}

// TestFlagsSpellTheCLIArguments pins the one place the two backends differ: the
// same parameters, sent as flags. A name spelled wrong here reaches CBM as an
// unknown flag only when the fallback is in use, which is exactly when nobody
// is watching.
func TestFlagsSpellTheCLIArguments(t *testing.T) {
	got := flags(map[string]any{
		"project":           "vacmcp-demo-v1",
		"name_pattern":      "^Process$",
		"limit":             1000,
		"include_connected": true,
		"format":            "json",
	})
	want := []string{
		"--format", "json",
		"--include-connected", "true",
		"--limit", "1000",
		"--name-pattern", "^Process$",
		"--project", "vacmcp-demo-v1",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("flags() = %q\nwant %q", got, want)
	}
}

// deadSession returns a client session whose server has stopped: a connection
// that was healthy and is not any more.
func deadSession(t *testing.T) *mcp.ClientSession {
	t.Helper()

	clientSide, serverSide := mcp.NewInMemoryTransports()
	srv := mcp.NewServer(&mcp.Implementation{Name: "stand-in-cbm", Version: "0"}, nil)
	serverSession, err := srv.Connect(t.Context(), serverSide, nil)
	if err != nil {
		t.Fatalf("connecting the stand-in server: %v", err)
	}

	session, err := mcp.NewClient(&mcp.Implementation{Name: "vacmcp", Version: "0"}, nil).
		Connect(t.Context(), clientSide, nil)
	if err != nil {
		t.Fatalf("connecting to the stand-in server: %v", err)
	}

	if err := serverSession.Close(); err != nil {
		t.Fatalf("closing the stand-in server: %v", err)
	}
	return session
}
