package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// commandContext returns the context a command does its work under, cancelled by
// SIGINT or SIGTERM, together with the stop function that releases the handler.
//
// Every command that starts an external process needs one. `git clone` of a large
// repository, `zoekt-git-index`, and codebase-memory-mcp's `index_repository` all
// run for minutes, and exec.CommandContext kills its child when the context it was
// given is done — so a command holding context.Background() is a command whose
// Ctrl-C reaches nothing. The operator's terminal returns, and git carries on
// cloning behind it.
//
// stop() is called again as soon as the context is done, which is what makes a
// second signal work: it puts SIGINT and SIGTERM back to their default
// disposition, so an operator who decides the first one is taking too long can end
// the process rather than wait for it. The first signal asks; the second insists.
// serve uses the same arrangement, and this is the one implementation of it.
func commandContext() (context.Context, func()) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
}
