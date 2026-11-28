package x402

// ResetSeenForTest forgets the in-memory nonce set, to prove the token
// contract (not this process) is what stops a replay (I11).
func (v *Verifier) ResetSeenForTest() { v.mu.Lock(); v.seen = map[[32]byte]bool{}; v.mu.Unlock() }
