# Payments

An agent pays for a tool call with USDC through the x402 flow. The gateway puts the cost on hold in the [ledger](ledger.md), asks policy, and only then lets the wallet sign. Code: [internal/policy](../../internal/policy), [internal/wallet](../../internal/wallet), [internal/x402](../../internal/x402), [internal/paidtool](../../internal/paidtool), [internal/server/payments.go](../../internal/server/payments.go).

## Model in one minute

1. The agent calls `POST /v1/payments {tool, input}` with its credential; the credential names the agent (a body `agent_id` is optional and must agree). Tool names map to URLs in config; clients never supply URLs.
2. The gateway sends the tool request **unpaid** and gets `402` with a `PAYMENT-REQUIRED` header: amount, `payTo`, token, network, and the token's EIP-712 name/version.
3. It reserves a hold (`kind=payment`) for that amount ([`pay`](../../internal/server/payments.go)).
4. `policy.Evaluate` allows, refuses or queues the payment for approval (see [policy](policy.md)): the tool and payee must be in the agent's scope and the budget and velocity limits must hold. A refusal releases the hold and stops. Nothing was signed.
5. `wallet.Prepare` builds the authorization (nonce from the hold id). The gateway records it on the hold (`AttachUpstream("x402", ref)`) **before** `wallet.Sign` runs. If the record cannot be written, nothing is signed.
6. The gateway retries with a `PAYMENT-SIGNATURE` header, on a context the client cannot cancel. The tool's x402 server verifies, runs the tool, submits `transferWithAuthorization`, and answers `200` with `PAYMENT-RESPONSE`.
7. The gateway settles the hold. If the answer was a definite "no", it releases the hold. Anything else leaves the hold for the reconciler (next section).

Wire format (x402 v2, HTTP transport): headers `PAYMENT-REQUIRED`, `PAYMENT-SIGNATURE`, `PAYMENT-RESPONSE`, each base64 JSON. Payload fields: `x402Version`, `resource`, `accepted` (the requirements), `payload.signature`, `payload.authorization{from,to,value,validAfter,validBefore,nonce}`. Network is CAIP-2 (`eip155:84532`).

## Screen before sign (I10)

[`policy.Allowed`](../../internal/policy/policy.go) has only unexported fields. Outside package `policy`, `policy.Allowed{}` compiles to the zero value (which reports `Valid() == false` and makes `wallet.Pay` refuse), and a literal such as `policy.Allowed{req: ...}` or `policy.Allowed{ok: true}` does **not compile**: "cannot refer to unexported field ok in struct literal". The only way to get a valid one is an Allow decision from `policy.Evaluate`. An `Allowed` is also tied to its hold id and single use: `wallet.Prepare` spends it with `Allowed.Use(holdID)`, so a second Prepare, or a Prepare for another hold, is refused. So signing without screening is a compile error, and screening once but signing twice is a runtime refusal.

`wallet.Prepare` also checks that the payee, amount and network it is asked to sign are the ones the `Allowed` approved, so a server that changes its price between the 402 and the retry gets a refusal, not a signature.

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

## After signing, the chain is the source of truth

Before the signature, the gateway decides. After it, the token contract decides whether money moved, and the gateway may not learn the answer (a dropped connection, a tool server that crashed after settling, a ledger write that failed). So the gateway never guesses "it failed":

- The hold carries a **ref** (`x402.AuthRef`: payer, nonce, value, `validBefore`, token), written before signing.
- **Release only on a definite no**: the tool answered 402 from *verification* (nothing was accepted or sent), or settlement reported a revert / a send the node rejected (`invalid_transaction_state`). The same `Idempotency-Key` is then spent; a new key retries cleanly.
- **Everything else is "settlement pending"**: a transport error, a 5xx, an unparsable answer, `unexpected_settle_error`, or a ledger failure after the chain succeeded. The gateway answers 202, audits `settlement_pending`, and leaves the hold and the agent's budget charge as they are; the reconciler closes both from the chain fact (policy budgets are persisted, see [policy](policy.md)).
- The **reconciler** (`serve` loop and `agentgate reconcile --payments FILE`) closes such holds after expiry through `x402.AuthLookup`, which asks `authorizationState(from, nonce)`:

| Chain says | Window (`validBefore` + 2 min margin) | Reconciler |
|---|---|---|
| authorization used | any | **settles** the full authorized value (`Source=chain`) |
| unused | still open | **error**: hold stays active, next pass asks again |
| unused | closed | **releases** (`reconciled_unmetered`): it can never execute |

The wait is sound because the hold TTL (5 min) is longer than the 60 s window plus margin, and because the contract checks `block.timestamp < validBefore`: after that no one can submit the signature.

**Trade-off.** A tool-side 4xx/5xx *before* settlement also lands in "pending", because the gateway cannot tell it from a 5xx after settlement. The agent's money is held until the window closes (about three minutes), not returned at once. That is the price of never refunding a transfer that happened. A payment hold must never reach a model-provider lookup, whose "not found" would release it: `RouteLookup` sends provider `x402` to the chain, and with no chain access it errors instead of releasing.

Where the chain is: against Base Sepolia the gateway reads it over `AG_RPC_URL`; the simulated chain lives inside the paid-tool process, so the gateway asks it over HTTP (`chain_query_url`, read-only `GET /chain/authorization-state`).

Other failure modes:

- **Policy refuses.** Hold released; audit `policy_denied`; signer count stays 0.
- **`SendTransaction` errors.** A timeout can lose only the reply, so any send error is "pending" unless the node's standard rejection text proves refusal (nonce too low, underpriced, insufficient funds). `already known` means the node has it.
- **Client disconnects mid-settlement.** The paid retry and everything after it run on a detached context bounded by `maxTimeoutSeconds + 30 s`; the ledger still settles once.
- **Reorgs** after a receipt are not handled (a testnet slice).

## Invariants and the test that proves each

| Invariant | Test |
|---|---|
| After signing the chain decides: response lost / tool 500 / ledger failure all end in a settle for what moved | `TestResponseLostAfterChainSettle`, `TestTool500AfterSettleIsNotRefunded`, `TestLedgerSettleFailsAfterChainSuccess` |
| Unused authorization: held while the window is open, released when closed | `TestSignedButNeverSent`, `TestAuthLookupThreeOutcomes` |
| Reverted transaction releases at once | `TestChainRevertReleasesAtOnce` |
| A client hang-up does not abort settlement | `TestClientDisconnectDuringSettlement` |
| An agent belongs to one account | `TestAgentBoundToAccount`, `TestAgentBelongsToOneAccount` |
| I10 refused payment: signer count zero, no chain tx, ledger and audit rows | `TestRefusedPaymentNeverSigns` (server); `TestWalletBindsPayee...`, `TestWalletRefusals`, `TestWalletRefusesOverflowingAmount` (x402) |
| I11 replay: one transfer, one charge | `TestReplayMovesMoneyOnce` (x402); `TestPaymentReplayChargesOnce` (server) |
| Tampered signature / wrong payee / wrong amount / expired / wrong network rejected | `TestServerRejects` |
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
7. **What happens on a reverted transaction?** Definite no: hold released, budget charge closed at zero, audit `payment_failed`; same key is spent, a new key retries.
8. **Why is a lost connection after the paid retry not released?** The transfer may have happened; the hold stays and the reconciler asks the chain.
13. **When does the reconciler release a payment hold?** Only when the authorization is unused on chain and its window (plus margin) has closed.
14. **Why record the ref before signing?** After the signature exists, only the chain knows if money moved; without the ref the reconciler could not ask.
15. **Why must an agent be bound to an account?** The agent id comes from the request body; without the binding any key could spend under the richest agent.
9. **EIP-3009 versus approve?** One signed transfer with no owner gas, versus a standing allowance that costs an extra transaction.
10. **Why does the offline run prove less than a Base Sepolia run?** It uses a mock token and a simulated chain; real USDC, RPC and fee behaviour are untested.
11. **Which chain ids may the wallet start with?** 84532 and the local simulated chain (1337).
12. **Why must the sim's clock be synced?** Windows are wall-clock seconds; the contract compares them to block time.

</details>
