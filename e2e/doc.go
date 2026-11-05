// Package e2e runs the whole gateway end to end with no secrets and no network
// beyond loopback.
//
// Payments run against a real local anvil node over real JSON-RPC (real
// transactions, EIP-1559 fees, receipts). Model traffic runs against recorded
// prompt fixtures (testdata/llm) replayed over real HTTP and SSE by
// internal/fakeupstream. The payment tests skip unless AG_ANVIL_URL is set and
// fail when AG_REQUIRE_ANVIL=1; the Postgres-backed ones follow AG_REQUIRE_DB.
//
// What this proves and what it does not is in docs/learn/local-e2e.md.
package e2e
