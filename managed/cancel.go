package managed

import "context"

// cancelled reports the caller's own cancellation as itself, or nil when the
// caller did not cancel.
//
// It exists because of how exec.CommandContext ends a child: when the context is
// done it kills the process, and Wait then reports how it died — "signal: killed"
// — never why. Every subprocess in this package therefore sees an ordinary
// failure, and without asking the context it would classify an operator pressing
// Ctrl-C as a fault report about git, Zoekt or codebase-memory-mcp. A cancelled
// `context create` would come back as GRAPH_PROVIDER_UNAVAILABLE, naming a graph
// engine that was working perfectly.
//
// So every subprocess failure asks this first. The query plane already does the
// same thing at adapters/git/history.go; this is that check, once, for the
// management plane.
//
// It reports the context's error rather than a code of its own: a cancellation is
// not one of doc-1's failures, it is the absence of an answer the caller stopped
// waiting for, and giving it a code would put it in a vocabulary clients branch on.
func cancelled(ctx context.Context) error {
	return ctx.Err()
}
