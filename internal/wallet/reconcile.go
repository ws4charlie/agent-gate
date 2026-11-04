package wallet

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/x402"
)

// Kinds of disagreement between the ledger and the chain.
const (
	// FindingNoTransfer: the ledger charged a payment (hold settled) but the
	// authorization it recorded was never used on chain. The agent was billed
	// for money that did not move.
	FindingNoTransfer = "ledger_without_transfer"
	// FindingNoLedger: the authorization was used on chain but the hold was
	// released, or is still open past the report. Money moved that the ledger
	// did not charge (yet).
	FindingNoLedger = "transfer_without_ledger"
	// FindingAmount: both agree something moved, but not how much.
	FindingAmount = "amount_mismatch"
)

type Finding struct {
	HoldID, Kind, Detail string
}

// ChainReport is the outcome of comparing payment holds with the chain.
type ChainReport struct {
	Checked  int // payment holds that carry an authorization ref
	Matched  int // settled in the ledger and used on chain for the same value
	Open     int // still active and not used on chain: resolved by the reconciler later
	Released int // released and not used on chain: nothing moved either side
	Findings []Finding
}

// ReconcileChain compares every payment hold created since `since` with the
// chain. The link between the two is the authorization recorded on the hold
// before signing (x402.AuthRef): the nonce is derived from the hold id (I11),
// so one hold maps to at most one transfer. It only reads.
//
// Limit: it starts from the ledger's holds. A transfer from the wallet that
// has no hold at all (made by someone holding the key outside this gateway) is
// not visible this way; that needs event scanning, which the live mode would add.
func ReconcileChain(ctx context.Context, pool *pgxpool.Pool, chain x402.AuthReader, since time.Time) (ChainReport, error) {
	rows, err := pool.Query(ctx, `SELECT id, status, settled_nanos, upstream_id FROM holds
		WHERE kind = 'payment' AND provider = $1 AND upstream_id <> '' AND created_at >= $2 ORDER BY created_at`, x402.Provider, since.UTC())
	if err != nil {
		return ChainReport{}, err
	}
	type hold struct {
		id, status, ref string
		settled         money.Nanos
	}
	var hs []hold
	for rows.Next() {
		var h hold
		var n int64
		if err := rows.Scan(&h.id, &h.status, &n, &h.ref); err != nil {
			rows.Close()
			return ChainReport{}, err
		}
		h.settled = money.Nanos(n)
		hs = append(hs, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ChainReport{}, err
	}
	var rep ChainReport
	for _, h := range hs {
		ref, err := x402.DecodeAuthRef(h.ref)
		if err != nil {
			return rep, fmt.Errorf("hold %s: %w", h.id, err)
		}
		used, err := chain.AuthorizationUsed(ctx, ref.Token, ref.From, ref.Nonce)
		if err != nil {
			return rep, fmt.Errorf("hold %s: asking the chain: %w", h.id, err)
		}
		rep.Checked++
		switch {
		case h.status == "settled" && !used:
			rep.Findings = append(rep.Findings, Finding{h.id, FindingNoTransfer,
				fmt.Sprintf("ledger charged %s USD, authorization %s never executed", h.settled, ref.Nonce.Hex())})
		case h.status == "settled":
			v, _ := new(big.Int).SetString(ref.Value, 10)
			if n, ok := x402.NanosFromAtomic(v); !ok || n != h.settled {
				rep.Findings = append(rep.Findings, Finding{h.id, FindingAmount,
					fmt.Sprintf("ledger charged %s USD, authorization was for %s atomic units", h.settled, ref.Value)})
				continue
			}
			rep.Matched++
		case used: // released, or active
			rep.Findings = append(rep.Findings, Finding{h.id, FindingNoLedger,
				fmt.Sprintf("authorization %s executed on chain (%s atomic units) but the hold is %s", ref.Nonce.Hex(), ref.Value, h.status)})
		case h.status == "active":
			rep.Open++
		default:
			rep.Released++
		}
	}
	return rep, nil
}

// Print renders the reconciliation.
func (r ChainReport) Print(w io.Writer) {
	fmt.Fprintf(w, "\nchain reconciliation: %d payment holds checked, %d matched, %d open, %d released with nothing moved, %d findings\n",
		r.Checked, r.Matched, r.Open, r.Released, len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(w, "  MISMATCH %s hold %s: %s\n", f.Kind, f.HoldID, f.Detail)
	}
}
