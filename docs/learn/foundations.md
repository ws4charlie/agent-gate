# Foundations: money, migrations, test database

## Money unit

`money.Nanos` is an int64 count of 1e-9 USD. Floats are never used for money: 0.1 + 0.2 != 0.3 would make a ledger drift. One micro-USDC (6 decimals) is 1000 Nanos, so on-chain amounts convert exactly.

- `FromUSD("1.5")` parses decimal text digit by digit, with no float. More than 9 decimals, signs, exponents and overflow are errors, never silent rounding.
- A price is Nanos per 1M tokens. `Cost(tokens, price)` = `ceil(tokens * price / 1_000_000)`, computed in `math/big`, so the multiply cannot overflow silently. Rounding up means we never undercharge.
- Code: [money.go](../../internal/money/money.go); proof: table tests plus a `rapid` property that the result is the exact ceiling.

## Migrations

- SQL files `migrations/NNNN_name.sql`, embedded in the binary ([embed.go](../../migrations/embed.go)). No external tool.
- [db.Migrate](../../internal/db/db.go) takes an advisory lock, applies each unapplied file in its own transaction, and records it in `schema_migrations`. A failed file leaves no trace.
- Never edit a merged migration; add a new number.
- [0001_ledger.sql](../../migrations/0001_ledger.sql) puts invariants in the database: CHECKs for I1 (`0 <= held <= balance`), UNIQUEs for I4, and a trigger that makes `ledger_entries` append-only.

## Test database

[dbtest.New(t)](../../internal/db/dbtest/dbtest.go) creates a randomly named database, migrates it, and drops it on cleanup, so tests run in parallel without sharing state. No `DATABASE_URL`: skip. With `AG_REQUIRE_DB=1` (CI): fail, so a missing database can never look like a green run.

## Quiz

<details><summary>Questions</summary>

1. Why is money an int64 of nanos instead of a float64?
2. How many Nanos is one micro-USDC?
3. Why does `Cost` round up?
4. What stops two processes running migrations at once?
5. Why should a CI run set `AG_REQUIRE_DB=1`?

Answers:
1. Floats cannot represent most decimals exactly; errors accumulate and balances stop adding up.
2. 1000.
3. So the ledger never charges less than the true cost; rounding down would leak money on every call.
4. A Postgres advisory lock taken for the duration of `Migrate`.
5. Without it, an unreachable database skips tests and CI passes without testing the ledger.

</details>
