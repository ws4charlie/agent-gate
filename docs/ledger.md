# Ledger

The ledger is where the money is tracked. It works like a hotel holding a deposit on your card. When you check in, they hold more than you will probably spend. When you check out, they charge the real amount and let the rest go.

![A $0.50 hold on a $10 balance. The call costs $0.12, so $0.12 is charged and $0.38 returned.](images/ledger.png)

Every model call, tool call and payment follows the same pattern. Before anything runs, we hold the most it could cost. Afterwards we charge what it really cost and give back the difference. Say an agent has $10 and makes a call that could cost up to $0.50. We hold $0.50 while it runs. If it ends up costing $0.12, the agent is charged $0.12 and the other $0.38 is free again.

This is what stops a group of agents overspending together. Ten calls at once can't all look at the same balance and decide they can afford it, because each one has to claim its share first.

Most gateways keep a running total. We keep a history that can only be added to, so the balance can be worked out again from scratch at any time. If the two ever disagree, we know something is wrong.

Crashes are the awkward case. If the gateway dies in the middle of a call, the held money would be stuck forever. So a background job looks for holds that have expired and closes them. If it can find out what the call really cost, it charges that. If it can't, it returns the money and flags the row, so you can see that something went unbilled.

Today, finding the real cost after a crash only works for providers that let you look it up afterwards, which in practice means OpenRouter.
