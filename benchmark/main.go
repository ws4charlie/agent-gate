// Command benchmark scores the policy layer. It boots the gateway in process on
// fakes (fake model upstream, fake MCP server, simulated chain, throwaway
// Postgres database), plays every scenario in benchmark/scenarios against it,
// compares the outcome of each scenario's last step with what the scenario
// expects, and writes benchmark/results.md. It exits non-zero on any mismatch.
//
// A scenario is {id, category, setup, steps, expected, rule}. Setup gives the
// agent its scope; steps are played in order; `expected` (allow, deny or
// approval) and `rule` describe the LAST step only, the earlier ones are the
// preparation (burn a budget, kill the agent, approve a request).
//
// Why a benchmark and not only unit tests: unit tests prove each rule in
// isolation; this proves the assembled gateway reaches the right decision for a
// request mix, and a gateway that refuses everything (or allows everything)
// scores badly on precision or recall instead of passing quietly.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	root := flag.String("root", ".", "repository root (scenarios, screen corpus)")
	out := flag.String("out", "benchmark/results.md", "where to write the results ('' to skip)")
	readme := flag.String("readme", "", "rewrite the benchmark table between the markers in this README")
	flag.Parse()
	failed, err := run(*root, *out, *readme)
	if err != nil {
		fmt.Fprintln(os.Stderr, "benchmark:", err)
		os.Exit(2)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "benchmark: %d scenario(s) did not behave as expected\n", failed)
		os.Exit(1)
	}
}
