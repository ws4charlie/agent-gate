// Command fakeupstream runs the scriptable fake provider for demos and benches.
// Model-name suffixes pick behaviour: -die, -nousage, -429, -500, -cache.
// With -fixtures DIR it replays the recorded prompts in DIR instead (testdata/llm):
// an unmatched prompt is a loud 500 naming the prompt hash.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/ws4charlie/agent-gate/internal/fakeupstream"
)

func main() {
	latency := flag.Duration("latency", 0, "delay before the first byte")
	chunk := flag.Duration("chunk-delay", 0, "delay between stream events")
	chunks := flag.Int("chunks", 0, "stream this many filler chunks instead of the default two (long streams for kill-switch demos)")
	chunkBytes := flag.Int("chunk-bytes", 40, "size of each filler chunk")
	addr := flag.String("addr", "", "listen address, e.g. 127.0.0.1:18999 (default: a free port; the base URL is printed either way)")
	fixtures := flag.String("fixtures", "", "replay the fixtures in this directory (match on model + last message)")
	flag.Parse()
	f := fakeupstream.NewAt(*addr)
	if *fixtures != "" {
		fx, err := fakeupstream.LoadFixtures(*fixtures)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fakeupstream:", err)
			os.Exit(1)
		}
		f.UseFixtures(fx)
	}
	f.SetDefault(fakeupstream.Script{Latency: *latency, ChunkDelay: *chunk, Chunks: *chunks, ChunkBytes: *chunkBytes})
	fmt.Println(f.URL) // first line: the base URL, for scripts to capture
	os.Stdout.Sync()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	f.Close()
	time.Sleep(10 * time.Millisecond)
}
