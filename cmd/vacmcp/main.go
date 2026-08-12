// Command vacmcp serves version-aware code intelligence over MCP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/adapters/cbm"
	"github.com/tc3oliver/version-aware-code-mcp/adapters/git"
	"github.com/tc3oliver/version-aware-code-mcp/adapters/zoekt"
	"github.com/tc3oliver/version-aware-code-mcp/config"
	"github.com/tc3oliver/version-aware-code-mcp/resolver"
	"github.com/tc3oliver/version-aware-code-mcp/server"
	"github.com/tc3oliver/version-aware-code-mcp/store"
	"github.com/tc3oliver/version-aware-code-mcp/tools"
)

// version is the build version of the vacmcp binary, reported by `vacmcp
// version` and by the MCP server in its implementation record.
//
// It is a var, not a const, so a release build can write the tag into it:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/vacmcp
//
// A build without that flag keeps the value below, which is how a developer
// build says it is not a release. The tag is never written into the source:
// .github/release-build.sh takes it from the git tag being built.
var version = "0.0.0-dev"

const usage = `vacmcp serves version-aware code intelligence over MCP.

Usage:
  vacmcp serve [--stdio] [--address ADDR] [--config FILE]
                                            run the MCP server
  vacmcp serve --managed [--data-dir DIR]   run the MCP server on the managed contexts
  vacmcp validate --config FILE             load and check a configuration file
  vacmcp contexts --config FILE             list the configured contexts
  vacmcp doctor --config FILE               check the dependencies and contexts
  vacmcp doctor --managed [--data-dir DIR]  check the managed repositories and contexts
  vacmcp repo SUBCOMMAND                    manage the repositories in the data directory
  vacmcp context SUBCOMMAND                 manage the contexts in the data directory
  vacmcp version                            print the vacmcp version`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "vacmcp: %v\n", err)
		os.Exit(1)
	}
}

// run executes one subcommand and writes its output to out. Failures come back
// as an error instead of exiting, which is what lets a test observe the exit
// code the CLI would produce: an error here is a non-zero exit.
func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("no command given\n\n" + usage)
	}

	switch command := args[0]; command {
	case "serve":
		return serve(args[1:])
	case "validate":
		return validate(args[1:], out)
	case "contexts":
		return contexts(args[1:], out)
	case "doctor":
		return doctor(args[1:], out)
	case "repo":
		return repoCommand(args[1:], out)
	case "context":
		return contextCommand(args[1:], out)
	case "version":
		_, err := fmt.Fprintln(out, version)
		return err
	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, usage)
	}
}

// serve runs the MCP server, over Streamable HTTP unless --stdio asks for
// STDIO. It picks the transport and registers the tools; serving is the server
// package's job.
//
// --config is optional: a server started without one is a server with no
// contexts configured, and list_contexts reports that as an empty list rather
// than an error. A configuration file that exists must still be a valid one,
// so a load failure stops the server instead of silently serving nothing.
//
// --managed is the other way to be given a configuration: it is generated from
// the READY contexts of a data directory the management CLI built. Which of the
// two a server runs on has to be said, so the two flags together are refused
// rather than one of them quietly winning.
func serve(args []string) error {
	fs := flag.NewFlagSet("vacmcp serve", flag.ExitOnError)
	stdio := fs.Bool("stdio", false, "serve over STDIO instead of Streamable HTTP")
	address := fs.String("address", server.DefaultAddress, "address to listen on in Streamable HTTP mode")
	path := fs.String("config", "", "path to the vacmcp configuration file")
	managed := fs.Bool("managed", false, "serve the READY contexts of a managed data directory")
	dataDir := fs.String("data-dir", "", "vacmcp data directory in managed mode (default ~/.vacmcp)")
	zoektURL := fs.String("zoekt-url", defaultZoektURL, "Zoekt web server to search through in managed mode")
	cbmCmd := fs.String("cbm-command", cbmCommand, "codebase-memory-mcp binary to trace through in managed mode")
	_ = fs.Parse(args)

	if *managed && *path != "" {
		return errors.New("serve: give either --config or --managed, not both")
	}

	cfg := &config.Config{}
	if *managed {
		s, err := store.Open(*dataDir)
		if err != nil {
			return err
		}
		loaded, err := managedConfig(s, *zoektURL, *cbmCmd)
		if err != nil {
			return err
		}
		cfg = loaded
	} else if *path != "" {
		loaded, err := config.Load(*path)
		if err != nil {
			return err
		}
		cfg = loaded
	}

	// The flag's default value is indistinguishable from the user typing it, so
	// ask the flag set what was actually set.
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "address" {
			explicit = true
		}
	})
	listen := listenAddress(*address, explicit, cfg)

	srv := server.New(version)
	addTools(srv, cfg)

	// Nothing may write to stdout in STDIO mode: it carries the protocol
	// stream. Errors go to stderr, and only after the server has stopped.
	if *stdio {
		return server.ServeStdio(context.Background(), srv)
	}
	return server.ServeHTTP(srv, listen)
}

// listenAddress resolves the Streamable HTTP listen address the way serve does,
// so the precedence can be tested without binding a port: an explicitly given
// --address wins, then the configuration file's server.address, then the
// built-in default.
func listenAddress(flagValue string, explicit bool, cfg *config.Config) string {
	if explicit || cfg.Server.Address == "" {
		return flagValue
	}
	return cfg.Server.Address
}

// addTools registers the four tools of doc-1 §5 on srv, backed by cfg.
//
// All of them are registered whether or not a configuration was given. The tool
// surface is what the server can do, not what it happens to be configured for,
// and a request naming a context that does not exist already has a defined
// answer: the resolver returns CONTEXT_NOT_FOUND before any provider is
// reached. Hiding the tools instead would leave an agent unable to tell "not
// supported" from "not configured".
//
// doctor registers them too, on a server of its own, which is how it can report
// on the MCP layer without going near a port.
func addTools(srv *mcp.Server, cfg *config.Config) {
	contexts := resolver.New(cfg)
	tools.AddListContexts(srv, cfg.Contexts)
	tools.AddSearchCode(srv, contexts, zoekt.New(cfg))
	tools.AddTraceCalls(srv, contexts, cbm.New(cfg))
	tools.AddGetCode(srv, contexts, git.New(cfg))
}

// validate reports whether a configuration file loads and passes validation.
// The error is returned as config produced it, so the caller sees the vacerr
// code and message rather than a rephrasing of them.
func validate(args []string, out io.Writer) error {
	path, cfg, err := loadConfig("validate", args)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s: ok, %d contexts\n", path, len(cfg.Contexts))
	return err
}

// contexts lists the configured contexts, one per line, sorted by ID.
//
// graph_ref is left out on purpose: it is the CBM project backing the context,
// an implementation detail no consumer of a context needs.
func contexts(args []string, out io.Writer) error {
	_, cfg, err := loadConfig("contexts", args)
	if err != nil {
		return err
	}
	for _, id := range slices.Sorted(maps.Keys(cfg.Contexts)) {
		codeCtx := cfg.Contexts[id]
		if _, err := fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", codeCtx.ID, codeCtx.Repository, codeCtx.Branch, codeCtx.Revision); err != nil {
			return err
		}
	}
	return nil
}

// loadConfig parses the flags shared by the commands that read a configuration
// file and loads it. --config has no default: which configuration is in force
// is not something to guess at.
func loadConfig(command string, args []string) (string, *config.Config, error) {
	fs := flag.NewFlagSet("vacmcp "+command, flag.ExitOnError)
	path := fs.String("config", "", "path to the vacmcp configuration file")
	_ = fs.Parse(args)

	if *path == "" {
		return "", nil, fmt.Errorf("%s: --config is required", command)
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return "", nil, err
	}
	return *path, cfg, nil
}
