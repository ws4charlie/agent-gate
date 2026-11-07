package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	ecommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/paidtool"
	"github.com/ws4charlie/agent-gate/internal/server"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// PaymentsFile is the JSON the gateway reads with --payments / $AG_PAYMENTS.
// Absent file: the payments route is off.
type PaymentsFile struct {
	Network string            `json:"network"` // sim | anvil | base-sepolia
	Tools   map[string]string `json:"tools"`   // tool name -> URL
	// ChainQueryURL (sim only): base URL of the paid tool, which hosts the
	// simulated chain; the reconciler asks it whether an authorization was used.
	// On anvil and base-sepolia the reconciler reads the node over $AG_RPC_URL instead.
	ChainQueryURL string `json:"chain_query_url"`
	// MaxTxUSD is the wallet's per-transaction cap (default $1). It is checked
	// inside the wallet, after and independent of policy.
	MaxTxUSD string `json:"max_tx_usd"`
}

func readPaymentsFile(path string) (PaymentsFile, error) {
	var f PaymentsFile
	b, err := os.ReadFile(path)
	if err != nil {
		return f, fmt.Errorf("payments config: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("payments config: %w", err)
	}
	return f, nil
}

func loadPayments(f PaymentsFile) (*server.Payments, error) {
	chainID, err := chainIDFor(f.Network)
	if err != nil {
		return nil, err
	}
	key, err := wallet.LoadKey()
	if err != nil {
		return nil, err
	}
	w, err := wallet.New(key, chainID) // refuses any chain but 84532 and the local sim
	if err != nil {
		return nil, err
	}
	if f.MaxTxUSD != "" { // the wallet's own cap, independent of every agent's policy
		n, err := money.FromUSD(f.MaxTxUSD)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("payments config: max_tx_usd: %q is not a positive USD amount", f.MaxTxUSD)
		}
		w.SetMaxTx(n)
	}
	return &server.Payments{Wallet: w, Tools: f.Tools}, nil
}

// paymentsLookup builds the reconciler's view of the chain for payment holds.
// After signing, the chain is the source of truth, so a gateway that has
// payments configured must be able to read it.
func paymentsLookup(ctx context.Context, f PaymentsFile) (ledger.UsageLookup, error) {
	r, err := chainReader(ctx, f)
	if err != nil {
		return nil, err
	}
	return &x402.AuthLookup{Chain: r}, nil
}

// chainReader is the read-only view of the chain: the simulated one over HTTP,
// or Base Sepolia over $AG_RPC_URL. It holds no key.
func chainReader(ctx context.Context, f PaymentsFile) (x402.AuthReader, error) {
	var r x402.AuthReader
	switch f.Network {
	case "sim", "":
		if f.ChainQueryURL == "" {
			return nil, errors.New("payments config: sim needs chain_query_url (the paid tool's base URL)")
		}
		r = &x402.RemoteChain{BaseURL: strings.TrimRight(f.ChainQueryURL, "/")}
	case "anvil":
		// The same node over real JSON-RPC (chainReader), not the paid tool's HTTP path.
		n, err := x402.DialAnvil(ctx, envOr("AG_RPC_URL", x402.DefaultAnvilURL))
		if err != nil {
			return nil, err
		}
		r = &x402.EthChain{Client: n.Client} // read-only use: no facilitator key
	case "base-sepolia":
		rpc := os.Getenv("AG_RPC_URL")
		if rpc == "" {
			return nil, errors.New("payments config: base-sepolia needs AG_RPC_URL for the reconciler")
		}
		cl, err := ethclient.DialContext(ctx, rpc)
		if err != nil {
			return nil, err
		}
		id, err := cl.ChainID(ctx)
		if err != nil {
			return nil, err
		}
		if id.Uint64() != x402.BaseSepoliaChainID {
			return nil, fmt.Errorf("payments config: RPC reports chain id %s, want %d", id, x402.BaseSepoliaChainID)
		}
		r = &x402.EthChain{Client: cl} // read-only use: no facilitator key
	default:
		return nil, fmt.Errorf("unknown network %q", f.Network)
	}
	return r, nil
}

func chainIDFor(network string) (uint64, error) {
	switch network {
	case "sim", "":
		return x402.LocalChainID, nil
	case "base-sepolia", x402.NetworkAnvil:
		return x402.BaseSepoliaChainID, nil // anvil runs with the testnet's chain id: the signing path is unchanged
	}
	return 0, fmt.Errorf("unknown network %q (want sim, anvil or base-sepolia)", network)
}

// walletCmd: `agentgate wallet new [--file PATH]` and `wallet address`.
func walletCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("wallet: need new | address")
	}
	fs := flag.NewFlagSet("wallet "+args[0], flag.ContinueOnError)
	file := fs.String("file", envOr(wallet.EnvKeyFile, wallet.DefaultKeyFile), "key file (gitignored)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "new":
		addr, err := wallet.NewKeyFile(*file)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, addr.Hex()) // the address only; the key never leaves the file
		return nil
	case "address":
		os.Setenv(wallet.EnvKeyFile, *file)
		k, err := wallet.LoadKey()
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, crypto.PubkeyToAddress(k.PublicKey).Hex())
		return nil
	}
	return fmt.Errorf("wallet: unknown subcommand %q", args[0])
}

// paidtoolCmd runs the paid endpoint. `--network sim` (default) runs an
// in-process simulated chain and needs nothing else. `--network base-sepolia`
// needs $AG_RPC_URL and $AG_FACILITATOR_KEY (a key funded with Base Sepolia ETH
// for gas); it is built but not run by the hands (PLAN hard rule 4).
func paidtoolCmd(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("paidtool", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:9402", "listen address")
	network := fs.String("network", "sim", "sim | anvil | base-sepolia")
	payTo := fs.String("pay-to", "", "address that receives payment (required)")
	crawlerCache := fs.String("litecrawler-cache", "", "directory of recorded LiteCrawler results; a new search is saved there")
	crawlerReplay := fs.Bool("litecrawler-replay", false, "answer only from --litecrawler-cache and never call Firecrawl")
	fund := fs.String("fund", "", "sim and anvil: mint this many USDC to --fund-address at startup, e.g. 5")
	fundAddr := fs.String("fund-address", "", "sim and anvil: the payer address to fund")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !ecommon.IsHexAddress(*payTo) {
		return errors.New("paidtool: --pay-to must be an address")
	}
	cfg := x402.Config{PayTo: ecommon.HexToAddress(*payTo), TokenName: "USDC", TokenVersion: "2"}
	switch *network {
	case "sim":
		sim, err := x402.NewSim()
		if err != nil {
			return err
		}
		defer sim.Close()
		cfg.Network, cfg.Token, cfg.Chain = x402.NetworkLocal, sim.Token, sim.Chain
		if *fund != "" {
			if !ecommon.IsHexAddress(*fundAddr) {
				return errors.New("paidtool: --fund needs --fund-address")
			}
			whole, err := strconv.ParseInt(*fund, 10, 64)
			if err != nil || whole < 0 {
				return errors.New("paidtool: --fund must be a whole number of USDC")
			}
			if err := sim.Mint(ctx, ecommon.HexToAddress(*fundAddr), big.NewInt(whole*1_000_000)); err != nil {
				return err
			}
		}
	case x402.NetworkAnvil:
		// A local anvil node (--chain-id 84532), $AG_RPC_URL or http://127.0.0.1:8545. The
		// guard in DialAnvil refuses anything that is not anvil before a single mint.
		node, err := x402.DialAnvil(ctx, envOr("AG_RPC_URL", x402.DefaultAnvilURL))
		if err != nil {
			return err
		}
		defer node.Close()
		chain, err := node.NewChain(ctx) // fresh facilitator key, gas via anvil_setBalance
		if err != nil {
			return err
		}
		token, err := chain.DeployMockUSDC(ctx)
		if err != nil {
			return err
		}
		cfg.Network, cfg.Token, cfg.Chain = x402.NetworkBaseSepolia, token, chain
		if *fund != "" {
			if !ecommon.IsHexAddress(*fundAddr) {
				return errors.New("paidtool: --fund needs --fund-address")
			}
			whole, err := strconv.ParseInt(*fund, 10, 64)
			if err != nil || whole < 0 {
				return errors.New("paidtool: --fund must be a whole number of USDC")
			}
			if err := chain.Mint(ctx, token, ecommon.HexToAddress(*fundAddr), big.NewInt(whole*1_000_000)); err != nil {
				return err
			}
		}
	case "base-sepolia":
		rpc, fkey := os.Getenv("AG_RPC_URL"), strings.TrimPrefix(strings.TrimSpace(os.Getenv("AG_FACILITATOR_KEY")), "0x")
		if rpc == "" || fkey == "" {
			return errors.New("paidtool: base-sepolia needs AG_RPC_URL and AG_FACILITATOR_KEY (a Base Sepolia key funded with ETH for gas)")
		}
		var k *ecdsa.PrivateKey
		k, err := crypto.HexToECDSA(fkey)
		if err != nil {
			return errors.New("paidtool: AG_FACILITATOR_KEY is not a hex private key")
		}
		cl, err := ethclient.DialContext(ctx, rpc)
		if err != nil {
			return err
		}
		id, err := cl.ChainID(ctx)
		if err != nil {
			return err
		}
		if id.Uint64() != x402.BaseSepoliaChainID { // never settle on any other chain
			return fmt.Errorf("paidtool: RPC reports chain id %s, want %d", id, x402.BaseSepoliaChainID)
		}
		cfg.Network, cfg.Token = x402.NetworkBaseSepolia, ecommon.HexToAddress(x402.USDCBaseSepolia)
		cfg.Chain = &x402.EthChain{Client: cl, Facilitator: k}
	default:
		return fmt.Errorf("paidtool: unknown network %q", *network)
	}
	ver := x402.NewVerifier(cfg)
	handler := http.Handler(paidtool.Handler(ver))
	if key := strings.TrimSpace(os.Getenv("FIRECRAWL_API_KEY")); key != "" || *crawlerReplay {
		if *crawlerReplay && *crawlerCache == "" {
			return errors.New("paidtool: --litecrawler-replay needs --litecrawler-cache")
		}
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		mux.Handle("POST /mcp/litecrawler", paidtool.LiteCrawlerMCP(ver, &paidtool.LiteCrawler{Key: key, Cache: *crawlerCache, Replay: *crawlerReplay}))
		handler = mux
	}
	hs := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sc)
	}()
	fmt.Fprintf(stdout, "paidtool listening on %s (network %s, token %s, pay to %s)\n", *listen, cfg.Network, cfg.Token, cfg.PayTo)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
