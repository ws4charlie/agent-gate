# MCP gateway, screening and runs

Model calls and tool calls are one control plane: a tool call is authenticated, decided by the same policy engine, metered by the same ledger, and logged in the same audit table. What the tool returns is screened before the agent reads it, and every request can belong to a run that is inspected or killed as a unit. Code: [internal/mcp](../../internal/mcp/jsonrpc.go) (protocol), [internal/server/mcp.go](../../internal/server/mcp.go) (the gate), [internal/screen](../../internal/screen/screen.go), [internal/run](../../internal/run/run.go), [internal/server/run.go](../../internal/server/run.go). Read [policy](policy.md) first: nothing here adds a second gate.

## What the spec says (2026-07-28)

Read from modelcontextprotocol.io on 2026-11-03; the session notes in the plan were right that sessions are gone, but they were secondary. In this revision:

- There is **no `initialize`** and no session. Each POST carries `protocolVersion` and `clientCapabilities` in `params._meta`; the HTTP headers `MCP-Protocol-Version`, `Mcp-Method` and `Mcp-Name` mirror them, and a server that processes the body must reject a header that disagrees (`400`, `-32020`).
- An unknown version gets `-32022` with the supported list. A notification gets `202`. A request gets JSON or an SSE stream scoped to that request. GET (the old standing stream) and DELETE are `405`; `Mcp-Session-Id` is ignored.
- Old clients are told, not served: a server that supports only this revision should name its versions in the error it returns to `initialize`. That is what [`mcp.Check`](../../internal/mcp/jsonrpc.go) does.
- Error codes `-32020..-32099` belong to the spec, so the gateway's own refusals use `40201/40202/40301/40302/40901/50201`.

The gateway targets this one revision, so an older client or server is refused. Checked once against `@modelcontextprotocol/server-everything` 2.1.0; everything else runs against the in-repo fake. It is stateless because the protocol is: there is nothing to remember between requests, so a gateway restart loses no MCP state.

## The flow of `tools/call`

1. Origin check (a browser origin is refused unless listed), then authenticate (`401` is the only auth error), then rule 1 (killed or revoked agents are refused before the body is read, I12).
2. Parse, then check the headers against the body. Then [`beginRun`](../../internal/server/run.go): attach the run, write the step, refuse a killed run.
3. Price from config. **Priced:** approval check, `Evaluate` (scope, budget, velocity, threshold), `Reserve` kind `tool`, `Charge` the budget windows, exactly as a model call ([`toolCall`](../../internal/server/mcp.go)). **Free:** the same engine with the limits removed ([`scopeOnly`](../../internal/server/mcp.go)); no hold; decision logged with amount 0.
4. Forward with the gateway's credential, the client's `Authorization` dropped. The call is registered with the kill registries (by agent and by run).
5. Screen what comes back. JSON: the whole result. SSE: each event, forwarded as it passes.
6. Settle at the price (or release), write the audit row, close the run step.

Why a refusal is HTTP 200: the transport worked and the call was refused; JSON-RPC carries that as an error object. Only malformed requests and bad credentials are HTTP errors.

## Why a free tool needs a deliberate path

`Evaluate` refuses `Amount <= 0` as `invalid_request`. That check protects models and payments, so it stays. A free tool still needs a decision (scope, status), so the gateway asks the engine with `DailyBudget`, `TaskBudget`, `VelocityPerMinute` and `ApprovalThreshold` set to 0 (no limit) and a nominal amount of 1. Only the scope and status rules can fire. No policy code changed, and no budget is touched by a call that costs nothing.

## Screening

Layer one is a list of rules ([`rules.go`](../../internal/screen/rules.go)); layer two is an optional `Judge`. Rules run on every string in the result (`ExtractTexts` walks the JSON, skipping image/audio bytes), in this order: invisible characters (they also hide the others), instruction-override phrases, role markers, exfiltration URLs, then base64/hex/percent runs decoded once and re-scanned. A hit blocks the call result: the client gets error `40302` with the rule id; `screen_events` gets the rule and a SHA-256 of the text; the log line carries the same two things. The content is never stored (I9). The audit row has status `screened` and the rule id in `err`.

What it cannot do is as important as what it does: see the README. The evasion samples are in the repository and a test fails the day one starts being caught, which is the prompt to move it into the malicious corpus.

## Runs

`runs(id, agent_id, status)` and `run_steps(run_id, kind, name, request_id, hold_id, status, cost_nanos, ...)`. A step is inserted when a request is admitted and closed from `finish` with the audit status. **Spend is not stored**: [`Spend`](../../internal/run/run.go) sums the `settle` entries of the holds the steps used, so the ledger stays the one source of truth for money (I2).

Kill by run (I12): `agentgate run kill` writes `status='killed'`. The next request naming the run is refused (`run_killed`). For calls in flight, `trackInFlight` registers the cancel func under the agent id and the run id; `WatchKills` polls both every 250 ms (only while something is in flight) and cancels. A killed tool call is released (nothing arrived); a killed stream settles what arrived (I8).

## Invariants and their tests

| Invariant or rule | Test |
|---|---|
| Out-of-scope call never reaches upstream | `TestMCPOutOfScopeCallNeverReachesUpstream` |
| `tools/list` is filtered | `TestMCPToolsListIsFilteredByAllowlist` |
| Free tool: decided, not charged, limits do not apply | `TestMCPFreeToolIsDecidedButNotCharged` |
| I9 upstream credential and client secret never cross | `TestMCPPricedCallSettlesAndKeepsCredentialsApart` |
| I9 blocked content is never logged, only hash and rule | `TestMCPScreenedResultNeverReachesClient`, `TestVerdictCarriesNoContent` |
| I12 kill by run, in flight and next request | `TestKillByRun_I12` |
| I12 kill by agent still cancels tools | `TestKillAgentCancelsToolCall` |
| I2 a run's spend equals the ledger | `TestRunStepsInOrderAndSpendFromLedger` (and `ledger.Audit` via `noActiveHolds`) |
| Steps in order, payments included | `TestRunStepsInOrderAndSpendFromLedger`, `TestRunIncludesPayments` |
| Protocol rules of 2026-07-28 | `TestMCPProtocolRules`, `TestMCPTransportEdges` |
| Screening corpus and known evasions | `TestCorpus`, `TestKnownEvasionsPass` |

## Failure modes

- **Upstream down or non-2xx:** the hold is released, the client gets `50201`, the upstream's error body is not relayed (it is text from outside).
- **Gateway dies between reserve and settle:** the hold expires and the reconciler closes it (I6), like any hold. Its kind is `tool`; with no usage to look up it is released and marked unmetered. The reconciler's audit row carries the hold's own kind (`mcp` for a tool hold, read from the `holds` row; `TestDeadToolHoldIsReleasedByReconciler_I6`).
- **Screen judge down (when enabled):** the content is blocked (`judge_unavailable`), not passed.
- **SSE stream ends without a result:** released, an error event ends the stream.

## Gotchas hit

- `c.fail` had to learn to answer in JSON-RPC, or every shared helper (`denyResponse`, `charge`, `reserve`) would have written a non-MCP body on the MCP route. One flag on the request, one place.
- `Registry` is keyed by a string: a second instance serves run ids, no change to the agents package.
- The held ledger kind was hard-coded to `model`; the MCP route needed `tool`.
- The policy package has no exported matcher, so "is this tool listed" goes through `Evaluate` rather than a second matcher.

## Quiz

<details>
<summary>Questions and answers</summary>

1. **Why does the gateway answer `initialize` with an error?** The targeted revision has no handshake; the spec asks a modern-only server to name its versions in that error so an old client can say why it failed.
2. **Why validate `Mcp-Name` against the body?** The gateway decides policy from the body. A header that differs could let an intermediary and the gateway disagree about which tool is called.
3. **An out-of-scope call returns HTTP 200. Why?** The transport worked; JSON-RPC reports a refused call as an error object.
4. **How does a free tool get a decision when `Evaluate` rejects amount 0?** The engine is asked with every limit at 0 and a nominal amount, so only scope and status rules can fire; nothing is held or counted.
5. **A tool returns an injection and the screen blocks it. Is the agent charged?** Yes, at the price: the tool ran. The audit row says `screened`.
6. **What does the log contain for a blocked result?** The rule id and a SHA-256 of the text. Never the text.
7. **Name three things rule-based screening misses.** Paraphrase, other languages, split or look-alike words (also ROT13, plain requests, images).
8. **Where is a run's spend computed, and why not stored?** From `ledger_entries` via the steps' hold ids, so it cannot disagree with the ledger audit.
9. **How is a run killed while a call is in flight?** The CLI writes `status='killed'`; the watcher polls the run registry against the database and cancels the registered calls.
10. **A killed run's next request?** Refused with `run_killed` before anything is reserved; the refusal is still a step.
11. **Why can another agent not use my run id?** A run belongs to the agent that first used the id; any other agent gets `run_forbidden`.
12. **What is not screened?** `tools/list` descriptions, resources, prompts, image and audio content.

</details>
