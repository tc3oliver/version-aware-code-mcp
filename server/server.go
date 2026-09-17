// Package server runs the vacmcp MCP server over the transports an agent can
// reach it on: STDIO for a local agent that spawns vacmcp as a subprocess, and
// Streamable HTTP for a shared or containerised deployment.
//
// The JSON-RPC wire is entirely the official MCP Go SDK's job. This package
// only builds the server, mounts it on a transport and serves it.
//
// The server this package builds is also the tool registry: tools attach to it
// with [mcp.AddTool] before it is served. A server with none attached is still
// a valid MCP server that starts and answers discovery with an empty tool list.
package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DefaultAddress is the address Streamable HTTP listens on when none is
// configured. vacmcp is local-first, so the default is the loopback interface:
// reaching the server from the network is something an operator has to ask for
// explicitly.
const DefaultAddress = "127.0.0.1:8080"

// New returns the vacmcp MCP server with no tools registered, identifying
// itself to clients as vacmcp at the given version.
//
// The empty ServerCapabilities is not a no-op: left nil, the SDK advertises
// `logging` by default "for historical reasons", and vacmcp never sends a log
// notification, so that is a capability it does not have. Claiming it breaks
// real clients — MCP Inspector 2.1.0 sets a log level on every server that
// advertises `logging`, and 2026-07-28 removed `logging/setLevel` (SEP-2577),
// so the client's own guard fails the connection before a single tool can be
// called. The tools capability is still inferred from the registered tools.
func New(version string) *mcp.Server {
	return mcp.NewServer(
		&mcp.Implementation{Name: "vacmcp", Version: version},
		&mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}},
	)
}

// ServeStdio serves srv over STDIO, speaking newline-delimited JSON-RPC on
// stdin and stdout. It returns when the client disconnects or ctx is done.
//
// Nothing else may write to stdout while this runs: stdout is the protocol
// stream, and any stray output corrupts it.
func ServeStdio(ctx context.Context, srv *mcp.Server) error {
	return srv.Run(ctx, &mcp.StdioTransport{})
}

// drainTimeout bounds how long [ServeHTTPContext] waits for the requests that
// were already in flight when shutdown was asked for.
//
// It is a budget for finishing work, not for starting any: the listener is
// closed first, so nothing new arrives while it runs. A query this server
// answers is bounded by its providers rather than by a client's patience, and
// the slowest of them is a CBM cold start measured at over ten seconds — so a
// budget under that would routinely sever the one request shutdown is supposed
// to protect. When it expires, Shutdown reports the deadline and the remaining
// connections are dropped rather than waited on for ever.
const drainTimeout = 30 * time.Second

// ServeHTTP serves srv over Streamable HTTP on addr, or on [DefaultAddress]
// when addr is empty. It blocks until the listener fails.
//
// It never returns on a signal, because it has no context to be told about one
// through. A caller that wants to stop this server cleanly wants
// [ServeHTTPContext].
func ServeHTTP(srv *mcp.Server, addr string) error {
	return ServeHTTPContext(context.Background(), srv, addr)
}

// ServeHTTPContext serves srv over Streamable HTTP on addr, or on
// [DefaultAddress] when addr is empty, until the listener fails or ctx is done.
//
// When ctx is done it stops the way an HTTP server is supposed to: the listener
// closes so nothing new is accepted, the requests already in flight are given
// [drainTimeout] to finish and answer their clients, and only then does this
// return. That is the whole reason it exists — http.ListenAndServe cannot be
// asked to stop, so a server built on it has no way to let a caller's deferred
// cleanup run before the process goes.
//
// A drain that does not finish inside the budget is reported, not hidden: the
// error is Shutdown's, so a caller that logs it learns that connections were
// dropped rather than being told the shutdown was clean.
func ServeHTTPContext(ctx context.Context, srv *mcp.Server, addr string) error {
	httpSrv := &http.Server{Addr: listenAddress(addr), Handler: Handler(srv)}

	// Cancelled on the way out so the watcher below cannot outlive this call
	// when the listener is what failed — otherwise a ServeHTTPContext that
	// never got as far as serving would leave a goroutine parked on a context
	// that may never be done.
	watch, stopWatching := context.WithCancel(ctx)
	defer stopWatching()

	drained := make(chan error, 1)
	go func() {
		<-watch.Done()
		if ctx.Err() == nil {
			// Not a shutdown: the listener returned on its own and the defer
			// above released this goroutine. There is nothing to drain.
			drained <- nil
			return
		}
		// WithoutCancel because this budget starts where ctx stopped: a
		// deadline inherited from an already-cancelled context would expire
		// before the first in-flight request got a millisecond of it.
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
		defer cancel()
		drained <- httpSrv.Shutdown(grace)
	}()

	err := httpSrv.ListenAndServe()
	stopWatching()
	if errors.Is(err, http.ErrServerClosed) {
		// Shutdown is what closed it, so what this call reports is what the
		// drain did rather than the sentinel saying it was asked to stop.
		return <-drained
	}
	return err
}

// Handler mounts srv on a Streamable HTTP handler in stateless mode. It is
// exported so an embedder can serve vacmcp on a mux of its own, and so a test
// can exercise the wiring [ServeHTTP] really runs rather than a copy of it.
//
// Stateless is not a tuning choice: MCP 2026-07-28 removed the protocol-level
// session and the initialize handshake in favour of server/discover, and the
// SDK only offers that version to clients on a stateless server. A stateful
// handler would negotiate an older protocol version instead.
//
// PropagateRequestCancellation is what makes a client's cancellation reach the
// engine. Stateless has no session for a notifications/cancelled to arrive on —
// that notification is a second POST, and the SDK rejects it — so without this
// the POST being abandoned would stop nothing: the client gives up while the
// server runs the whole query against Zoekt, CBM and git to deliver an answer
// down a connection that is gone. Over STDIO the notification does arrive and
// does cancel, so this is also what keeps the two transports answering a
// cancellation the same way.
func Handler(srv *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true},
	)
}

func listenAddress(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return DefaultAddress
	}
	return addr
}
