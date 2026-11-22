# 银行通道

[English](../bank.md)

智能体可以请 Gate 用 ACH 向银行收款方付款。Gate 先按自己的策略检查，然后把这笔付款作为一个**需要真人在银行那边批准的请求**提交给银行。Gate 从不直接转账。它会跟踪付款的每个状态，把每一步写进审计记录，并处理 ACH 退回。

银行是 Mercury，**只支持沙盒**（假钱）。这条通道默认关闭。

## 一笔付款怎么走

1. 智能体调用 `POST /v1/bank/payments`，body 为 `{recipient_id, amount_usd, idempotency_key, memo}`。金额精确到分。
2. Gate 像对待其他付款一样做决定：工具为 `mercury/ach`，网络为 `ach`，收款方是收款方 id。智能体的收款方白名单里放 Mercury 的收款方 id。开启登记册检查时，`mercury/ach` 需要有已批准的登记项。
   - 被拒：返回 `403`（预算类为 `429`），带规则名，写一条审计，不会向 Mercury 发任何东西。
   - 超过审批阈值：返回 `202 approval_required`。Gate 的真人运行 `agentgate approve allow <id> --by <name>`，智能体用同样的请求重试，付款就会提交。
   - 允许：先冻结资金，再向 Mercury 提交请求，返回 `202 {payment_id, state: "awaiting_bank_approval", mercury_request_id}`。
3. Mercury 的用户在 Mercury 后台批准或取消这个请求。Gate 轮询（默认每 30 秒）并更新状态。

同一个 `idempotency_key` 代表同一笔付款。付款记录在冻结任何资金之前先创建，所以同一个键的两个并发请求不会都去执行：后到的得到已保存的答复（第一个还在决定时返回 `409 request_in_flight`），银行也不会被问两次。

## 状态

`received`（幂等键的占位，决定之前）、`refused`、`awaiting_gate_approval`、`awaiting_bank_approval`、`bank_approved`、`sent`、`settled`、`cancelled`、`failed`、`returned`。

| Mercury 的状态 | Gate 状态 | 账本 |
|---|---|---|
| 请求待批 | `awaiting_bank_approval` | 资金冻结 |
| 请求已批准，交易待处理 | `bank_approved` | 冻结 |
| 交易已发出 | `sent` | 扣款 |
| 交易已发出并入账 | `settled` | 扣款 |
| 请求被取消或拒绝，交易被取消、失败或拦截 | `cancelled` 或 `failed` | 释放冻结，预算归还 |
| 交易在发出后失败或被冲回 | `returned` | 资金退回账户，预算归还 |

Gate 不认识的状态会被记录，状态保持不变。已发出的付款会被继续观察五天，这是 ACH 退回可能需要的时间。

每次状态变化都会在同一个事务里写一行 `bank_payment_events`（只追加的链：付款、序号、前态、后态、操作者、详情）和一条审计记录。这条链能看出谁发起、金额和收款方、策略结论和规则、Gate 审批人、Mercury 审批人以及最终状态。查看：

```bash
agentgate bank list
agentgate bank show <payment_id>
```

## 开启

```bash
export MERCURY_API_TOKEN=...        # 或 AG_MERCURY_TOKEN；沙盒 token，只从环境变量读取
export AG_MERCURY_ACCOUNT=<支票账户 id>
agentgate registry add tool mercury/ach --owner <团队> --purpose "ACH through Mercury" --status approved
agentgate serve --mercury           # 或 AG_MERCURY=true
```

其他设置：`--mercury-base-url`（`AG_MERCURY_BASE_URL`，默认沙盒）、`--mercury-poll`（`AG_MERCURY_POLL`，默认 30 秒）、`--mercury-proxy`（`AG_MERCURY_PROXY`，见下）。日志会写 `mercury bank rail on (sandbox)`。不加 `--mercury` 时该路由返回 404，其他行为完全不变。

token 不会被记录或保存。它应当是只能“请求付款”、不能直接转账的 Mercury token。

## 两个演示

`scripts/bank-demo.sh` 在沙盒里运行两个演示。需要 `DATABASE_URL`、token、`AG_MERCURY_ACCOUNT` 和 `DEMO_RECIPIENT_ID`（该沙盒里的一个 ACH 收款方）。

- **演示 A，正常 ACH。** 一笔 1 美元的付款通过策略，提交给 Mercury，停在 `awaiting_bank_approval`。脚本会打印“正在等待 Mercury 后台批准”，没人批准就以 0 退出。有人批准后 Gate 会自己继续：`sent`，然后 `settled`。
- **演示 B，超限。** 日限额 100 美元的智能体请求 150 美元，立刻被拒（`budget_daily`），写一条审计，不会调用 Mercury。

## 两人规则

Mercury 不允许创建 API token 的用户批准该 token 发起的请求，必须由另一位有批准权限的人来做。沙盒组织目前只有一个用户，所以演示付款暂时无法被批准。生产环境需要第二个有批准权限的 Mercury 用户。在那之前付款会停在 `awaiting_bank_approval`，这正是设计的目的：智能体无法独自转走钱。

## 模拟退回

真实的 ACH 退回由轮询发现：Mercury 报告交易在发出后被冲回或失败，Gate 把付款改为 `returned`，把钱退回账户并归还预算。

**沙盒无法产生退回，所以这一步是模拟的。** `agentgate bank simulate-return <payment_id> [--code R01]` 运行与轮询相同的代码，操作者记为 `simulated-return`。它只对已发出或已入账、并且自己的记录显示在沙盒上创建的付款有效。每笔付款都记录它所在的银行环境，所以生产网关的付款永远不会被模拟退回。

## 生产环境须知

Mercury 生产环境只接受 token 白名单内 IP 的调用。如果网关没有固定地址，把 Mercury 调用经由一个有固定地址的代理：`AG_MERCURY_PROXY=http://proxy.internal:3128`。生产环境同样需要第二位审批人。离开沙盒是一次有意的改动：base URL 必须明确设置，日志会写 `NOT the sandbox`，`simulate-return` 仍然会被拒绝。
