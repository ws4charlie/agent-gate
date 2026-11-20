# AI Registry

[中文](zh/registry.md)

The registry is one list of every model, tool and agent that is allowed to exist. Each entry has an owner, a purpose and a status. It is the inventory an auditor asks for first: what AI do you run, who owns it, and why.

## What an entry holds

An entry names one thing exactly: a model by its name, a tool as `server/tool`, an agent by its id. Names are never patterns, so "is it listed?" is a plain yes or no. The owner is a person or a team. The purpose is one plain sentence. Notes are free text.

Two more fields describe it. The **risk tier** is `low`, `medium` (the default) or `high`. The **version** is free text, such as a model snapshot or a tool release.

## Statuses

- **proposed**: someone asked for it; nobody has approved it yet. This is the default.
- **approved**: it may be used. This is the only status that passes the check below.
- **deprecated**: it should be replaced. Its owner is expected to move off it.
- **blocked**: it must not be used.

## The check

The registry governs when the gateway starts with `--registry-enforce` (or `AG_REGISTRY_ENFORCE=true`). Then every model call, tool call and paid tool call is refused with the rule `registry_unapproved` unless that model or tool has an entry with status `approved`. No entry counts as not approved. The tool list hides tools that are not approved, so an agent does not see what it cannot use.

The check comes right after the agent's own allowlist. A model that is not on the allowlist is still refused as `scope_model` or `scope_tool`; one that is on it but not approved is `registry_unapproved`. Cedar and the built-in rules apply the same check and agree. If the registry cannot be read, the request is refused.

The flag is off by default. Off means no lookups and no change in behaviour.

## Getting something approved

An entry is approved on the command line, and every change is recorded.

```bash
agentgate registry add model gpt-4o --owner ml-team --purpose "support chat" --risk medium --version 2026-05
agentgate registry status model gpt-4o approved
agentgate registry history model gpt-4o
```

`registry history` lists every creation, status change and removal with who did it and when; without a name it lists everything. The actor is your OS user, or `--actor`.

`agentgate registry seed` adds every model in the price table, and every exact model and tool name on an active agent's allowlist, that has no entry yet, as `approved` with the purpose "seeded from live config". It never changes an existing entry, and it is safe to run twice. Wildcards such as `claude-*` are not seeded; it lists them. `--dry-run` shows what it would add. Run it before turning the flag on, so nothing in use is cut off.

## What it shows

The registry also compares the list with what the gateway has really seen. Two views matter. The first is what is in use but not listed at all, with call counts and the last time it was seen. The second is what is listed as deprecated or blocked but is still being used. Both look at the last 30 days by default.

The console's Registry tab shows the list and these two views, read-only. `agentgate registry gaps` prints the same views.
