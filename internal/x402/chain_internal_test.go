package x402

import (
	"errors"
	"testing"
)

// Only the node's standard refusals are definite; a timeout is not.
func TestRejectedBySend(t *testing.T) {
	for msg, want := range map[string]bool{
		"nonce too low: next nonce 5, tx nonce 4":        true,
		"replacement transaction underpriced":            true,
		"insufficient funds for gas * price + value":     true,
		"Post \"http://rpc\": context deadline exceeded": false,
		"EOF":                      false,
		"already known":            false, // the node HAS it
		"connection reset by peer": false,
	} {
		if got := rejectedBySend(errors.New(msg)); got != want {
			t.Errorf("%q: got %v want %v", msg, got, want)
		}
	}
}
