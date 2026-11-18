# AI Registry

[中文](zh/registry.md)

The registry is one list of every model, tool and agent that is allowed to exist. Each entry has an owner, a purpose and a status. It is the inventory an auditor asks for first: what AI do you run, who owns it, and why.

## What an entry holds

An entry names one thing exactly: a model by its name, a tool as `server/tool`, an agent by its id. Names are never patterns, so "is it listed?" is a plain yes or no. The owner is a person or a team. The purpose is one plain sentence. Notes are free text.

## Statuses

- **proposed**: someone asked for it; nobody has approved it yet. This is the default.
- **approved**: it may be used.
- **deprecated**: it still works, but it should be replaced. Its owner is expected to move off it.
- **blocked**: it must not be used.

## What it shows

The registry also compares the list with what the gateway has really seen. Two views matter. The first is what is in use but not listed at all, with call counts and the last time it was seen. The second is what is listed as deprecated or blocked but is still being used. Both look at the last 30 days by default.

The console's Registry tab shows the list and these two views, read-only. The command line `agentgate registry` adds, changes and removes entries and prints the same gaps.

## What it does not do yet

The registry records and reports. It does not refuse any request: an unlisted model still works, and a blocked tool is still reachable. Enforcement is a later step and would come through the policy layer, not through this list. Until then, "blocked" is a statement of intent that the gaps view lets you check.
