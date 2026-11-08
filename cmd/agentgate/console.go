package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ws4charlie/agent-gate/console"
)

// usdcOn is Circle's USDC contract per payments network, for the wallet tile.
var usdcOn = map[string]string{
	"base-sepolia": "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
	"base":         "0x833589fCD6eDb6E08f4c7C32D4c71b54bdA02913",
}

// consoleCmd is `agentgate console`: the local web console. It listens on
// loopback only and reads the same database the gateway uses.
func consoleCmd(ctx context.Context, args []string, stdout io.Writer) error {
	c := newCommon("console")
	home, _ := os.UserHomeDir()
	listen := c.fs.String("listen", envOr("AG_CONSOLE_LISTEN", "127.0.0.1:18095"), "address to listen on (loopback only)")
	gateway := c.fs.String("gateway", envOr("AG_GATEWAY_URL", "http://127.0.0.1:8080"), "gateway base URL the console's tasks call")
	dataDir := c.fs.String("data", envOr("AG_CONSOLE_DATA", filepath.Join(home, ".config", "agentgate", "console")), "tasks and run results")
	runners := c.fs.String("runners", os.Getenv("AG_CONSOLE_RUNNERS"), "the program that runs each agent's tasks, as agent=path pairs separated by commas; empty: tasks cannot run")
	paymentsPath := c.fs.String("payments", os.Getenv("AG_PAYMENTS"), "the gateway's payments file: the wallet's network")
	walletAddr := c.fs.String("wallet-address", os.Getenv("AG_WALLET_ADDRESS"), "show this wallet's USDC balance (address only; no key is read)")
	usdc := c.fs.String("usdc", "", "USDC contract for the wallet tile (default: Circle's, from the payments network)")
	_, pool, err := c.open(ctx, args)
	if err != nil {
		return err
	}
	defer pool.Close()

	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("--listen must be a loopback address: the console has no login")
	}

	run, err := parseRunners(*runners)
	if err != nil {
		return err
	}
	cfg := console.Config{Gateway: *gateway, DataDir: *dataDir, Runners: run,
		WalletAddress: *walletAddr, RPCURL: os.Getenv("AG_RPC_URL"), USDC: *usdc}
	if *paymentsPath != "" {
		f, err := readPaymentsFile(*paymentsPath)
		if err != nil {
			return err
		}
		cfg.USDC = cmp.Or(cfg.USDC, usdcOn[f.Network])
	}
	if cfg.USDC == "" {
		cfg.USDC = usdcOn["base-sepolia"]
	}

	srv, err := console.New(cfg, pool)
	if err != nil {
		return err
	}
	hs := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "console on http://%s/#k=%s\n(gateway %s; the key in the link is also in %s)\n",
		ln.Addr(), srv.Key(), *gateway, filepath.Join(*dataDir, "console.key"))
	if len(run) == 0 {
		fmt.Fprintln(stdout, "no --runners: the overview works, running tasks is off")
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	srv.Shutdown()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

// parseRunners reads "agent=path,agent=path". Each path must be a program.
func parseRunners(v string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(v, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		name, path, ok := strings.Cut(pair, "=")
		if !ok || name == "" || path == "" {
			return nil, fmt.Errorf("--runners: %q is not agent=path", pair)
		}
		fi, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("--runners: %s: %w", name, err)
		}
		if fi.IsDir() || fi.Mode()&0o111 == 0 {
			return nil, fmt.Errorf("--runners: %s: %s is not an executable file", name, path)
		}
		out[strings.TrimSpace(name)] = path
	}
	return out, nil
}
