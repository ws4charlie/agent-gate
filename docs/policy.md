# Policy

[中文](zh/policy.md)

An API key with a budget treats an agent as a billing line. We treat it like a bank customer: it has an identity, it has limits, and someone can freeze the account.

<p align="center"><img src="images/policy.png" width="60%" alt="Five checks in order. The first failure decides."></p>

## What you control

- which models and tools it may use, and who it may pay
- how much it may spend in a day
- how fast it may spend, which catches an agent stuck in a loop
- how large a single spend can be before a person must approve it

Anything you don't list is refused. An agent can't approve its own requests or switch off its limits, because that takes direct database access, which it never has.

## Rules run in order

The first rule that fails gives the answer. The agent is told which rule stopped it, so it can change course instead of retrying blindly. Yes and no decisions are both recorded.

## When a person must approve

The money stays held and the request waits for a decision. After a yes, the agent sends the same request again and it runs once. An approval covers only that request, can't be swapped for something else, and expires if nobody uses it.

## One command stops an agent

New requests are refused and running calls are cut off within about a second. You pay for what already happened.

## Where the limits are soft

- **Task budgets rely on the client.** The client names the task, so a hostile agent can invent a new one for each call.
- **Budget checks use an estimate.** If our price table is out of date or a request is very large, one call can occasionally land over budget. The account balance is the real ceiling.
