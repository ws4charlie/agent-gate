# Bank rail

[中文](zh/bank.md)

An agent can ask Gate to pay a bank recipient by ACH. Gate checks its own policy first. Then it files the payment at the bank as a **request that waits for a human to approve it there**. Gate never sends money directly. It follows the payment through every state, writes each step to the audit trail, and handles an ACH return.

The bank is Mercury, **sandbox only** (fake money). The rail is off unless you turn it on.

## How a payment goes

```text
agent asks -> Gate policy -> Gate human (above the threshold) -> request at Mercury -> Mercury human approves -> sent -> settled
```

1. The agent calls `POST /v1/bank/payments` with `{recipient_id, amount_usd, idempotency_key, memo}`. The amount is whole cents.
2. Gate decides like any other payment, with tool `mercury/ach`, network `ach` and the recipient id as the payee. The agent's payee allowlist holds Mercury recipient ids. With the registry check on, `mercury/ach` needs an approved registry entry.
   - Refused: `403` (or `429` for a budget) with the rule, an audit row, nothing sent to Mercury.
   - Above the approval threshold: `202 approval_required`. A Gate human runs `agentgate approve allow <id> --by <name>`; the agent repeats the same request and the payment is filed.
   - Allowed: the money is held, the request is filed at Mercury, and the answer is `202 {payment_id, state: "awaiting_bank_approval", mercury_request_id}`.
3. A Mercury user approves or cancels the request in the Mercury dashboard. Gate polls (every 30 seconds by default) and moves the state.

The same `idempotency_key` is one payment. The payment row is created first, before any money is held, so two requests with the same key at once cannot both act: the second gets the stored answer (or `409 request_in_flight` while the first is still deciding), and the bank is not asked twice.

## States

`received` (the claim on the idempotency key, before the decision), `refused`, `awaiting_gate_approval`, `awaiting_bank_approval`, `bank_approved`, `sent`, `settled`, `cancelled`, `failed`, `returned`.

| Mercury says | Gate state | Ledger |
|---|---|---|
| request pending | `awaiting_bank_approval` | money held |
| request approved, transaction pending | `bank_approved` | held |
| transaction sent | `sent` | charged |
| transaction sent and posted | `settled` | charged |
| request cancelled or rejected, transaction cancelled, failed or blocked | `cancelled` or `failed` | hold released, budget freed |
| transaction failed or reversed after it was sent | `returned` | money credited back, budget freed |

A status Gate does not know is logged and the state is kept. Gate keeps watching a sent payment for five days, the time an ACH return can take.

Each state change and its money (the hold, the debit, the release or the refund, and the budget) commit in one database transaction, so a crash leaves both or neither. The regular reconciler never touches the hold of a payment that is not final; only the bank poller settles or releases it.

Every state change writes one row to `bank_payment_events` (an append-only chain: payment, sequence, from, to, who, details) and one audit row, in the same transaction. The chain shows who asked, the amount and payee, the policy verdict and rule, the Gate approver, the Mercury reviewers and the final state. Read it with:

```bash
agentgate bank list
agentgate bank show <payment_id>
```

## Turn it on

```bash
export AG_MERCURY_TOKEN=...         # the sandbox token, from the environment only
export AG_MERCURY_ACCOUNT=<checking account id>
agentgate registry add tool mercury/ach --owner <team> --purpose "ACH through Mercury" --status approved
agentgate serve --mercury           # or AG_MERCURY=true
```

Other settings: `--mercury-base-url` (`AG_MERCURY_BASE_URL`, default the sandbox), `--mercury-poll` (`AG_MERCURY_POLL`, default 30s), `--mercury-proxy` (`AG_MERCURY_PROXY`, see below). The log says `mercury bank rail on (sandbox)`. Without `--mercury` the route answers 404 and nothing else changes.

The token is never logged or stored. It should be a Mercury token that can request payments but not send them directly.

## Two demos

`scripts/bank-demo.sh` runs both against the sandbox. It needs `DATABASE_URL`, the token, `AG_MERCURY_ACCOUNT` and `DEMO_RECIPIENT_ID` (an ACH recipient in that sandbox).

- **Demo A, a normal ACH.** A $1 payment passes policy, is filed at Mercury, and stops at `awaiting_bank_approval`. The script prints that it is waiting for approval in the Mercury dashboard and exits 0 if nobody approves. Gate carries on by itself once someone does: `sent`, then `settled`.
- **Demo B, over the limit.** A $150 payment for an agent with a $100 daily limit is refused at once (`budget_daily`), with an audit row and no call to Mercury.

## The two-person rule

Mercury does not let the user who created an API token approve that token's requests. Someone else with approval rights must do it. The sandbox organisation has one user today, so a demo payment cannot be approved yet. In production the organisation needs a second Mercury user with approve rights. Until then a payment waits at `awaiting_bank_approval`, which is the point of the design: the agent cannot move money alone.

## The simulated return

A real ACH return is found by the poller: Mercury reports the transaction as reversed or failed after it was sent, and Gate moves the payment to `returned`, credits the money back and frees the budget.

**The sandbox cannot produce a return, so this step is simulated.** `agentgate bank simulate-return <payment_id> [--code R01]` runs the same code the poller uses, with the actor `simulated-return`. It works only on a sent or settled payment, and only on a payment whose own record says it was made on the sandbox. Each payment stores the bank it was made on, so a payment from a production gateway can never be simulated back.

## Production notes

Mercury production accepts calls only from IP addresses on the token's whitelist. If the gateway has no fixed address, route Mercury calls through a proxy that has one: `AG_MERCURY_PROXY=http://proxy.internal:3128`. Production also needs a second approver, as above. Moving off the sandbox is a deliberate change: the base URL is set on purpose, the log then says `NOT the sandbox`, and `simulate-return` stays refused.
