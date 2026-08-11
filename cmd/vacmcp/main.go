// Command vacmcp serves version-aware code intelligence over MCP.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/tc3oliver/version-aware-code-mcp/server"
)

// version is the build version of the vacmcp binary.
const version = "0.0.0-dev"

func main() {
	stdio := flag.Bool("stdio", false, "serve over STDIO instead of Streamable HTTP")
	address := flag.String("address", server.DefaultAddress, "address to listen on in Streamable HTTP mode")
	flag.Parse()

	srv := server.New(version)

	// log writes to stderr, which is what keeps stdout free for the STDIO
	// protocol stream.
	var err error
	if *stdio {
		err = server.ServeStdio(context.Background(), srv)
	} else {
		err = server.ServeHTTP(srv, *address)
	}
	if err != nil {
		log.Fatalf("vacmcp: %v", err)
	}
}
