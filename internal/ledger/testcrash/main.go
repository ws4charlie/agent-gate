// Command testcrash is a test helper for I6. It reserves a hold, attaches an
// upstream id, prints "ready <hold id>" and then blocks forever, so the test
// can SIGKILL it between reserve and settle, the way a crashed gateway dies.
//
// Usage: AG_CRASH_DSN=<dsn> testcrash <account id> <idem key> <amount nanos> <upstream id>
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: testcrash <account> <key> <amount> <upstream id>")
		os.Exit(2)
	}
	amount, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("AG_CRASH_DSN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	l := ledger.New(pool)
	h, err := l.Reserve(ctx, ledger.ReserveReq{AccountID: os.Args[1], Amount: money.Nanos(amount),
		IdemKey: os.Args[2], Kind: ledger.KindModel, TTL: time.Minute})
	if err == nil {
		err = l.AttachUpstream(ctx, h.ID, "fake", os.Args[4])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("ready", h.ID)
	select {} // never settles: the test kills us here
}
