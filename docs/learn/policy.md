# Policy

Custody for agents: each agent has a scoped credential, and one engine decides every model call, tool call and payment before money moves. Code: [internal/policy](../../internal/policy/policy.go) (the pure decision), [internal/agents](../../internal/agents/agents.go) (identities, budget counters), [internal/approval](../../internal/approval/approval.go) (the queue), [internal/server/policy.go](../../internal/server/policy.go) (the gate in the request flow). Read [ledger](ledger.md) first: the budget statements copy its trick.

## Model in one minute

1. A credential (`ag_...`, stored as SHA-256, I9) maps to one **agent**. The agent has one ledger account and a **scope**: allowed models, tools, payees; a daily budget; a per-task budget; a velocity limit (spend per rolling minute); an approval threshold. A zero limit means "no limit". An empty allowlist means "nothing" (default deny).
2. A request becomes a `policy.Request` (agent, kind, model or tool, payee, amount, task). The amount is the worst-case price the gateway would hold anyway.
3. `agents.Store.Snapshot` reads the agent's status and counters. `policy.Evaluate(Request, Snapshot)` is a pure function: no database, no clock. It returns `Allow`, `Deny` or `RequireApproval`, a rule id and a reason. The server logs it (`policy_decisions`, append-only), allow included.
4. `Deny`: refuse (403, or 429 for budget and velocity), no hold. `RequireApproval`: reserve the hold, park the request, answer 202. `Allow`: reserve the hold, then **charge** the budget windows with one conditional statement, then go upstream or to the wallet.
5. When the hold settles or is released, the windows are corrected to the real cost. The reconciler does the same for a dead process's hold.

## The rules and their order

| # | Rule ids | Fires when |
|---|---|---|
| 1 | `agent_killed`, `agent_revoked` | the credential is not active. Checked right after authentication, before the body is read |
| 2 | `scope_model`, `scope_tool`, `scope_payee` | the name is not matched by the allowlist (exact, `*`, or `prefix*`; payees case-insensitive) |
| 3 | `budget_daily`, `budget_task` | spent so far plus this request exceeds the window |
| 4 | `velocity` | spend in the last 60 s plus this request exceeds the per-minute limit |
| 5 | `approval_threshold` | one request above the threshold, and no human has approved this exact request yet |

The first rule that fires decides. Why this order: a dead credential must learn nothing else; "never" (scope) outranks "not now" (budget, velocity); a person is only asked about requests that are otherwise allowed. [`TestEvaluateRuleOrderCollisions`](../../internal/policy/policy_test.go) checks every pair.

## Budgets without a race (I5 for budgets)

Reading the spent total, comparing, then writing would let N parallel requests all pass a check that holds one. So the window is one statement ([`Charge`](../../internal/agents/agents.go)):

```sql
INSERT INTO budget_windows (agent_id, window_key, spent_nanos)
SELECT $1, $2, $3 WHERE $4 = 0 OR $3 <= $4            -- first spend of the window
ON CONFLICT (agent_id, window_key) DO UPDATE
SET spent_nanos = budget_windows.spent_nanos + $3
WHERE $4 = 0 OR budget_windows.spent_nanos + $3 <= $4  -- the row is only changed if it still fits
RETURNING spent_nanos
```

No row returned means "does not fit". Daily and task windows run in one transaction, so a refusal changes nothing. Velocity sums many rows, which a row-level condition cannot guard, so that insert runs under a per-agent advisory lock in the same transaction. The window keys are `day:2026-11-03` (UTC) and `task:<X-AgentGate-Task>`; a request without the header counts as the task `default`, so omitting it cannot dodge a task budget.

`Evaluate` reads a snapshot that may be stale; `Charge` is what enforces. A request that loses the race after its hold exists releases the hold and is refused with the rule that ran out ([`call.charge`](../../internal/server/policy.go)).

## Budgets that follow real spend

T3's in-memory budget was counted at check time and never refunded. Now `budget_charges` has one row per admitted hold with the windows it was counted in. `CloseCharge(holdID, actual)` runs once per hold, from the request handler or the reconciler, whichever gets there first, and moves the windows from the estimate to the real cost (0 for a release). Because each charge row holds the amount, the same table is the velocity ledger. A hold that was never charged is a no-op.

## Approvals

Life of an entry: `pending` → `approved` → `consumed`, or `denied`, or `expired`. Every change is a conditional `UPDATE` on `status`, so Allow racing Deny, or an approval racing its expiry, has one winner.

- **Queue.** An over-threshold request reserves its hold (money stays locked), stretches the lease over the approval window ([`queueApproval`](../../internal/server/policy.go)), inserts a row, and answers `202 approval_required` with the id. Asking again while pending returns the same id.
- **Decide.** `agentgate approve allow ID --by NAME` or `deny ID`. The approver is recorded (`--by`, else `$AG_APPROVER`, else the OS user). Deny releases the hold.
- **Release once.** After Allow the agent replays the request with the same `Idempotency-Key`. [`checkApproval`](../../internal/server/policy.go) finds the row by (agent, key), compares the **fingerprint** (kind, model or tool, payee, amount, network, task, body hash), and spends the approval with `Consume`. The replay reuses the claim and the hold from queue time (`Reserve` with the same key and amount returns the same hold, I4). A second replay is 409.
- **Never a different request.** A changed model, amount, payee, task or body under the same key is 422 `approval_mismatch` and does not spend the approval.
- **Only the threshold.** An approved replay still goes through rules 1-4, and the charge.
- **Expiry.** `ApprovalTTL` (default 15 min) covers pending and approved-but-unused. A replay, a late Allow, or the sweeper (`serve` runs it with the reconciler) marks it expired and releases the hold.
- **No self-approval.** No HTTP route exists to approve, create agents or kill ([`TestNoHTTPRouteToApprove`](../../internal/server/policy_test.go)); the CLI needs the database.

## Kill switch and revoke (I12)

`agentgate kill --agent X` writes `status='killed'` (reversible with `--resume`); `agent revoke` writes `revoked` (permanent). The next request is refused by rule 1. For in-flight calls, the server keeps an in-process [`Registry`](../../internal/agents/registry.go) of cancel funcs per agent, registered around the upstream call ([`trackInFlight`](../../internal/server/policy.go)). The CLI is another process, so [`WatchKills`](../../internal/server/policy.go) polls every 250 ms (only while something is in flight) and cancels the calls of any agent that is no longer active. It cancels the **upstream** context, not the client's, so the handler survives to settle: usage received so far is charged (I8, `cost_source=estimated` when the cut came before the usage event), the rest released, audit status `killed`.

## Payments and the Allowed value (I10)

[`Allowed`](../../internal/policy/policy.go) is still unconstructible outside the package; the only source is an Allow decision. It is now **tied to a hold id and single use**: `wallet.Prepare` ends by calling `Allowed.Use(holdID)`, which fails for another hold or a second time, and copies of an `Allowed` share one used flag. For payments the hold is reserved before `Evaluate` so the id exists; a refusal releases it (hold, release rows).

## Failure modes

- **Gateway dies after charge, before settle.** The hold expires, the reconciler settles or releases it and closes the charge.
- **`CloseCharge` fails.** Logged; the reconciler closes it (it is called for every outcome).
- **Kill arrives between auth and registration.** The next poll cancels it (the registry is checked against the database, not a one-shot event).
- **Policy snapshot fails.** 500, nothing held (payments: hold released).
- **Several gateway processes.** Out of scope: the registry is per process.

## Gotchas hit

- Postgres could not infer parameter types when `$3` was both a selected value and compared (`inconsistent types deduced`): explicit `::bigint` casts.
- `Evaluate` first checked "invalid amount" and then "killed", so a malformed request to a killed agent got 400. Kill/revoke now comes first.
- A refused replay of an approved request left its hold active (the approval was already spent): `releaseApproved`.
- The reconciler test that advanced time by `HoldTTL` stopped working when payment holds got a longer lease up front; the lease is now stretched only when a request is queued.

## Quiz

<details>
<summary>Questions and answers</summary>

1. **Why is `Evaluate` pure, and what does that buy?** No I/O or clock, so every rule and every collision is a table test, and the same input always yields the same decision.
2. **If the engine already read the budget, why does `Charge` exist?** The read can be stale under concurrency. Only the conditional upsert guarantees N parallel requests cannot exceed the window.
3. **A request breaks the scope and the budget. Which rule id is reported?** `scope_*`: scope is rule 2, budget rule 3, and the first rule that fires decides.
4. **How does the daily window return to real spend?** `CloseCharge` replaces the estimate with the settled amount (0 for a release) once per hold, from the handler or the reconciler.
5. **Why does a missing `X-AgentGate-Task` count as task `default`?** Otherwise omitting the header would escape the task budget.
6. **What stops an approval from being used for a bigger request?** The fingerprint stored with the approval: any change to model, tool, payee, amount, network, task or body gives 422 and does not spend it.
7. **Two replays of one approval arrive together. What happens?** `Consume` is `UPDATE ... WHERE status='approved'`; one gets a row, the other gets 409.
8. **Does approval skip the budget?** No. It lifts only rule 5; the replay is evaluated and charged again.
9. **Why can an agent not approve itself?** There is no HTTP route; approving needs the database through the CLI.
10. **How is a kill delivered to a running stream, and why cancel the upstream context?** The CLI writes the database; `WatchKills` finds the agent inactive and calls its registered cancel funcs. Cancelling upstream (not the client) lets the handler settle what arrived.
11. **What makes an `Allowed` single use?** A shared atomic flag consumed by `Use(holdID)`, which also checks the hold id.
12. **What happens to a T2 API key?** A migration made it an agent with the default scope: any model, any tool, no limits, no payees.

</details>
