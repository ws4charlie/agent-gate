package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	ecommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/ws4charlie/agent-gate/internal/gatecred"
	"github.com/ws4charlie/agent-gate/internal/ledger"
	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/paidtool"
	"github.com/ws4charlie/agent-gate/internal/server"
	"github.com/ws4charlie/agent-gate/internal/vaultsign"
	"github.com/ws4charlie/agent-gate/internal/wallet"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// PaymentsFile is the JSON the gateway reads with --payments / $AG_PAYMENTS.
// Absent file: the payments route is off.
type PaymentsFile struct {
	Network string            `json:"network"` // required: sim | anvil | base-sepolia
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
	if err := checkNetwork(f.Network); err != nil {
		return f, err
	}
	return f, nil
}

// checkNetwork refuses an empty or unknown network. There is no default: an
// empty network once meant the simulated chain, which a real deployment must
// never fall into by leaving the setting out.
func checkNetwork(network string) error {
	switch network {
	case "sim", x402.NetworkAnvil, "base-sepolia":
		return nil
	case "":
		return errors.New(`payments config: "network" is required: sim, anvil or base-sepolia`)
	}
	return fmt.Errorf(`payments config: "network" is %q: want sim, anvil or base-sepolia`, network)
}

// Signer selection follows the network, and no setting can change it: on a
// real chain (anvil, base-sepolia) only QuorumVault signs; on the in-process
// simulated chain the gateway signs with a throw-away key it makes at start.
const (
	EnvVaultAudience      = "AG_VAULT_AUDIENCE"
	EnvVaultWalletAddress = "AG_VAULT_WALLET_ADDRESS"
	EnvVaultCredTTL       = "AG_VAULT_CRED_TTL" // Go duration, e.g. 60s; default and most 90s
)

// retiredEnv are the settings of the local-key signer the gateway once used
// on real chains. A deployment that still sets one must fail loudly, never
// start with a different signer than it asked for.
var retiredEnv = []string{"AG_SIGNER", "AG_WALLET_KEY", "AG_WALLET_KEY_FILE"}

// refuseRetiredEnv stops the gateway when any retired setting is non-empty.
func refuseRetiredEnv() error {
	for _, n := range retiredEnv {
		if os.Getenv(n) != "" {
			return fmt.Errorf("%s is set, but the gateway no longer has a local-key signer: on anvil and base-sepolia it signs only through QuorumVault (the AG_VAULT_* settings replace it), on sim with a throw-away key; unset %s", n, n)
		}
	}
	return nil
}

// vaultErr prefixes every QuorumVault setup error.
const vaultErr = "x402 signer (QuorumVault)"

func loadPayments(f PaymentsFile) (*server.Payments, error) {
	chainID, err := chainIDFor(f.Network)
	if err != nil {
		return nil, err
	}
	var maxTx money.Nanos
	if f.MaxTxUSD != "" { // the signer's own cap, independent of every agent's policy
		n, err := money.FromUSD(f.MaxTxUSD)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("payments config: max_tx_usd: %q is not a positive USD amount", f.MaxTxUSD)
		}
		maxTx = n
	}
	if f.Network == "sim" {
		key, err := crypto.GenerateKey() // lives in this process only; never read from env or file
		if err != nil {
			return nil, err
		}
		w, err := wallet.New(key, chainID) // refuses every chain but the simulated one
		if err != nil {
			return nil, err
		}
		w.SetMaxTx(maxTx)
		return &server.Payments{Wallet: w, Tools: f.Tools}, nil
	}
	v, err := loadVaultSigner(chainID)
	if err != nil {
		return nil, err
	}
	v.SetMaxTx(maxTx)
	return &server.Payments{Wallet: v, Tools: f.Tools}, nil
}

// vaultEnv is every setting the QuorumVault signer needs, in the order it is reported.
var vaultEnv = []string{vaultsign.EnvURL, vaultsign.EnvWalletID, vaultsign.EnvCert, vaultsign.EnvKey, vaultsign.EnvCA,
	vaultsign.EnvTokenURL, vaultsign.EnvClientID, vaultsign.EnvClientSecretFile, gatecred.EnvKid, EnvVaultAudience, EnvVaultWalletAddress}

// loadVaultSigner builds the QuorumVault signer from the environment. Any
// missing setting stops the gateway with its name.
func loadVaultSigner(chainID uint64) (*wallet.VaultSigner, error) {
	var missing []string
	for _, n := range vaultEnv {
		if strings.TrimSpace(os.Getenv(n)) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s: missing %s", vaultErr, strings.Join(missing, ", "))
	}
	addr := strings.TrimSpace(os.Getenv(EnvVaultWalletAddress))
	if !ecommon.IsHexAddress(addr) {
		return nil, fmt.Errorf("%s: %s is not a 0x address", vaultErr, EnvVaultWalletAddress)
	}
	var ttl time.Duration
	if s := strings.TrimSpace(os.Getenv(EnvVaultCredTTL)); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < wallet.MinVaultTTL || d > gatecred.DefaultTTL {
			return nil, fmt.Errorf("%s: %s=%q: want a duration from %s to %s", vaultErr, EnvVaultCredTTL, s, wallet.MinVaultTTL, gatecred.DefaultTTL)
		}
		ttl = d
	}
	cfg, err := vaultsign.ConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", vaultErr, err)
	}
	client, err := vaultsign.New(*cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", vaultErr, err)
	}
	key, err := gatecred.LoadKey() // $AG_GATE_CRED_KEY_FILE (default data/gate-cred.key) and $AG_GATE_CRED_KID
	if err != nil {
		return nil, fmt.Errorf("%s: Gate credential key (%s, %s): %w", vaultErr, gatecred.EnvKeyFile, gatecred.EnvKid, err)
	}
	return wallet.NewVaultSigner(wallet.VaultConfig{Client: client, CredKey: key, Audience: strings.TrimSpace(os.Getenv(EnvVaultAudience)),
		WalletID: cfg.WalletID, From: ecommon.HexToAddress(addr), ChainID: chainID, TTL: ttl})
}

// setPolicyVersion fills pay.PolicyVersion when the signer is Vault, the only
// thing that uses it: the sim startup reads no policy files for it.
func setPolicyVersion(pay *server.Payments, cedarDir string, enforce bool) error {
	if _, ok := pay.Wallet.(*wallet.VaultSigner); !ok {
		return nil
	}
	v, err := policyVersion(cedarDir, enforce)
	if err != nil {
		return fmt.Errorf("%s: policy version: %w", vaultErr, err)
	}
	pay.PolicyVersion = v
	return nil
}

// policyVersion names the policy a payment was decided under, for the Vault
// credential (≤ 64 bytes): the Go engine's version and, when Cedar runs, its
// mode and a hash of the policy files.
func policyVersion(cedarDir string, enforce bool) (string, error) {
	if cedarDir == "" {
		return server.DefaultPolicyVersion, nil
	}
	files, err := filepath.Glob(filepath.Join(cedarDir, "*.cedar"))
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\n%d\n", filepath.Base(f), len(b))
		h.Write(b)
	}
	mode := "shadow"
	if enforce {
		mode = "enforce"
	}
	return fmt.Sprintf("%s+cedar-%s:%x", server.DefaultPolicyVersion, mode, h.Sum(nil)[:6]), nil
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
	case "sim":
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
	case "sim":
		return x402.LocalChainID, nil
	case "base-sepolia", x402.NetworkAnvil:
		return x402.BaseSepoliaChainID, nil // anvil runs with the testnet's chain id: the signing path is unchanged
	}
	return 0, fmt.Errorf("unknown network %q (want sim, anvil or base-sepolia)", network)
}

// EnvPaidtoolToken names the token the paid tool accepts on anvil (--token).
const EnvPaidtoolToken = "AG_PAIDTOOL_TOKEN"

// anvilToken returns the token the paid tool accepts on anvil. With an
// address, it is that deployed token, after checking it is an EIP-3009
// contract whose EIP-712 domain matches what it reports; cfg takes the
// token's own name and version, so the domain in the 402 is the one the
// contract verifies. Without one, a fresh mock USDC is deployed.
func anvilToken(ctx context.Context, chain *x402.EthChain, addr string, cfg *x402.Config) (ecommon.Address, error) {
	if addr == "" {
		return chain.DeployMockUSDC(ctx)
	}
	d, err := chain.TokenDomain(ctx, ecommon.HexToAddress(addr))
	if err != nil {
		return ecommon.Address{}, fmt.Errorf("paidtool: --token: %w", err)
	}
	cfg.TokenName, cfg.TokenVersion = d.Name, d.Version
	return d.Contract, nil
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
	scraperCache := fs.String("litescraper-cache", "", "directory of recorded LiteScraper results; a new search is saved there")
	scraperReplay := fs.Bool("litescraper-replay", false, "answer only from --litescraper-cache and never call Firecrawl")
	fund := fs.String("fund", "", "sim and anvil: mint this many USDC to --fund-address at startup, e.g. 5")
	fundAddr := fs.String("fund-address", "", "sim and anvil: the payer address to fund")
	tokenAddr := fs.String("token", os.Getenv(EnvPaidtoolToken), "anvil: accept this already deployed EIP-3009 token instead of deploying a mock (default $"+EnvPaidtoolToken+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !ecommon.IsHexAddress(*payTo) {
		return errors.New("paidtool: --pay-to must be an address")
	}
	*tokenAddr = strings.TrimSpace(*tokenAddr)
	if *tokenAddr != "" {
		if *network != x402.NetworkAnvil {
			return fmt.Errorf("paidtool: --token (%s) is for --network anvil only", EnvPaidtoolToken)
		}
		if !ecommon.IsHexAddress(*tokenAddr) {
			return fmt.Errorf("paidtool: --token %q is not a 0x address", *tokenAddr)
		}
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
		token, err := anvilToken(ctx, chain, *tokenAddr, &cfg)
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
	if key := strings.TrimSpace(os.Getenv("FIRECRAWL_API_KEY")); key != "" || *scraperReplay {
		if *scraperReplay && *scraperCache == "" {
			return errors.New("paidtool: --litescraper-replay needs --litescraper-cache")
		}
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		mux.Handle("POST /mcp/litescraper", paidtool.LiteScraperMCP(ver, &paidtool.LiteScraper{Key: key, Cache: *scraperCache, Replay: *scraperReplay}))
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
