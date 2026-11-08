package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
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
	account := c.fs.String("account", os.Getenv("AG_CONSOLE_ACCOUNT"), "ledger account the template agents spend from; empty: tasks cannot run")
	dataDir := c.fs.String("data", envOr("AG_CONSOLE_DATA", filepath.Join(home, ".config", "agentgate", "console")), "templates, template agent keys and run results")
	paymentsPath := c.fs.String("payments", os.Getenv("AG_PAYMENTS"), "the gateway's payments file: paid tool names for the forms, and the wallet's network")
	mcpPath := c.fs.String("mcp", os.Getenv("AG_MCP"), "the gateway's MCP file: server names for the forms")
	walletAddr := c.fs.String("wallet-address", os.Getenv("AG_WALLET_ADDRESS"), "show this wallet's USDC balance (address only; no key is read)")
	payees := c.fs.String("payees", os.Getenv("AG_CONSOLE_PAYEES"), "payee addresses the built-in templates may pay, comma separated (the sellers of your paid tools)")
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

	cfg := console.Config{Gateway: *gateway, Account: *account, DataDir: *dataDir,
		DefaultPayees: csv(*payees), WalletAddress: *walletAddr, RPCURL: os.Getenv("AG_RPC_URL"), USDC: *usdc}
	if *paymentsPath != "" {
		f, err := readPaymentsFile(*paymentsPath)
		if err != nil {
			return err
		}
		for name := range f.Tools {
			cfg.KnownTools = append(cfg.KnownTools, name)
		}
		if cfg.USDC == "" {
			cfg.USDC = usdcOn[f.Network]
		}
	}
	if *mcpPath != "" {
		raw, err := os.ReadFile(*mcpPath)
		if err != nil {
			return err
		}
		var m struct {
			Servers map[string]json.RawMessage `json:"servers"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("%s: %w", *mcpPath, err)
		}
		for name := range m.Servers {
			cfg.KnownTools = append(cfg.KnownTools, name+"/")
		}
	}
	sort.Strings(cfg.KnownTools)
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
	if *account == "" {
		fmt.Fprintln(stdout, "no --account: the overview works, running tasks is off")
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
