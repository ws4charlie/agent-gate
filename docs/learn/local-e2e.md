# Local end to end

CI runs the whole gateway with no secret, no faucet and no money. Two layers do it, and each proves less than it looks like it does.

## Layer 1: payments on a local anvil node

`anvil --chain-id 84532` is a real EVM node with the Base Sepolia chain id. Code: [internal/x402/anvil.go](../../internal/x402/anvil.go), [cmd/agentgate/payments.go](../../cmd/agentgate/payments.go), tests [e2e/payments_test.go](../../e2e/payments_test.go).

- **Guard.** `x402.DialAnvil` checks `web3_clientVersion` says anvil and `eth_chainId` is 84532 before returning anything usable. `TestDialAnvilRefusesWhatIsNotAnvil` proves it against a stub node and counts that no other call was made. This is what stops a mint or `anvil_setBalance` reaching a real network.
- **Keys.** None in the repo, including anvil's well-known dev keys (GitGuardian flags them). The wallet key, the facilitator key and the payee address are generated per run. The facilitator pays gas from `anvil_setBalance`; the wallet is funded by minting the mock token.
- **Token.** `MockUSDC` is deployed per run with domain `USDC`, version `2`, the real token's config, so the EIP-712 domain the wallet signs is the real one.
- **Clock.** An authorization window is a chain-time fact. `x402.Clock` is wall time plus whatever `evm_increaseTime` moved the node by; the wallet, the verifier and the reconciler's window check all take it. Tests close a window with `Clock.Advance` and never sleep.

| Test | What it shows |
|---|---|
| `TestAnvilPaymentSettles` | a real EIP-1559 transaction, receipt status 1, payee balance moved by exactly the ledger charge |
| `TestAnvilOverPolicyPaymentSendsNothing` | I10: refused, signer never ran, block number unchanged |
| `TestAnvilResponseLostAfterSettlement` | the reconciler settles from `authorizationState` over RPC (I6, I11) |
| `TestAnvilSignedNeverBroadcastReleasedAfterWindow` | hold kept while the window is open, released after `evm_increaseTime` |
| `TestAnvilToolReportsFailureThenSubmitsChargedOnce` | a lying tool submits twice: one transfer, one charge |
| `TestAnvilReportByAgentReconcilesWithRealTransfers` | `report --by agent` matches ledger holds to real transfers |

## Layer 2: model traffic on fixtures

`testdata/llm/*.json` are hand-written exchanges: `{id, match{model,prompt}, response, usage, cost, upstream_id}`. [fakeupstream](../../internal/fakeupstream/fixtures.go) replays them at the HTTP boundary (`cmd/fakeupstream -fixtures DIR`), matching on the model plus the text of the last message (a tool result counts). It emits the OpenRouter wire shapes: `x-generation-id`, `usage.cost`, cache fields, `[DONE]` and `message_stop`, and a `/generation` endpoint that returns the fixture's cost. An unmatched prompt is a 500 naming the prompt hash. `configs/providers.mock.json` points every route at the mock, so there is no mock branch in the gateway.

[e2e/llm_test.go](../../e2e/llm_test.go) runs a 14-request transcript (chat and Messages, stream and not, a tool call, a cache write then read, a four-turn coding session, an injected tool result) through a real `agentgate serve` process, then asserts from the audit log: exact cost per request, `cost_source`, token classes, upstream id. The bill test checks that the ledger's charges equal the fixtures' costs and the sum of `GET /generation` over the audit rows, and pins the total in `e2e/testdata/bill.golden`. `report.golden` pins the report, cache hit rate included. `e2e/judge_test.go` drives `ProviderJudge` from a fixture.

Mutation check: raise one inline-cost fixture's `cost` and `TestLLMReportGolden` and `TestLLMBillReconciliation` both fail (the pinned total moved), then restore it.

## What the layers prove, and what they do not

| | Proves | Does not prove |
|---|---|---|
| anvil | the RPC client, EIP-1559 fees, receipt waiting, the real EVM running the token's `transferWithAuthorization` | Circle's USDC contract, a public RPC's rate limits and lag, Base's fee market, reorgs |
| fixtures | routing, SSE passthrough, usage parsing, cost source, ledger and report arithmetic, the Judge's request and answer handling | how a real provider streams, bills, rate limits or fails; prompt quality |

Fixture numbers never go in any report of real traffic. They are labelled **local e2e**.

## Quiz

<details><summary>Questions</summary>

1. Why does anvil run with `--chain-id 84532` instead of its default?
2. What stops `anvil_setBalance` from reaching a real network?
3. Why are there no anvil dev keys in the repo, and where do the keys come from?
4. Why does the reconciler's window check use a chain clock and not `time.Now()`?
5. How does a test close an authorization window without sleeping?
6. What does an unmatched prompt return, and why is that better than a default answer?
7. Why can the bill test pass while a fixture's cost is wrong, and what catches that?
8. Which claims does a fixture run support, and which does only a live run support?

</details>

<details><summary>Answers</summary>

1. The signed EIP-712 domain carries the chain id. With 84532 the wallet guard and the signing path are the testnet's, unchanged.
2. `DialAnvil` refuses any node whose `web3_clientVersion` does not say anvil (and any other chain id) before it returns a handle that can mint or set balances.
3. Secret scanners flag them and they are public. The wallet, facilitator and payee are generated at run time.
4. The token contract compares `validBefore` to `block.timestamp`. Chain time is the time that decides whether the authorization can still execute.
5. `evm_increaseTime` then `evm_mine`; `x402.Clock` re-reads the offset.
6. A 500 naming the model and the prompt hash. A test that sends an unrecorded prompt fails instead of passing on a generic answer.
7. The fixture and the gateway both read the same file, so they agree with each other. `bill.golden` pins the total independently.
8. Fixtures support the gateway's accounting and wire handling. Provider behaviour, real prices and real latency need a live run.

</details>
