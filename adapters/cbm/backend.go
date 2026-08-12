package cbm

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// How CBM is asked, and why it is asked that way.
//
// CBM's cost is almost entirely startup. Measured against the fixture on a
// warm machine: `codebase-memory-mcp cli <tool>` takes 2.9 seconds with the
// shared daemon running and 8.5 seconds without it, and answers the query
// itself in a few milliseconds. One trace_calls makes three of those calls —
// search_graph to resolve the symbol, trace_path to walk it, search_graph again
// to locate what the walk reached — so a trace paid roughly nine seconds of
// process startup to do about fifty milliseconds of work.
//
// So the process is started once instead of nine times. `codebase-memory-mcp`
// with no arguments is an MCP server on STDIO, offering the same tools the
// `cli` mode runs one at a time; the adapter connects to it as an MCP client
// and keeps the session for the life of the process. The same three calls then
// take about 15 milliseconds each.
//
// The `cli` mode stays as the fallback, and both modes send the same thing: the
// tool name and the same parameters, with `project` set from the context's
// GraphRef on every single call. A session is a connection to CBM, not to a
// graph, so which mode answered cannot change which version answered — there is
// no per-connection project to inherit, and nothing is cached between calls.

// connectTimeout bounds one attempt to start the persistent session. CBM has
// taken 8.5 seconds to come up on a cold machine, and a cold CI runner
// indexing in the background can be slower still, so the limit is generous —
// it exists to stop a CBM that never finishes starting from hanging every
// trace_calls behind it, not to time a healthy one.
const connectTimeout = 2 * time.Minute

// reported is a failure CBM described, as opposed to a failure to reach CBM at
// all: a non-zero `cli` exit or a tool result marked as an error. The
// distinction is what the fallback turns on — an answer CBM gave is an answer,
// and asking the same question through the other mode would only get it again.
type reported struct {
	reason string
}

func (r *reported) Error() string { return r.reason }

// call runs one CBM tool and returns the JSON body it answered with.
//
// It prefers the persistent session and falls back to a one-shot `cli`
// subprocess when there is no session to use or the one there was has broken.
func (p *Provider) call(ctx context.Context, tool string, params map[string]any) ([]byte, error) {
	if session := p.persistent(ctx); session != nil {
		out, err := callTool(ctx, session, tool, params)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			// The caller gave up. That says nothing about CBM, so the session
			// is left alone: treating it as broken would let one client that
			// walked away cost every other one a restart.
			return nil, err
		}
		var byCBM *reported
		if errors.As(err, &byCBM) {
			return nil, err
		}
		// Anything else is the connection rather than the answer: a CBM that
		// exited, a pipe that closed, a response that did not parse. Retire the
		// session and ask the same question the slow way instead of failing a
		// request the fallback can still serve.
		p.retire(session)
	}
	return p.oneShot(ctx, tool, params)
}

// persistent returns the session to use, starting it on first use, or nil when
// this provider has to use the `cli` mode.
func (p *Provider) persistent(ctx context.Context) *mcp.ClientSession {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.session != nil || p.cliOnly {
		return p.session
	}

	// The session outlives the request that starts it, so it is not started on
	// that request's context: a client that goes away would otherwise take
	// down the session every later call depends on.
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), connectTimeout)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "vacmcp", Version: "0"}, nil)
	session, err := client.Connect(startCtx, &mcp.CommandTransport{Command: exec.Command(p.command)}, nil)
	if err != nil {
		// One attempt, then this provider is a `cli` provider. A CBM that
		// cannot be started is usually one that is not installed, and paying a
		// failed startup before every call would make the fallback slower than
		// the mode it is falling back to. The error is not reported here: the
		// `cli` call that follows fails against the same binary for the same
		// reason, in this project's error model.
		p.cliOnly = true
		return nil
	}
	p.session = session
	return session
}

// retire drops a broken session, so the next call starts a new one. The session
// is compared rather than assumed, because concurrent calls can both fail on it
// and the second one must not close its replacement.
func (p *Provider) retire(session *mcp.ClientSession) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.session == session {
		p.session = nil
		_ = session.Close()
	}
}

// Close shuts down the persistent CBM session, if one was started. A Provider
// is usable again afterwards: the next call starts a new session.
//
// The server does not need this — the session ends with the process — but a
// test that builds providers in a loop does, and so does anything embedding
// vacmcp that outlives its providers.
func (p *Provider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	session := p.session
	p.session = nil
	if session == nil {
		return nil
	}
	return session.Close()
}

// callTool makes one tools/call on the persistent session.
func callTool(ctx context.Context, session *mcp.ClientSession, tool string, params map[string]any) ([]byte, error) {
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: params})
	if err != nil {
		return nil, err
	}

	body, err := payload(res)
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, &reported{reason: failure(body)}
	}
	return body, nil
}

// payload returns the text block CBM carries its answer in. Both a result and
// an error arrive that way, holding exactly what the `cli` mode writes to
// standard output and standard error respectively.
func payload(res *mcp.CallToolResult) ([]byte, error) {
	for _, content := range res.Content {
		if block, ok := content.(*mcp.TextContent); ok {
			return []byte(block.Text), nil
		}
	}
	return nil, errors.New("the response carries no text content")
}

// oneShot runs one CBM tool as `codebase-memory-mcp cli <tool>`, the mode that
// starts a process, answers and exits.
func (p *Provider) oneShot(ctx context.Context, tool string, params map[string]any) ([]byte, error) {
	out, err := exec.CommandContext(ctx, p.command, append([]string{"cli", tool}, flags(params)...)...).Output()
	if err == nil {
		return out, nil
	}

	// CBM reports a failure by exiting non-zero and writing a JSON payload to
	// standard error, mixed in with its progress logging.
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil, &reported{reason: fmt.Sprintf("%v: %s", err, failure(exit.Stderr))}
	}
	return nil, err
}

// flags spells the tool parameters as the `cli` mode's command line: the same
// names, with dashes. Sorted, so the arguments of a failed call are in a fixed
// order and can be copied out of a log and re-run as they were.
func flags(params map[string]any) []string {
	out := make([]string, 0, 2*len(params))
	for _, name := range slices.Sorted(maps.Keys(params)) {
		out = append(out, "--"+strings.ReplaceAll(name, "_", "-"), fmt.Sprint(params[name]))
	}
	return out
}
