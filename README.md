# AgentGate

A gateway between AI agents and everything that costs money. Every model call, tool call and payment is authorized, metered, limited and logged.

An agent that can spend money can also be hijacked by text in a tool result, so a bare API key with a budget is not enough. AgentGate keeps the keys, gives each agent a scoped identity, and checks every spend against a ledger and a policy before it happens.

Two rules carry the design:

- **Reserve before you spend.** The worst-case cost is held first, then settled to the real cost or released.
- **Screen before you sign.** A payment refused by policy never reaches the wallet's key.

## How it works

![Every request is authenticated, checked by policy, held, executed, settled and audited. Refusals, failures and crashes all end with the funds settled or returned.](docs/images/request-flow.png)

It is one Go process and one Postgres database. Operators use a CLI that writes to the database, so an agent has no HTTP route to approve its own requests.

| Area | What it does |
|---|---|
| [Gateway](docs/reference.md#gateway) | OpenAI-compatible and native Anthropic endpoints, with streaming |
| [Ledger](docs/reference.md#ledger) | Holds, settlement and recovery after a crash |
| [Policy](docs/reference.md#policy) | Scopes, budgets, velocity limits, human approval, kill switch |
| [MCP gateway](docs/reference.md#mcp-gateway) | Tool access control and tool output screening |
| [Payments](docs/reference.md#payments) | x402 payments in USDC, with the chain as the source of truth |
| [Runs](docs/reference.md#runs) | Groups the steps of one job so you can inspect or kill them together |
| [SDK](docs/reference.md#sdk) | A thin Python client |

## Try it

You need Go and a running Postgres. This demo uses a fake model provider and a throwaway database, so it needs no keys and no network.

```bash
demo/phase1.sh
```

To run the tests:

```bash
createdb agentgate_test
export DATABASE_URL='postgres:///agentgate_test?host=/tmp'
AG_REQUIRE_DB=1 go test -race -count=1 ./...
```

## Status

| Verified | How |
|---|---|
| Ledger, gateway, policy, MCP gateway, payments | Tests in CI, with no secrets |
| Payments on a local chain | Local anvil node with a mock USDC, in CI |
| OpenRouter | One small live run of 8 requests |

**Not done:** Base Sepolia payments and real-traffic dogfooding. This is testnet-only software for a single gateway process. See [Limits](docs/reference.md#limits).

## Learn more

- [Reference](docs/reference.md): every feature, measured number and limit
- [Study guides](docs/learn/foundations.md): how each part works, with quiz questions

## License

MIT
