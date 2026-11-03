// Package money holds the one currency unit used on the ledger path.
package money

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Nanos is an int64 count of 1e-9 USD. No floats touch money (PLAN "Money").
// 1 micro-USDC = 1000 Nanos.
type Nanos int64

const (
	perUSD     = 1_000_000_000
	usdDecimal = 9
)

var (
	ErrInvalidAmount = errors.New("money: invalid amount")
	ErrOverflow      = errors.New("money: overflow")
)

// FromUSD parses a decimal USD string ("12", "0.5", "3.000000001") exactly.
// Strings that need more than 9 decimals, carry a sign or exponent, or do not
// fit in int64 Nanos are rejected: silently rounding money is how ledgers drift.
func FromUSD(s string) (Nanos, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" || !digits(whole) || !digits(frac) || len(frac) > usdDecimal {
		return 0, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	frac += strings.Repeat("0", usdDecimal-len(frac))
	n := new(big.Int)
	n.SetString(whole+frac, 10) // digits only, cannot fail; leading zeros are fine
	if !n.IsInt64() {
		return 0, fmt.Errorf("%w: %q", ErrOverflow, s)
	}
	return Nanos(n.Int64()), nil
}

func digits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String renders Nanos as a decimal USD amount with trailing zeros trimmed,
// e.g. 1500000000 -> "1.5", 0 -> "0".
func (n Nanos) String() string {
	neg := n < 0
	u := uint64(n)
	if neg {
		u = -u // correct for MinInt64 too
	}
	s := fmt.Sprintf("%d.%09d", u/perUSD, u%perUSD)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if neg {
		s = "-" + s
	}
	return s
}

// Cost returns ceil(tokens * pricePerMTok / 1_000_000): the price is Nanos per
// one million tokens. Rounding up means the ledger never undercharges (I1/I2
// depend on charges being whole Nanos). Negative inputs and results that do not
// fit in Nanos are errors.
func Cost(tokens int64, pricePerMTok Nanos) (Nanos, error) {
	if tokens < 0 || pricePerMTok < 0 {
		return 0, fmt.Errorf("%w: tokens=%d price=%d", ErrInvalidAmount, tokens, pricePerMTok)
	}
	p := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(int64(pricePerMTok)))
	p.Add(p, big.NewInt(999_999))
	p.Quo(p, big.NewInt(1_000_000))
	if !p.IsInt64() {
		return 0, ErrOverflow
	}
	return Nanos(p.Int64()), nil
}
