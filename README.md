# AgentGate

A custody-grade gateway for AI agents: every model call and every payment is authorized, metered, limited and auditable.

## Status

Early work. Foundations only: Go module, money type, ledger types and schema, migration runner, test database helper, config, telemetry, CI. The ledger logic, gateway and policy layers land in later pull requests.

## Build and test

Requires Go and a reachable Postgres (17 recommended).

```bash
go build -o bin/agentgate ./cmd/agentgate
bin/agentgate version

createdb agentgate_test
export DATABASE_URL='postgres:///agentgate_test?host=/tmp'
gofmt -l . && go vet ./... && AG_REQUIRE_DB=1 go test -race -count=1 ./...
```

Without `DATABASE_URL`, database tests skip; with `AG_REQUIRE_DB=1` they fail instead. `agentgate migrate` applies the SQL files in `migrations/`.

Copy `.env.example` to `.env` for local secrets (gitignored) and `configs/agentgate.example.json` for config.

## Learn

- [Foundations](docs/learn/foundations.md)

## License

MIT
