package money

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"
)

func TestFromUSD(t *testing.T) {
	tests := []struct {
		in   string
		want Nanos
		err  error
	}{
		{"0", 0, nil},
		{"1", 1_000_000_000, nil},
		{"0.5", 500_000_000, nil},
		{".5", 500_000_000, nil},
		{"5.", 5_000_000_000, nil},
		{"3.000000001", 3_000_000_001, nil},
		{"007.10", 7_100_000_000, nil},
		{"9223372036.854775807", math.MaxInt64, nil},
		{"9223372036.854775808", 0, ErrOverflow},
		{"99999999999999999999", 0, ErrOverflow},
		{"0.0000000001", 0, ErrInvalidAmount},
		{"", 0, ErrInvalidAmount},
		{".", 0, ErrInvalidAmount},
		{"-1", 0, ErrInvalidAmount},
		{"+1", 0, ErrInvalidAmount},
		{"1e3", 0, ErrInvalidAmount},
		{"1.2.3", 0, ErrInvalidAmount},
		{"1,5", 0, ErrInvalidAmount},
		{" 1", 0, ErrInvalidAmount},
	}
	for _, tc := range tests {
		got, err := FromUSD(tc.in)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("FromUSD(%q) = %d, %v; want %d, %v", tc.in, got, err, tc.want, tc.err)
		}
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		in   Nanos
		want string
	}{
		{0, "0"},
		{1, "0.000000001"},
		{1_500_000_000, "1.5"},
		{2_000_000_000, "2"},
		{-250_000_000, "-0.25"},
		{math.MaxInt64, "9223372036.854775807"},
		{math.MinInt64, "-9223372036.854775808"},
	}
	for _, tc := range tests {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Nanos(%d).String() = %q; want %q", int64(tc.in), got, tc.want)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	for _, n := range []Nanos{0, 1, 999_999_999, 1_000_000_000, 123_456_789_012, math.MaxInt64} {
		got, err := FromUSD(n.String())
		if err != nil || got != n {
			t.Errorf("round trip %d -> %q -> %d, %v", int64(n), n.String(), int64(got), err)
		}
	}
}

func TestCost(t *testing.T) {
	tests := []struct {
		name   string
		tokens int64
		price  Nanos
		want   Nanos
		err    error
	}{
		{"exact", 1_000_000, 3_000_000_000, 3_000_000_000, nil},
		{"zero tokens", 0, 3_000_000_000, 0, nil},
		{"zero price", 500, 0, 0, nil},
		{"rounds up", 1, 1, 1, nil},
		{"rounds up not nearest", 1, 400_000, 1, nil},
		{"just above exact", 1_000_001, 1, 2, nil},
		{"exact division no extra", 2_000_000, 1, 2, nil},
		{"typical", 1234, 15_000_000_000, 18_510_000, nil},
		{"large but fits", 1 << 40, 1 << 20, (1<<60)/1_000_000 + 1, nil},
		{"overflow", math.MaxInt64, math.MaxInt64, 0, ErrOverflow},
		{"negative tokens", -1, 1, 0, ErrInvalidAmount},
		{"negative price", 1, -1, 0, ErrInvalidAmount},
	}
	for _, tc := range tests {
		got, err := Cost(tc.tokens, tc.price)
		if !errors.Is(err, tc.err) || got != tc.want {
			t.Errorf("%s: Cost(%d, %d) = %d, %v; want %d, %v", tc.name, tc.tokens, int64(tc.price), got, err, tc.want, tc.err)
		}
	}
}

// Cost is the exact ceiling: result*1e6 >= tokens*price > (result-1)*1e6.
func TestCostCeilingProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		tokens := rapid.Int64Range(0, 1<<40).Draw(t, "tokens")
		price := rapid.Int64Range(0, 1<<30).Draw(t, "price")
		got, err := Cost(tokens, Nanos(price))
		if err != nil {
			t.Fatal(err)
		}
		exact := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(price))
		hi := new(big.Int).Mul(big.NewInt(int64(got)), big.NewInt(1_000_000))
		lo := new(big.Int).Mul(big.NewInt(int64(got)-1), big.NewInt(1_000_000))
		if hi.Cmp(exact) < 0 || (got > 0 && lo.Cmp(exact) >= 0) {
			t.Fatalf("Cost(%d, %d) = %d is not ceil", tokens, price, got)
		}
	})
}
