#!/usr/bin/env bash
# Phase 1 demo: gateway + ledger + audit against a fake upstream. No network, no keys.
# Needs: go, curl, and a local Postgres reachable with createdb/dropdb
# (default: socket in /tmp; override with PGHOST).
set -euo pipefail
cd "$(dirname "$0")/.."

export PGHOST="${PGHOST:-/tmp}"
DB="agentgate_demo_$$"
WORK="$(mktemp -d)"
PORT="${DEMO_PORT:-18080}"
GW="http://127.0.0.1:$PORT"
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  dropdb --if-exists --force "$DB" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

say "build"
go build -o "$WORK/agentgate" ./cmd/agentgate
go build -o "$WORK/fakeupstream" ./cmd/fakeupstream
createdb "$DB"
export DATABASE_URL="postgres:///$DB?host=$PGHOST"
export AG_LISTEN="127.0.0.1:$PORT"
AG="$WORK/agentgate"

say "start the fake upstream"
"$WORK/fakeupstream" -chunk-delay 30ms > "$WORK/fake.out" &
PIDS+=($!)
for _ in $(seq 50); do [ -s "$WORK/fake.out" ] && break; sleep 0.1; done
FAKE="$(head -n1 "$WORK/fake.out")"
echo "fake upstream at $FAKE"

cat > "$WORK/providers.json" <<JSON
{
  "providers": {
    "fake":   {"kind": "openai-compatible", "base_url": "$FAKE/v1"},
    "fakean": {"kind": "anthropic",         "base_url": "$FAKE/v1"}
  },
  "routes": [
    {"prefix": "fake/",   "provider": "fake",   "strip": true},
    {"prefix": "fakean/", "provider": "fakean", "strip": true}
  ]
}
JSON
# Nanos per 1M tokens: $1 in, $2 out, $0.10 cache read, $1.25 cache write.
row='{"input": 1000000000, "output": 2000000000, "cache_read": 100000000, "cache_write": 1250000000}'
cat > "$WORK/prices.json" <<JSON
{"models": {"chat": $row, "chat-die": $row, "chat-cache": $row, "chat-nousage": $row}}
JSON
export AG_PROVIDERS="$WORK/providers.json" AG_PRICES="$WORK/prices.json"
export FAKE_API_KEY=demo-upstream-key FAKEAN_API_KEY=demo-upstream-key

say "migrate, create an account, fund it with \$1, mint a key"
"$AG" migrate
ACCT="$("$AG" account create --name demo)"
"$AG" account deposit --account "$ACCT" --amount 1
KEY="$("$AG" key create --account "$ACCT" --label demo 2>/dev/null)"
echo "client key: ${KEY:0:7}... (shown once; only its SHA-256 is stored)"

say "start the gateway"
"$AG" serve > "$WORK/gw.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -sf "$GW/healthz" >/dev/null 2>&1 && break; sleep 0.1; done

call() { # call <path> <idempotency-key> <body>
  curl -sS -N -i -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    ${2:+-H "Idempotency-Key: $2"} -d "$3" "$GW$1" || true
}
chat='{"model":"fake/chat","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}'

say "1. streamed chat completion (events arrive as the fake emits them)"
call /v1/chat/completions k-1 "$chat" | grep -E '^(HTTP|data:)' | cut -c1-110
"$AG" account balance --account "$ACCT"

say "2. the same Idempotency-Key again: 409, the upstream is not called"
call /v1/chat/completions k-1 "$chat" | grep -E '^(HTTP|\{)' | cut -c1-200

say "3. native Anthropic Messages, with cache tokens priced separately"
call /v1/messages k-3 '{"model":"fakean/chat-cache","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}' | grep -E '^(HTTP|\{)' | cut -c1-160

say "4. upstream dies mid-stream: partial usage is charged, the rest of the hold is released"
call /v1/chat/completions k-4 '{"model":"fake/chat-die","stream":true,"max_tokens":4000,"messages":[{"role":"user","content":"hi"}]}' | grep -E '^(HTTP|data:)' | cut -c1-110
sleep 0.3
"$AG" account balance --account "$ACCT"

say "5. a stream with no usage: charged from a token estimate, flagged"
call /v1/chat/completions k-5 '{"model":"fake/chat-nousage","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}' >/dev/null
sleep 0.3

say "6. report"
"$AG" report --since 1h --by model
echo
"$AG" report --since 1h --by account

say "7. audit log (every outcome, refusals included)"
psql -h "$PGHOST" -d "$DB" -c "select status, model, cost_source, cost_nanos, input_tokens, output_tokens, cache_read_tokens, err from audit_events order by id"
