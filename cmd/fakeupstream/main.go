// Command fakeupstream runs the scriptable fake provider for demos and benches.
// Model-name suffixes pick behaviour: -die, -nousage, -429, -500, -cache.
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
	addr := flag.String("addr", "", "ignored; the fake picks a free port and prints it")
	flag.Parse()
	_ = addr
	f := fakeupstream.New()
	f.SetDefault(fakeupstream.Script{Latency: *latency, ChunkDelay: *chunk, Chunks: *chunks, ChunkBytes: *chunkBytes})
	fmt.Println(f.URL) // first line: the base URL, for scripts to capture
	os.Stdout.Sync()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	f.Close()
	time.Sleep(10 * time.Millisecond)
}
