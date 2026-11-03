// Package pricing holds per-model price rows and turns token usage into Nanos.
package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"

	"github.com/ws4charlie/agent-gate/internal/money"
	"github.com/ws4charlie/agent-gate/internal/provider"
)

// Price is Nanos per one million tokens, per token class. A zero CacheRead or
// CacheWrite means "not priced separately": see Normalize.
type Price struct {
	Input      money.Nanos `json:"input"`
	Output     money.Nanos `json:"output"`
	CacheRead  money.Nanos `json:"cache_read"`
	CacheWrite money.Nanos `json:"cache_write"`
}

// Table maps an upstream model name to its price.
type Table struct {
	Models map[string]Price `json:"models"`
}

var ErrUnknownModel = errors.New("pricing: no price for model")

func Load(path string) (*Table, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pricing: %w", err)
	}
	var t Table
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("pricing: parse %s: %w", path, err)
	}
	if t.Models == nil {
		t.Models = map[string]Price{}
	}
	return &t, nil
}

func (t *Table) Save(path string) error {
	b, err := json.MarshalIndent(t, "", "  ") // map keys are sorted by encoding/json: stable diffs
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Lookup returns the price row. Cache classes that the row leaves unset fall
// back to the plain input price, so a provider that reports cache tokens we
// have no rate for is never charged less than input (never undercharge).
func (t *Table) Lookup(model string) (Price, error) {
	p, ok := t.Models[model]
	if !ok {
		return Price{}, fmt.Errorf("%w %q", ErrUnknownModel, model)
	}
	if p.CacheRead == 0 {
		p.CacheRead = p.Input
	}
	if p.CacheWrite == 0 {
		p.CacheWrite = p.Input
	}
	return p, nil
}

// Cost prices every token class separately: cost = sum(ceil(tokens*price/1M)).
// provider.Usage.InputTokens is the non-cached input; the parsers guarantee it.
func (p Price) Cost(u provider.Usage) (money.Nanos, error) {
	var total money.Nanos
	for _, c := range []struct {
		tokens int64
		price  money.Nanos
	}{{u.InputTokens, p.Input}, {u.OutputTokens, p.Output}, {u.CacheReadTokens, p.CacheRead}, {u.CacheWriteTokens, p.CacheWrite}} {
		n, err := money.Cost(c.tokens, c.price)
		if err != nil {
			return 0, err
		}
		if total+n < total {
			return 0, money.ErrOverflow
		}
		total += n
	}
	return total, nil
}

// EstimateTokens is the deliberately high token guess used for holds and for
// the no-usage fallback: bytes/3, rounded up.
func EstimateTokens(byteLen int) int64 { return int64((byteLen + 2) / 3) }

// ---- OpenRouter sync ----

type orModels struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
			CacheRead  string `json:"input_cache_read"`
			CacheWrite string `json:"input_cache_write"`
		} `json:"pricing"`
	} `json:"data"`
}

// perMTok converts "USD per token" as a decimal string into Nanos per 1M
// tokens exactly (x * 1e6 * 1e9), rounding up. Empty means unset.
func perMTok(s string) (money.Nanos, error) {
	if s == "" {
		return 0, nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 {
		return 0, fmt.Errorf("pricing: bad price %q", s)
	}
	r.Mul(r, big.NewRat(1_000_000_000_000_000, 1))
	q, m := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if m.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return 0, money.ErrOverflow
	}
	return money.Nanos(q.Int64()), nil
}

// ParseOpenRouterModels converts the body of GET /api/v1/models. Models with
// negative or unparseable prices (OpenRouter uses -1 for routers) are skipped.
func ParseOpenRouterModels(r io.Reader) (*Table, error) {
	var m orModels
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("pricing: decode models: %w", err)
	}
	t := &Table{Models: map[string]Price{}}
	for _, d := range m.Data {
		in, e1 := perMTok(d.Pricing.Prompt)
		out, e2 := perMTok(d.Pricing.Completion)
		cr, e3 := perMTok(d.Pricing.CacheRead)
		cw, e4 := perMTok(d.Pricing.CacheWrite)
		if d.ID == "" || e1 != nil || e2 != nil || e3 != nil || e4 != nil {
			continue
		}
		t.Models[d.ID] = Price{Input: in, Output: out, CacheRead: cr, CacheWrite: cw}
	}
	return t, nil
}

// Sync fetches url (the free /models endpoint, no key) and returns the table.
func Sync(ctx context.Context, client *http.Client, url string) (*Table, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pricing: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing: fetch: status %d", resp.StatusCode)
	}
	return ParseOpenRouterModels(resp.Body)
}

// Names lists models in sorted order.
func (t *Table) Names() []string {
	out := make([]string, 0, len(t.Models))
	for k := range t.Models {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
