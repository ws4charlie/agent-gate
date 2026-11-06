# AgentGate

English | [中文](README.zh-CN.md)

A gateway between AI agents and everything that costs money. Every model call, tool call and payment is authorized, metered, limited and logged.

An agent that can spend money can also be hijacked by text in a tool result, so a bare API key with a budget is not enough. AgentGate keeps the keys, gives each agent a scoped identity, and checks every spend against a ledger and a policy before it happens.

Two rules carry the design:

- **Reserve before you spend.** The worst-case cost is held first, then settled to the real cost or released.
- **Screen before you sign.** A payment refused by policy never reaches the wallet's key.

## Architecture

<p>
<a href="docs/sdk.md"><img src="docs/images/arch-agent.png" width="66.66%" alt="Agent"></a><a href="docs/policy.md"><img src="docs/images/arch-operator.png" width="33.33%" alt="Operator"></a><br>
<img src="docs/images/arch-down.png" width="100%" alt=""><br>
<a href="docs/gateway.md"><img src="docs/images/arch-gateway.png" width="66.66%" alt="Gateway"></a><a href="docs/ledger.md"><img src="docs/images/arch-postgres.png" width="33.33%" alt="Postgres"></a><br>
<img src="docs/images/arch-split.png" width="100%" alt=""><br>
<a href="docs/policy.md"><img src="docs/images/arch-policy.png" width="33.33%" alt="Policy"></a><a href="docs/ledger.md"><img src="docs/images/arch-ledger.png" width="33.33%" alt="Ledger"></a><a href="docs/runs.md"><img src="docs/images/arch-runs.png" width="33.33%" alt="Runs"></a><br>
<img src="docs/images/arch-merge.png" width="100%" alt=""><br>
<a href="docs/gateway.md"><img src="docs/images/arch-models.png" width="33.33%" alt="Models"></a><a href="docs/tools.md"><img src="docs/images/arch-tools.png" width="33.33%" alt="Tools"></a><a href="docs/payments.md"><img src="docs/images/arch-payments.png" width="33.33%" alt="Payments"></a><br>
<img src="docs/images/arch-out.png" width="100%" alt=""><br>
<a href="docs/gateway.md"><img src="docs/images/arch-providers.png" width="33.33%" alt="Model providers"></a><a href="docs/tools.md"><img src="docs/images/arch-mcp.png" width="33.33%" alt="MCP tools"></a><a href="docs/payments.md"><img src="docs/images/arch-chain.png" width="33.33%" alt="Blockchain"></a>
</p>

Click a box to open its page.

## How it works

![Every request is authenticated, checked by policy, held, executed, settled and audited. Refusals, failures and crashes all end with the funds settled or returned.](docs/images/request-flow.png)

It is one Go process and one Postgres database. Operators use a CLI that writes to the database, so an agent has no HTTP route to approve its own requests.

Read more about each part. Each page takes a minute or two:

| Area | What it does |
|---|---|
| [Gateway](docs/gateway.md) | Sits between agents and model providers |
| [Ledger](docs/ledger.md) | Holds the money first, then charges the real cost |
| [Policy](docs/policy.md) | Sets what each agent may do and spend |
| [Tools](docs/tools.md) | Controls tool access and screens what comes back |
| [Payments](docs/payments.md) | Pays for tools in USDC, trusting the chain over the tool |
| [Runs](docs/runs.md) | Groups one job's steps so you can read or stop them together |
| [Python SDK](docs/sdk.md) | A small client for scripts and agents |
| [Testing](docs/testing.md) | How it's tested without spending money |

## Try it

You need Go and a running Postgres. This demo uses a fake model provider and a throwaway database, so it needs no keys and no network.

```bash
demo/phase1.sh
```

## Status

| Verified | How |
|---|---|
| Ledger, gateway, policy, MCP gateway, payments | Tests in CI, with no secrets |
| Payments on a local chain | Local anvil node with a mock USDC, in CI |
| OpenRouter | Early live testing, with more to come |

**Not done:** Base Sepolia payments and real-traffic dogfooding. This is testnet-only software for a single gateway process. See [Limits](docs/limits.md).

## License

MIT
