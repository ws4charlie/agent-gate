#!/usr/bin/env bash
# Phase 3 demo: an agent pays for MCP tool calls through the gateway wallet, and
# the report shows model spend and tool payments side by side.
# Offline: a simulated EVM chain runs inside the paidtool process. Four steps:
#   1. paid call           the tool answers 402; policy allows; the wallet signs; the call is retried
#   2. over policy         $0.50 against a $0.30 daily budget: refused BEFORE signing, nothing on chain
#   3. over the threshold  a human has to approve; the payment is queued, then approved, then paid once
#   4. report --by agent   model spend and tool payments side by side, reconciled with the chain
#
#   demo/phase3.sh                         # offline, simulated chain (default)
#   demo/phase3.sh --network base-sepolia  # LIVE testnet run; see below
#
# base-sepolia needs, from the environment (nothing is read from files in the repo):
#   AG_RPC_URL           a Base Sepolia JSON-RPC URL
#   AG_FACILITATOR_KEY   hex key of an account holding Base Sepolia ETH (pays gas)
#   AG_WALLET_KEY        hex key of the agent wallet, holding Base Sepolia USDC
#   AG_PAYEE             address that receives the USDC
# The hands never run this mode; the lead does, with the human's say-so.
# Every step checks its own outcome; the script exits non-zero if one is wrong.
set -euo pipefail
cd "$(dirname "$0")/.."

NETWORK=sim
while [ $# -gt 0 ]; do
  case "$1" in
    --network) NETWORK="${2:?--network needs sim or base-sepolia}"; shift 2 ;;
    *) echo "usage: $0 [--network sim|base-sepolia]" >&2; exit 2 ;;
  esac
done
case "$NETWORK" in sim|base-sepolia) ;; *) echo "unknown network $NETWORK" >&2; exit 2 ;; esac
if [ "$NETWORK" = base-sepolia ]; then
  for v in AG_RPC_URL AG_FACILITATOR_KEY AG_WALLET_KEY AG_PAYEE; do
    [ -n "${!v:-}" ] || { echo "base-sepolia needs \$$v (see the header of this script)" >&2; exit 2; }
  done
fi

export PGHOST="${PGHOST:-/tmp}"
DB="agentgate_demo3_$$"
WORK="$(mktemp -d)"
PORT="${DEMO_PORT:-18083}"
TOOLPORT="${DEMO_TOOL_PORT:-19403}"
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
fail() { echo "FAIL: $*" >&2; exit 1; }
expect() { # expect <what> <needle> <haystack>
  case "$3" in *"$2"*) echo "   ok: $1" ;; *) echo "   output was: $3" >&2; fail "$1 (wanted: $2)" ;; esac
}

say "build"
go build -o "$WORK/agentgate" ./cmd/agentgate
go build -o "$WORK/fakeupstream" ./cmd/fakeupstream
AG="$WORK/agentgate"
createdb "$DB"
export DATABASE_URL="postgres:///$DB?host=$PGHOST"
export AG_LISTEN="127.0.0.1:$PORT"

# A fake model provider, so the report has model spend to put next to tool payments.
"$WORK/fakeupstream" > "$WORK/fake.out" &
PIDS+=($!)
for _ in $(seq 50); do [ -s "$WORK/fake.out" ] && break; sleep 0.1; done
FAKE="$(head -n1 "$WORK/fake.out")"
cat > "$WORK/providers.json" <<JSON
{"providers":{"fake":{"kind":"openai-compatible","base_url":"$FAKE/v1"}},"routes":[{"prefix":"fake/","provider":"fake","strip":true}]}
JSON
# $1 in, $2 out per 1M tokens: a call costs fractions of a cent.
cat > "$WORK/prices.json" <<'JSON'
{"models":{"chat":{"input":1000000000,"output":2000000000,"cache_read":100000000,"cache_write":1250000000}}}
JSON
export AG_PROVIDERS="$WORK/providers.json" AG_PRICES="$WORK/prices.json" FAKE_API_KEY=demo-upstream-key

say "ledger: account with \$1"
"$AG" migrate
ACCT="$("$AG" account create --name agent-demo)"
"$AG" account deposit --account "$ACCT" --amount 1

say "wallet and paid tool"
if [ "$NETWORK" = sim ]; then
  export AG_WALLET_KEY_FILE="$WORK/wallet.key"
  WALLET="$("$AG" wallet new --file "$AG_WALLET_KEY_FILE")"
  PAYEE=0x70997970C51812dc3A010C7d01b50e0d17dc79C8   # a public test address, not a secret
  echo "wallet address (key stays in a 0600 file): $WALLET"
  "$AG" paidtool --network sim --listen "127.0.0.1:$TOOLPORT" --pay-to "$PAYEE" --fund 5 --fund-address "$WALLET" > "$WORK/tool.log" 2>&1 &
else
  PAYEE="$AG_PAYEE"
  "$AG" paidtool --network base-sepolia --listen "127.0.0.1:$TOOLPORT" --pay-to "$PAYEE" > "$WORK/tool.log" 2>&1 &
fi
PIDS+=($!)
for _ in $(seq 100); do curl -sf "http://127.0.0.1:$TOOLPORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
curl -sf "http://127.0.0.1:$TOOLPORT/healthz" >/dev/null || { cat "$WORK/tool.log"; exit 1; }
head -n1 "$WORK/tool.log"

# Two MCP servers, both behind x402. Their tools have no list price: the 402 is the price.
cat > "$WORK/mcp.json" <<JSON
{"servers":{"echo":{"url":"http://127.0.0.1:$TOOLPORT/mcp/echo"},"premium":{"url":"http://127.0.0.1:$TOOLPORT/mcp/premium"}},"allowed_origins":[]}
JSON
# max_tx_usd is the wallet's own per-transaction cap, checked inside the wallet
# whatever any agent's policy says.
cat > "$WORK/payments.json" <<JSON
{"network": "$NETWORK", "chain_query_url": "http://127.0.0.1:$TOOLPORT", "max_tx_usd": "1"}
JSON

# agent-1: may use both tools, pay only PAYEE, spend $0.30 a UTC day.
# agent-2: same, but any single payment above half a cent needs a human.
TOOLS="echo/lookup,premium/lookup"
KEY1="$("$AG" agent create --account "$ACCT" --name agent-1 --models '*' --tools "$TOOLS" --payees "$PAYEE" --daily 0.30 2>/dev/null)"
KEY2="$("$AG" agent create --account "$ACCT" --name agent-2 --models '*' --tools "$TOOLS" --payees "$PAYEE" --approval-over 0.005 2>/dev/null)"

say "gateway"
"$AG" serve --payments "$WORK/payments.json" --mcp "$WORK/mcp.json" > "$WORK/gw.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do curl -sf "$GW/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
curl -sf "$GW/healthz" >/dev/null || { cat "$WORK/gw.log"; exit 1; }

ID=0
META='"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}'
call() { # call <key> <server> <idempotency-key>: a tools/call; prints the HTTP code and the JSON-RPC body
  ID=$((ID + 1))
  curl -sS -m 90 -w ' [HTTP %{http_code}]' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2026-07-28' -H 'Mcp-Method: tools/call' -H 'Mcp-Name: lookup' \
    -H "Idempotency-Key: $3" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":$ID,\"method\":\"tools/call\",\"params\":{\"name\":\"lookup\",\"arguments\":{\"text\":\"hello\"},$META}}" "$GW/mcp/$2"
}

say "model call, so the report has model spend too"
curl -sS -H "Authorization: Bearer $KEY1" -H 'Content-Type: application/json' \
  -d '{"model":"fake/chat","messages":[{"role":"user","content":"hi"}],"max_tokens":20}' "$GW/v1/chat/completions" | head -c 200; echo

say "0. what the agent can see before it spends: GET /v1/balance"
curl -sS -H "Authorization: Bearer $KEY1" "$GW/v1/balance"

say "1. agent-1 calls echo/lookup: the tool says 402, policy allows, the wallet signs, the call is retried"
r="$(call "$KEY1" echo demo-1)"; echo "   $r"
expect "the agent got the tool result" 'HELLO' "$r"
"$AG" account balance --account "$ACCT"

say "2. agent-1 calls premium/lookup (\$0.50): over its \$0.30 daily budget -> refused before signing"
r="$(call "$KEY1" premium demo-2)"; echo "   $r"
expect "refused by the budget rule" 'budget_daily' "$r"

say "3. agent-2 calls echo/lookup (\$0.01): over its approval threshold -> queued, a human approves, it is paid once"
r="$(call "$KEY2" echo demo-3)"; echo "   first try: $r"
expect "the payment waits for a human" 'approval_required' "$r"
"$AG" approve list
APID="$("$AG" approve list | awk 'NR==2 {print $1}')"
"$AG" approve allow "$APID" --by demo-operator
r="$(call "$KEY2" echo demo-3)"; echo "   replay: $r"
expect "the replay is paid and runs" 'HELLO' "$r"
r="$(call "$KEY2" echo demo-3)"; echo "   replay again: $r"
expect "a second replay is a duplicate" 'duplicate_request' "$r"

say "ledger rows (hold -> settle for each payment; hold -> release for the refusal)"
psql -h "$PGHOST" -d "$DB" -c "select e.kind, h.kind as hold_kind, e.amount_nanos, e.balance_after, e.held_after from ledger_entries e left join holds h on h.id = e.hold_id order by e.id"

say "4. report --by agent: model spend and tool payments side by side, reconciled with the chain"
OUT="$("$AG" report --since 1h --by agent --payments "$WORK/payments.json")"
echo "$OUT"
expect "agent-1 and agent-2 both listed" 'agent-2' "$OUT"
expect "no ledger/chain mismatch" ', 0 findings' "$OUT"
expect "both payments matched the chain" '2 matched' "$OUT"

say "reconcile: nothing is pending, so nothing changes"
"$AG" reconcile --payments "$WORK/payments.json"
say "phase 3 demo OK"
