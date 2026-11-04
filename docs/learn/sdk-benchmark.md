# SDK, benchmark and demo

The consumer surface and the proof. The SDK is what an agent author imports; the benchmark shows the assembled gateway reaches the right decision across a mix of requests; the demo shows it live. Code: [sdk/python/agentgate](../../sdk/python/agentgate/client.py), [benchmark](../../benchmark/main.go), [demo/demo.sh](../../demo/demo.sh), [demo/agents](../../demo/agents/runaway.py). Read [policy](policy.md) and [mcp-runs](mcp-runs.md) first: this layer only calls what they built.

## The SDK in one minute

`Client(base_url, key)` wraps four verbs: `chat` / `chat_stream` (`/v1/chat/completions`), `tool_call` / `tools_list` (`/mcp/{server}`), `pay` (`/v1/payments`), and `run()`, a context manager that stamps `X-AgentGate-Run` on every call inside it so a run can be inspected or killed as a unit. Every call gets a fresh `Idempotency-Key` unless the caller passes one; an approved request is replayed with the original key.

Two things the SDK does so the caller does not have to:

1. **The 2026-07-28 MCP shape.** `tool_call` puts `protocolVersion` and `clientCapabilities` in `params._meta` and sends `MCP-Protocol-Version`, `Mcp-Method` and `Mcp-Name` ([`_rpc`](../../sdk/python/agentgate/client.py)). Without them the gateway answers `400`/`-32020`.
2. **Errors by rule id.** [`from_rule`](../../sdk/python/agentgate/errors.py) maps the rule to a class. The HTTP status is not used: `429` carries `budget_daily`, `budget_task` and `velocity`, `403` carries scope and kill rules, and an MCP refusal is HTTP `200` with a JSON-RPC error whose `data.rule` holds the same ids.

| Answer | Python |
|---|---|
| `budget_daily`, `budget_task` | `BudgetExceeded` |
| `velocity` | `VelocityLimited` |
| `scope_model`, `scope_tool`, `scope_payee` | `ScopeDenied` |
| `agent_killed`, `agent_revoked`, `run_killed` | `Killed` |
| `duplicate_request` | `Duplicate` (`.original_request_id`) |
| JSON-RPC `40302` | `ScreenedOutput` (`.rule` is the screen rule) |
| `202 approval_required` / JSON-RPC `40202` | `ApprovalRequired(id)` |
| `202 settlement_pending` | `SettlementPending` |
| anything else | `AgentGateError` (`.rule`, `.status`, `.request_id`) |

A `202` is never returned as a result. `approval_required` means a human must decide; `settlement_pending` means a payment was signed and may have settled, and the gateway will resolve it from the chain. They are different types because the right reaction differs: wait and replay with the same key, versus do nothing and never pay again with a new key.

**Not in the SDK: `balance()`.** The task listed it, but the gateway has no HTTP route for balances (the CLI reads the database), and adding one is a change to `internal/server`, outside this task. The SDK does not guess a URL.

## The benchmark

`go run ./benchmark` ([main.go](../../benchmark/main.go), [driver.go](../../benchmark/driver.go), [report.go](../../benchmark/report.go)) builds the same wiring `serve` uses, in process: the real `server.Server`, a real Postgres database created for the run, the fake model upstream, the in-repo fake MCP server, and the simulated chain with the real wallet and x402 verifier. Nothing is mocked inside the gateway.

A scenario ([scenarios/*.json](../../benchmark/scenarios/)) is `{id, category, setup, steps, expected, rule}`. `setup` is the agent's scope (USD amounts; `@payee` and `@other` are the paid tool's address and another one). `steps` play in order; ops are `chat`, `tool`, `pay`, `screen`, `kill`, `revoke`, `kill_run`, `approve`. **Only the last step is judged**; the earlier ones prepare (burn a budget, kill the agent, approve a request). It passes when the verdict (`allow`, `deny`, `approval`) **and** the rule id match.

- **Outcome from the body, not the status.** [`classify`](../../benchmark/driver.go) reads `rule`, then `data.rule`, then `code`. `202 approval_required` is `approval`; any other `202` is `pending`, which matches no expectation, so an unresolved payment fails a scenario instead of passing as success.
- **Velocity needs overlap.** Spend counts at its worst-case estimate while a call is in flight. The fake upstream answers after 200 ms so parallel calls are all in flight together. With no delay the first call settled to its small real cost before the second arrived and the scenario was flaky.
- **Why allow cases.** 11 of 45 scenarios must be allowed. A gateway that refuses everything has deny recall 100% and allow recall 0%, and fails.
- **Precision and recall** are computed per class from the verdict only; the rule id is checked per scenario. [`results.md`](../../benchmark/results.md) has the matrix.
- **Screen rows.** Five injection scenarios give a corpus file straight to the screen (`op: screen`); one goes through the gateway and the fake MCP server's `inject` tool.
- **Known misses.** The six files in `internal/screen/testdata/evasions` run through the screen and are reported as expected misses, outside the pass/fail counts. A file that starts being caught **fails the run**, to force it into the scored set (the same intent as `TestKnownEvasionsPass`).
- **The task budget is not a hard limit.** The caller picks `X-AgentGate-Task`. `allow-new-task-new-window` demonstrates it: after task `t1` is exhausted, `t2` is allowed.
- **After the run** the benchmark calls `ledger.Audit`, so a scenario that corrupted the books (I1, I2) fails the run too.

CI regenerates `results.md` and the README table and fails if the committed copies differ ([ci.yml](../../.github/workflows/ci.yml), job `benchmark`).

### Mutation check

Setting `scope.VelocityPerMinute = 0` in `agents.Store.Create` (rule disabled for every agent) made `velocity-parallel-chat`, `velocity-parallel-chat-wide` and `velocity-tool-sequential` fail: 42/45, exit 1, deny recall 90.3%. The change was reverted. If a rule can be switched off without the benchmark noticing, the benchmark is decoration.

## The demo

[`demo/demo.sh`](../../demo/demo.sh) runs the Phase 2 acceptance scenario with Python agents on the SDK and checks each step: (1) `runaway.py burn` stops at `budget_daily`; (2) `runaway.py velocity` gets a `velocity` refusal; (3) `paying.py queued` waits, `approve allow`, `paying.py replay` pays once, and a $0.50 payment is refused before signing; (4) a streaming agent is killed and is gone within seconds, then refused; (5) `injected.py` is refused `scope_tool` and has an injected result blocked. It ends by proving no hold is left active. `DEMO_PAUSE=5` slows it for narration (about five minutes).

The agents contain no model: they drive the fake upstream. They exist to show the gateway, not to be clever.

## Failure modes and gotchas

- **A cut stream does not raise.** The kill closes the connection; `Stream` catches the transport error and `finished` stays `False`. A caller that needs to know checks it. The *next* request raises `Killed`.
- **Local Python.** The system `python3` here is 3.9 without httpx. The SDK needs 3.11+; run tests with an interpreter that has httpx and pytest, plus `PYTHONPATH=sdk/python`. CI installs from `pyproject.toml`.
- **pytest shares a database.** It migrates and adds uniquely named agents to the `DATABASE_URL` database and drops nothing; it does not create a database of its own.
- **Velocity in pytest** uses threads for the same reason as the benchmark: sequential calls settle to their real, much smaller cost.
- **A killed agent's last events.** The number of events a killed stream delivered varies by timing; tests assert "fewer than the full run", not an exact count.

## Quiz

<details>
<summary>Questions and answers</summary>

1. **Why map exceptions from the rule id and not the HTTP status?** The same status carries different rules (429: daily, task, velocity), and MCP refusals are HTTP 200. Only the rule says what happened.
2. **What do `ApprovalRequired` and `SettlementPending` have in common, and why are they different types?** Both arrive as 202 and neither is success. Approval: no money has moved, wait for a human and replay with the same key. Settlement pending: money may have moved, resolve it, never pay again with a new key.
3. **What does `tool_call` add that a plain POST would miss?** `_meta` with the protocol version and capabilities, and the `MCP-Protocol-Version`, `Mcp-Method`, `Mcp-Name` headers; without them the gateway rejects the request.
4. **Why does a scenario judge only its last step?** Earlier steps create the situation (burn, kill, approve); the question is how the gateway treats the final request in that situation.
5. **Why do the benchmark and its README table fail CI when stale?** So the published numbers are always what the current code produces.
6. **A gateway that refuses everything: what does it score?** Deny recall 100%, allow recall 0%, allow precision undefined or zero, so the 11 allow scenarios fail.
7. **Why are the six evasion samples outside the pass/fail count?** A miss is their expected result. Counting them as passes would hide that the screen misses them; counting them as failures would make CI permanently red. They are listed, and the run fails only if one is newly caught.
8. **Why does the velocity scenario fire calls in parallel against a slow upstream?** Spend is counted at the worst-case estimate while a call is in flight; sequential fast calls settle to their small real cost and never reach the limit.
9. **Is the per-task budget a hard limit, and what shows it?** No. The caller picks `X-AgentGate-Task`; `allow-new-task-new-window` shows a new value starting a new window.
10. **What did the mutation check prove?** Disabling velocity fails three scenarios and exits 1, so the benchmark detects a missing rule.
11. **Why can the SDK not offer `balance()`?** The gateway has no balance route; adding one is a gateway change outside this task.
12. **A stream is killed mid-flight. What does the SDK show?** The iterator ends early with `finished == False`; the next call raises `Killed`.

</details>
