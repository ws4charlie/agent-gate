# Console

[中文](zh/console.md)

The console is a local web page for the person who runs the gateway. It shows what every agent is doing and spending, lets you change their limits, and runs research tasks from templates.

## See every agent at a glance

Each agent gets a card: what it spent today against its daily budget, how its spend splits between models, tools and payments, how many calls it made, and how many of those were refused, failed or blocked. A small chart shows its spend hour by hour. Above the cards, a few totals and the wallet's USDC balance, read from the chain.

The events page lists everything that was not a plain success, newest first, with the rule that refused it.

## Change limits without the command line

From a card you can set an agent's budgets and allowlists, or kill it and bring it back. The change applies to the agent's next request. There is no separate copy of the rules: the console writes the same policy the gateway already enforces.

## Run tasks from templates

A template is a reusable task, a bit like a skill: a system prompt, one model, the tools it may use, and two budgets, one per run and one per day. A few public research templates ship with it, and you can write your own or copy and edit a built-in one.

Each template runs as its own agent. Every model call, search and payment goes through the gateway with that agent's key, so the budgets are ordinary policy, the spend shows on its own card, and a run can be stopped like any other run. While a task runs you watch each step arrive; when it ends you read the report, the raw tool results and the full transcript.

## Who can use it

The console listens only on this machine. Its API also needs a key, which comes in the link the console prints when it starts. That keeps a web page, or an agent with a shell on the same machine, from raising its own budget through the console. Anyone who can read your files can still find the key, as they could reach the database itself, so treat the machine as the boundary.
