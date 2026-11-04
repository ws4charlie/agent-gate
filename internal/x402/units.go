package x402

import (
	"math"
	"math/big"

	"github.com/ws4charlie/agent-gate/internal/money"
)

// USDC has 6 decimals; Nanos are 1e-9 USD, so 1 atomic unit = 1000 Nanos.
const nanosPerAtomic = 1000

// NanosFromAtomic converts USDC atomic units to Nanos with overflow checked.
func NanosFromAtomic(a *big.Int) (money.Nanos, bool) {
	if a == nil || a.Sign() < 0 || !a.IsInt64() || a.Int64() > math.MaxInt64/nanosPerAtomic {
		return 0, false
	}
	return money.Nanos(a.Int64()) * nanosPerAtomic, true
}

// AtomicFromNanos is the inverse; it rejects fractions of a micro-USDC.
func AtomicFromNanos(n money.Nanos) (*big.Int, bool) {
	if n < 0 || n%nanosPerAtomic != 0 {
		return nil, false
	}
	return big.NewInt(int64(n / nanosPerAtomic)), true
}
