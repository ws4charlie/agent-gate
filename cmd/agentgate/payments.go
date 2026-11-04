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

	"github.com/ws4charlie/agent-gate/internal/paidtool"
	"github.com/ws4charlie/agent-gate/internal/policy"
	"github.com/ws4charlie/agent-gate/internal/server"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// PaymentsFile is the JSON the gateway reads with --payments / $AG_PAYMENTS.
// Absent file: the payments route is off.
type PaymentsFile struct {
	Network string            `json:"network"` // sim | base-sepolia
	Tools   map[string]string `json:"tools"`   // tool name -> URL
	Policy  policy.Config     `json:"policy"`
}

func loadPayments(path string) (*server.Payments, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("payments config: %w", err)
	}
	var f PaymentsFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("payments config: %w", err)
	}
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
	return &server.Payments{Policy: policy.New(f.Policy), Wallet: w, Tools: f.Tools}, nil
}

func chainIDFor(network string) (uint64, error) {
	switch network {
	case "sim", "":
		return x402.LocalChainID, nil
	case "base-sepolia":
		return x402.BaseSepoliaChainID, nil
	}
	return 0, fmt.Errorf("unknown network %q (want sim or base-sepolia)", network)
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
	network := fs.String("network", "sim", "sim | base-sepolia")
	payTo := fs.String("pay-to", "", "address that receives payment (required)")
	fund := fs.String("fund", "", "sim only: mint this many USDC to --fund-address at startup, e.g. 5")
	fundAddr := fs.String("fund-address", "", "sim only: the payer address to fund")
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
	hs := &http.Server{Addr: *listen, Handler: paidtool.Handler(ver), ReadHeaderTimeout: 10 * time.Second}
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
