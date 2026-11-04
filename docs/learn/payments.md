# Payments

An agent pays for a tool call with USDC through the x402 flow. The gateway puts the cost on hold in the [ledger](ledger.md), asks policy, and only then lets the wallet sign. Code: [internal/policy](../../internal/policy), [internal/wallet](../../internal/wallet), [internal/x402](../../internal/x402), [internal/paidtool](../../internal/paidtool), [internal/server/payments.go](../../internal/server/payments.go).

## Model in one minute

1. The agent calls `POST /v1/payments {tool, agent_id}`. Tool names map to URLs in config; clients never supply URLs.
2. The gateway sends the tool request **unpaid** and gets `402` with a `PAYMENT-REQUIRED` header: amount, `payTo`, token, network, and the token's EIP-712 name/version.
3. It reserves a hold (`kind=payment`) for that amount ([`pay`](../../internal/server/payments.go) line 62).
4. `policy.Check` approves or refuses (line 161). A refusal releases the hold and stops. Nothing was signed.
5. `wallet.Pay` signs an EIP-3009 authorization (line 172) and the gateway retries with a `PAYMENT-SIGNATURE` header.
6. The tool's x402 server verifies, runs the tool, submits `transferWithAuthorization` to the chain, and answers `200` with `PAYMENT-RESPONSE`.
7. The gateway settles the hold. If the chain refused, it releases the hold instead.

Wire format (x402 v2, HTTP transport): headers `PAYMENT-REQUIRED`, `PAYMENT-SIGNATURE`, `PAYMENT-RESPONSE`, each base64 JSON. Payload fields: `x402Version`, `resource`, `accepted` (the requirements), `payload.signature`, `payload.authorization{from,to,value,validAfter,validBefore,nonce}`. Network is CAIP-2 (`eip155:84532`).

## Screen before sign (I10)

[`policy.Allowed`](../../internal/policy/policy.go) has only unexported fields. Outside package `policy`, `policy.Allowed{}` compiles to the zero value (which reports `Valid() == false` and makes `wallet.Pay` refuse), and a literal such as `policy.Allowed{req: ...}` or `policy.Allowed{ok: true}` does **not compile**: "cannot refer to unexported field ok in struct literal". The only way to get a valid one is `Policy.Check`. So signing without screening is a compile error, not a code-review item.

`wallet.Pay` also checks that the payee, amount and network it is asked to sign are the ones the `Allowed` approved, so a server that changes its price between the 402 and the retry gets a refusal, not a signature.

## EIP-3009 versus approve / transferFrom

| | approve + transferFrom | EIP-3009 `transferWithAuthorization` |
|---|---|---|
| Who sends the transaction | the spender, after the owner sent an `approve` transaction first | anyone holding the owner's signature |
| Owner needs gas | yes, for the `approve` | no: signing is off-chain |
| What the owner authorizes | a standing allowance, often "unlimited" | one transfer: exact amount, payee, time window, nonce |
| Replay | n/a (allowance is state) | the contract marks the (owner, nonce) used |

**Who pays gas.** The wallet owner pays none. In x402 the party that submits the transaction pays: here the paid tool's server (acting as its own facilitator) holds a separate gas key (`AG_FACILITATOR_KEY` on Base Sepolia; a funded throwaway key on the simulated chain). It never holds the payer's tokens; the signature fixes the amount and payee, so it cannot redirect funds.

## Nonce and replay (I11)

`PaymentNonce(holdID) = keccak256("agentgate/x402/nonce/v1:" + holdID)`. One hold, one nonce. Three layers stop a double payment: the gateway's `Idempotency-Key` claim (409), the server's in-flight nonce set, and the token contract's `authorizationState` (a second transfer reverts). The test `TestReplayMovesMoneyOnce` wipes the server's memory and still sees one transfer.

## Failure modes

- **Policy refuses.** Hold released; audit `policy_denied`; signer count stays 0.
- **Chain/verification refuses after signing.** Hold released, budget refunded, audit `payment_failed` with the x402 reason. The same `Idempotency-Key` is spent (409); a new key retries cleanly. Money never moved, so there is no double charge.
- **Settlement pending / connection lost after the retry was sent.** The money may have moved. The gateway does **not** release; it audits `settlement_pending` and leaves the hold active. Resolution is manual or reconciler-driven; T7 hardens this. Known limit.
- **Ledger write fails after the chain settled.** Logged loudly with the tx hash; audit `internal_error`. Also left for T7.

## Invariants and the test that proves each

| Invariant | Test |
|---|---|
| I10 refused payment: signer count zero, no chain tx, ledger and audit rows | `TestRefusedPaymentNeverSigns` (server); `TestWalletBindsPayee...`, `TestWalletRefusals` (x402) |
| I11 replay: one transfer, one charge | `TestReplayMovesMoneyOnce` (x402); `TestPaymentReplayChargesOnce` (server) |
| Tampered signature / wrong payee / wrong amount / expired / wrong network rejected | `TestServerRejects` |
| Chain failure after signing: released, audited, no double charge | `TestChainFailureAfterSigningReleases` |
| I1, I2 after every gateway test | `noActiveHolds()` runs `ledger.Audit` |

## Gotchas hit

- The simulated chain's clock starts near zero; `Sim.syncClock` moves it to wall time before each transaction, or every authorization window would look expired to the contract.
- The mock token bytecode is compiled with solc's default EVM version (uses `PUSH0`); go-ethereum's simulated backend accepts it. See [testdata/README.md](../../internal/x402/testdata/README.md) for the exact compile command.
- `v` must be 27/28 for the contract's `ecrecover`; go-ethereum returns 0/1.

## Quiz

<details>
<summary>Questions and answers</summary>

1. **Why can't code outside `policy` build an `Allowed`?** Its fields are unexported; only `Check` sets them.
2. **What stops a refused payment from being signed?** `wallet.Pay` needs a valid `Allowed`; the gateway only has one after `Check` approves.
3. **Why does the wallet re-check payee and amount?** The 402 can change between the policy check and the retry; the signature must match what was approved.
4. **Who pays gas, and why can't they steal funds?** The tool's server. The signature fixes amount, payee and window.
5. **How is the nonce chosen and why?** Hash of the hold id, so one hold can authorize one transfer.
6. **Three replay defences?** Idempotency key claim, in-flight nonce set, token contract nonce state.
7. **What happens on a chain failure after signing?** Hold released, budget refunded, audit `payment_failed`; same key is spent, a new key retries.
8. **Why is a lost connection after the paid retry not released?** The transfer may have happened; releasing could give the money back while the tool was paid.
9. **EIP-3009 versus approve?** One signed transfer with no owner gas, versus a standing allowance that costs an extra transaction.
10. **Why does the offline run prove less than a Base Sepolia run?** It uses a mock token and a simulated chain; real USDC, RPC and fee behaviour are untested.
11. **Which chain ids may the wallet start with?** 84532 and the local simulated chain (1337).
12. **Why must the sim's clock be synced?** Windows are wall-clock seconds; the contract compares them to block time.

</details>
