// Command fakemcp runs the in-repo fake MCP server for the offline demo. It
// prints its URL on the first line of stdout and serves until killed.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/ws4charlie/agent-gate/internal/mcp"
)

func main() {
	key := flag.String("key", os.Getenv("FAKEMCP_KEY"), "required Authorization header value (default $FAKEMCP_KEY; empty: none)")
	flag.Parse()
	f := mcp.NewFake()
	f.Key = *key
	// Re-serve the fake's handler on a loopback port we choose, so the demo does
	// not depend on httptest's port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("http://" + ln.Addr().String() + "/mcp")
	_ = http.Serve(ln, f.Config.Handler)
}
