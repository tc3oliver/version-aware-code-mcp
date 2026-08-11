package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stdioHelperEnv makes this test binary run as a vacmcp STDIO server instead of
// running tests, so the STDIO transport is exercised against a real subprocess
// over real pipes rather than simulated in-process.
const stdioHelperEnv = "VACMCP_TEST_STDIO_SERVER"

const testVersion = "0.0.0-test"

func TestMain(m *testing.M) {
	if os.Getenv(stdioHelperEnv) == "1" {
		// Returning before m.Run keeps the testing package's own output off
		// stdout, which here carries the JSON-RPC stream.
		if err := ServeStdio(context.Background(), New(testVersion)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestServeStdioDiscovery(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), stdioHelperEnv+"=1")
	cmd.Stderr = os.Stderr

	assertDiscoverable(t, connect(t, &mcp.CommandTransport{Command: cmd}))
}

func TestServeHTTPDiscovery(t *testing.T) {
	httpServer := httptest.NewServer(handler(New(testVersion)))
	t.Cleanup(httpServer.Close)

	assertDiscoverable(t, connect(t, &mcp.StreamableClientTransport{Endpoint: httpServer.URL}))
}

func TestListenAddressDefaultsToLoopback(t *testing.T) {
	for _, addr := range []string{"", "   "} {
		if got := listenAddress(addr); got != DefaultAddress {
			t.Errorf("listenAddress(%q) = %q, want %q", addr, got, DefaultAddress)
		}
	}
	if got := listenAddress("0.0.0.0:9000"); got != "0.0.0.0:9000" {
		t.Errorf("listenAddress kept %q, want the address as given", got)
	}
}

func connect(t *testing.T, transport mcp.Transport) *mcp.ClientSession {
	t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp-test", Version: testVersion}, nil)
	session, err := client.Connect(t.Context(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// assertDiscoverable checks that the session was established by server/discover
// and that the server is usable with an empty tool registry.
func assertDiscoverable(t *testing.T, session *mcp.ClientSession) {
	t.Helper()

	res := session.InitializeResult()
	if res == nil {
		t.Fatal("session has no discovery result")
	}
	// Only server/discover can reach 2026-07-28: the legacy initialize
	// handshake the client falls back to negotiates 2025-11-25 at best.
	if res.ProtocolVersion != "2026-07-28" {
		t.Errorf("protocol version = %q, want 2026-07-28", res.ProtocolVersion)
	}

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools.Tools) != 0 {
		t.Errorf("tools/list returned %d tools, want an empty registry", len(tools.Tools))
	}
}
