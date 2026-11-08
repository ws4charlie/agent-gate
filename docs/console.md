# Console

[中文](zh/console.md)

The console is a local web page for the person who runs the gateway. It shows what every agent is doing and spending, lets you change their limits, and runs research tasks from templates.

## See every agent at a glance

The overview shows totals across every agent: what was spent today and over seven days, the wallet's USDC balance read from the chain, how many calls were made in the last day and how many of them were refused, failed or blocked, and which models and tools the money went to.

The agents page gives each agent a short card: today's spend against its daily budget, and any problems. Open an agent to see its own spend, its activity, an hourly chart, and what it is allowed to use.

Every count is a link. Clicking an agent's errors opens the events page already filtered to that agent and to errors, so you go from "something is wrong" to the rows that say what in one click.

## Change limits without the command line

From an agent's page you can rename it, set its budgets and allowlists, or kill it and bring it back. A rename keeps its key working. The change applies to the agent's next request. There is no separate copy of the rules: the console writes the same policy the gateway already enforces.

## Run research tasks

A task is a reusable research job, a bit like a skill: instructions, one model, the tools it may use, and two budgets, one per run and one per day. A few public research tasks ship with it. You can add your own, edit any task in place (a built-in one can be reset to how it shipped), and drag the list into the order you like.

Each task runs as its own agent. Every model call, search and payment goes through the gateway with that agent's key, so the budgets are ordinary policy, the spend shows on the agent's own page, and a run can be stopped like any other run. Each run can look into something different without changing the task. While it runs you watch each step arrive; when it ends you read the report, the raw search results and the full conversation with the model. These files stay on this computer, readable only by your user, and you can delete a run when you no longer need it.

## Who can use it

The console listens only on this machine. Its API also needs a key, which comes in the link the console prints when it starts. That keeps a web page, or an agent with a shell on the same machine, from raising its own budget through the console. Anyone who can read your files can still find the key, as they could reach the database itself, so treat the machine as the boundary.
