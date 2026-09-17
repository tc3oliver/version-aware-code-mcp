package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// What a graceful shutdown has to be worth: the request that was already being
// answered when the signal arrived gets to finish and reach its client, and the
// serve call does not return until it has. Both halves matter — a Shutdown that
// returned immediately would look identical from the outside to the
// ListenAndServe this replaced, which severed everything.

// TestServeHTTPContextDrainsAnInFlightRequest cancels the context while a tool
// call is mid-flight and asserts the client still gets its answer.
//
// The tool blocks until this test releases it, and it is released only after
// the cancellation, so the answer arriving proves the request was carried
// across the shutdown rather than having finished before it started.
func TestServeHTTPContextDrainsAnInFlightRequest(t *testing.T) {
	srv := New(testVersion)
	entered := make(chan struct{})
	release := make(chan struct{})
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "slow",
		Description: "blocks until the test lets it answer",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		close(entered)
		<-release
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "drained"}}}, struct{}{}, nil
	})

	addr := freeAddress(t)
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()

	served := make(chan error, 1)
	go func() { served <- ServeHTTPContext(ctx, srv, addr) }()
	waitUntilAccepting(t, addr)

	session := connect(t, &mcp.StreamableClientTransport{Endpoint: "http://" + addr})

	answered := make(chan error, 1)
	go func() {
		// context.Background(), not t.Context(): the point is that the server
		// finishes this on its own account, so the client must not be the
		// reason it survives.
		_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "slow"})
		answered <- err
	}()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the tool was never entered, so nothing was in flight to drain")
	}

	shutdown()

	// The listener is closed first, so a new connection is refused while the
	// one already being served is still being answered. Asserting it here is
	// what distinguishes a drain from the server simply not having stopped.
	waitUntilRefusing(t, addr)

	close(release)

	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("the in-flight tools/call failed across the shutdown: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the in-flight tools/call never answered")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("ServeHTTPContext = %v, want nil after a drain that finished", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ServeHTTPContext never returned after its context was cancelled")
	}
}

// TestServeHTTPContextReturnsWhenTheListenerFails is the other way out, and it
// is here because the drain watcher must not swallow it: a bind failure has to
// come back as itself rather than as a nil from a shutdown nobody asked for.
func TestServeHTTPContextReturnsWhenTheListenerFails(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	err = ServeHTTPContext(context.Background(), New(testVersion), occupied.Addr().String())
	if err == nil {
		t.Fatal("ServeHTTPContext = nil on an address already in use, want the listener's error")
	}
	if errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("ServeHTTPContext = %v, want the bind failure rather than the stop sentinel", err)
	}
}

// TestServeHTTPKeepsItsSignature is the compatibility half: the context-free
// entry point is still there and still serves, so an embedder calling it is not
// broken by the one that takes a context.
func TestServeHTTPKeepsItsSignature(t *testing.T) {
	addr := freeAddress(t)
	served := make(chan error, 1)
	go func() { served <- ServeHTTP(New(testVersion), addr) }()
	waitUntilAccepting(t, addr)

	assertDiscoverable(t, connect(t, &mcp.StreamableClientTransport{Endpoint: "http://" + addr}))
}

// freeAddress returns a loopback address nothing is listening on, by taking one
// and giving it back. A port can in principle be claimed in between, which is
// why the callers below wait for the server to actually accept rather than
// assuming it bound.
func freeAddress(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func waitUntilAccepting(t *testing.T, addr string) {
	t.Helper()

	for range 200 {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing was listening on %s", addr)
}

func waitUntilRefusing(t *testing.T, addr string) {
	t.Helper()

	for range 200 {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s was still accepting connections after shutdown was asked for", addr)
}
