# Console

[中文](zh/console.md)

The console is a local web page for the person who runs the gateway. It shows what every agent is doing and spending, lets you change their limits, and runs research tasks with your own agents.

## See every agent at a glance

The overview shows totals across every agent: what was spent today and over seven days, the wallet's USDC balance read from the chain, how many calls were made in the last day and how many of them were refused, failed or blocked, and which models and tools the money went to.

The agents page gives each agent a short card: today's spend against its daily budget, and any problems. Open an agent to see its own spend, its activity, an hourly chart, and what it is allowed to use.

Every count is a link. Clicking an agent's errors opens the events page already filtered to that agent and to errors, so you go from "something is wrong" to the rows that say what in one click.

## Change limits without the command line

From an agent's page you can rename it, set its budgets and allowlists, or kill it and bring it back. A rename keeps its key working. The change applies to the agent's next request. There is no separate copy of the rules: the console writes the same policy the gateway already enforces.

## Run research tasks with your own agents

A task is a reusable research job: what to look into, the instructions, and which of your agents does it. Three public tasks ship with it. Two are run by GPT Researcher, which is good at reading many sources and summing them up. The one about x402 is run by Claude Code, which can also read code and say what AgentGate should change. You can add your own tasks, edit any task in place (a built-in one can be reset to how it shipped), and drag the list into the order you like.

The console does not do the research itself. It starts the agent's runner, a small program on this machine that holds the agent's key, and gives it a run id. Every model call, search and payment goes through the gateway as that agent and carries the run id, so the agent's own limits apply: its per-task budget caps each run, its daily budget caps the day, and a run can be stopped like any other. The runner gets only the run's details from the console, never the console's own secrets, and Claude Code runs with an empty settings folder of its own, so your everyday Claude Code is untouched.

While a run goes you watch its steps arrive; when it ends you read the report, the full trace and the log. These files stay on this computer, readable only by your user, and you can delete a run when you no longer need it.

## Who can use it

The console listens only on this machine. Its API also needs a key, which comes in the link the console prints when it starts. That keeps a web page, or an agent with a shell on the same machine, from raising its own budget through the console. Anyone who can read your files can still find the key, as they could reach the database itself, so treat the machine as the boundary.
