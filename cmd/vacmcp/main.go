// Command vacmcp serves version-aware code intelligence over MCP.
package main

import "fmt"

// version is the build version of the vacmcp binary.
const version = "0.0.0-dev"

func main() {
	fmt.Println("vacmcp", version)
}
