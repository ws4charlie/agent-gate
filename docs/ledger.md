# Ledger

[中文](zh/ledger.md)

The ledger is where the money is tracked. It works like a hotel holding a deposit on your card. When you check in, they hold more than you will probably spend. When you check out, they charge the real amount and let the rest go.

<p align="center"><img src="images/ledger.png" width="60%" alt="A $0.50 hold on a $10 balance. The call costs $0.12, so $0.12 is charged and $0.38 returned."></p>

Every model call, tool call and payment follows the same pattern. Before anything runs, we hold the most it could cost. Afterwards we charge what it really cost and give back the difference. Say an agent has $10 and makes a call that could cost up to $0.50. We hold $0.50 while it runs. If it ends up costing $0.12, the agent is charged $0.12 and the other $0.38 is free again.

This is what stops a group of agents overspending together. Ten calls at once can't all look at the same balance and decide they can afford it, because each one has to claim its share first.

Most gateways keep a running total. We keep a history that can only be added to, so the balance can be worked out again from scratch at any time. If the two ever disagree, we know something is wrong.

Crashes are the awkward case. If the gateway dies in the middle of a call, the held money would be stuck forever. So a background job looks for holds that have expired and closes them. If it can find out what the call really cost, it charges that.

Some providers, such as OpenRouter, let you look up a past call. Others, such as OpenAI and Anthropic when called directly, do not. After a crash we cannot tell whether such a call ran, and the provider may well have billed us. Returning the money would mean we pay and nobody is charged. So for these providers the job charges the whole hold and marks the row, so you can see it was not the real cost.

This means that in the rare case of a crash, an account can pay up to its hold more than the call cost. We chose that on purpose: a small overcharge can be refunded, but spend nobody was billed for is lost. A call that fails cleanly is not affected. It is returned in full, as before.
