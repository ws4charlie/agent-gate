package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ws4charlie/agent-gate/console"
)

// usdcOn is Circle's USDC contract per payments network, for the wallet tile.
var usdcOn = map[string]string{
	"base-sepolia": "0x036CbD53842c5426634e7929541eC2318f3dCF7e",
	"base":         "0x833589fCD6eDb6E08f4c7C32D4c71b54bdA02913",
}

// explorerOn is the block explorer per payments network, for links on the page.
var explorerOn = map[string]string{
	"base-sepolia": "https://sepolia.basescan.org",
	"base":         "https://basescan.org",
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
	tailnetListen := c.fs.String("tailnet-listen", os.Getenv("AG_CONSOLE_TAILNET_LISTEN"), "also serve over HTTPS on this machine's Tailscale address, e.g. 100.101.102.103:443; empty: this machine only")
	tailnetHost := c.fs.String("tailnet-host", os.Getenv("AG_CONSOLE_TAILNET_HOST"), "this machine's name on the tailnet, e.g. agentgate.tail1234.ts.net")
	tailnetCert := c.fs.String("tailnet-cert", os.Getenv("AG_CONSOLE_TAILNET_CERT"), "certificate file from `tailscale cert`")
	tailnetKey := c.fs.String("tailnet-key", os.Getenv("AG_CONSOLE_TAILNET_KEY"), "key file from `tailscale cert`")
	tailscaleSocket := c.fs.String("tailscale-socket", envOr("AG_TAILSCALE_SOCKET", "/var/run/tailscale/tailscaled.sock"), "tailscaled's LocalAPI socket, used to tell who is calling")
	allow := c.fs.String("allow", os.Getenv("AG_CONSOLE_ALLOW"), "logins that may open the console over the tailnet, comma separated, e.g. you@gmail.com")
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
		cfg.Explorer = explorerOn[f.Network]
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

	servers := []*http.Server{hs}
	errc := make(chan error, 2)
	go func() { errc <- hs.Serve(ln) }()
	if *tailnetListen != "" {
		ts, tln, err := tailnetServer(srv, *tailnetListen, *tailnetHost, *tailnetCert, *tailnetKey, *tailscaleSocket, csv(*allow))
		if err != nil {
			hs.Close()
			return err
		}
		servers = append(servers, ts)
		go func() { errc <- ts.ServeTLS(tln, "", "") }()
		fmt.Fprintf(stdout, "console on the tailnet at https://%s/ for %s\n", *tailnetHost, strings.Join(csv(*allow), ", "))
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	srv.Shutdown()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	return nil
}

// tailnetServer serves the console over HTTPS on a Tailscale address, to the
// allowed logins only. The certificate comes from `tailscale cert` and is
// read again when the files change, so a renewal needs no restart.
func tailnetServer(srv *console.Server, listen, host, certFile, keyFile, socket string, allow []string) (*http.Server, net.Listener, error) {
	h, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, nil, fmt.Errorf("--tailnet-listen: %w", err)
	}
	if ip := net.ParseIP(h); ip == nil || !console.TailnetAddr(ip) {
		return nil, nil, errors.New("--tailnet-listen must be this machine's Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48)")
	}
	switch {
	case host == "" || !strings.HasSuffix(host, ".ts.net"):
		return nil, nil, errors.New("--tailnet-host must be this machine's name on the tailnet, ending in .ts.net")
	case certFile == "" || keyFile == "":
		return nil, nil, errors.New("--tailnet-cert and --tailnet-key are needed: make them with `tailscale cert`")
	case len(allow) == 0:
		return nil, nil, errors.New("--allow must name at least one login: nobody else can open the console on the tailnet")
	}
	certs := &reloadingCert{cert: certFile, key: keyFile}
	if _, err := certs.get(nil); err != nil {
		return nil, nil, err
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, nil, err
	}
	ts := &http.Server{Handler: srv.TailnetHandler(host, allow, console.TailscaleWhois(socket)),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: certs.get}}
	return ts, ln, nil
}

// reloadingCert serves a key pair from disk, reading it again when the
// certificate file changes.
type reloadingCert struct {
	cert, key string
	mu        sync.Mutex
	mod       time.Time
	pair      *tls.Certificate
}

func (c *reloadingCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	fi, err := os.Stat(c.cert)
	if err != nil {
		return nil, fmt.Errorf("tailnet certificate: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pair == nil || !fi.ModTime().Equal(c.mod) {
		pair, err := tls.LoadX509KeyPair(c.cert, c.key)
		if err != nil {
			return nil, fmt.Errorf("tailnet certificate: %w", err)
		}
		c.pair, c.mod = &pair, fi.ModTime()
	}
	return c.pair, nil
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
