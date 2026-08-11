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
	"net/http"
	"strings"

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

// ServeHTTP serves srv over Streamable HTTP on addr, or on [DefaultAddress]
// when addr is empty. It blocks until the listener fails.
func ServeHTTP(srv *mcp.Server, addr string) error {
	return http.ListenAndServe(listenAddress(addr), handler(srv))
}

// handler mounts srv on a Streamable HTTP handler in stateless mode.
//
// Stateless is not a tuning choice: MCP 2026-07-28 removed the protocol-level
// session and the initialize handshake in favour of server/discover, and the
// SDK only offers that version to clients on a stateless server. A stateful
// handler would negotiate an older protocol version instead.
func handler(srv *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
}

func listenAddress(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return DefaultAddress
	}
	return addr
}
