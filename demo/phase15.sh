#!/usr/bin/env bash
# Phase 1.5 demo: an agent pays for a tool through x402 with screen-before-sign.
# Offline: a simulated EVM chain runs inside the paidtool process. Two payments:
#   1. echo ($0.01): inside policy, signed, settled on chain, charged in the ledger.
#   2. premium ($0.50): over the agent's budget, refused BEFORE signing; the
#      ledger shows a hold and its release, the audit log shows the refusal.
#
#   demo/phase15.sh                         # offline, simulated chain (default)
#   demo/phase15.sh --network base-sepolia  # LIVE testnet run; see below
#
# base-sepolia needs, from the environment (nothing is read from files in the repo):
#   AG_RPC_URL           a Base Sepolia JSON-RPC URL
#   AG_FACILITATOR_KEY   hex key of an account holding Base Sepolia ETH (pays gas)
#   AG_WALLET_KEY        hex key of the agent wallet, holding Base Sepolia USDC
#   AG_PAYEE             address that receives the USDC
# The hands never run this mode; the lead does, with the human's say-so.
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
DB="agentgate_demo15_$$"
WORK="$(mktemp -d)"
PORT="${DEMO_PORT:-18081}"
TOOLPORT="${DEMO_TOOL_PORT:-19402}"
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
AG="$WORK/agentgate"
createdb "$DB"
export DATABASE_URL="postgres:///$DB?host=$PGHOST"
export AG_LISTEN="127.0.0.1:$PORT"
echo '{"providers":{},"routes":[]}' > "$WORK/providers.json"
echo '{"models":{}}' > "$WORK/prices.json"
export AG_PROVIDERS="$WORK/providers.json" AG_PRICES="$WORK/prices.json"

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

# The agent's credential carries its scope: it may use the echo and premium
# tools, pay only PAYEE, and spend at most $0.30 a UTC day. chain_query_url (sim
# only) is how the reconciler asks the simulated chain whether a signed payment executed.
KEY="$("$AG" agent create --account "$ACCT" --name agent-1 --models '*' --tools echo,premium --payees "$PAYEE" --daily 0.30 2>/dev/null)"
cat > "$WORK/payments.json" <<JSON
{
  "network": "$NETWORK",
  "tools": {"echo": "http://127.0.0.1:$TOOLPORT/echo", "premium": "http://127.0.0.1:$TOOLPORT/premium"},
  "chain_query_url": "http://127.0.0.1:$TOOLPORT"
}
JSON
say "gateway"
"$AG" serve --payments "$WORK/payments.json" > "$WORK/gw.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do curl -sf "$GW/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
curl -sf "$GW/healthz" >/dev/null || { cat "$WORK/gw.log"; exit 1; }

pay() { # pay <tool> <idempotency-key>
  curl -sS -w '\nHTTP %{http_code}\n' -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: $2" -d "{\"tool\":\"$1\",\"input\":{\"text\":\"hello\"}}" "$GW/v1/payments"
}

say "1. echo (\$0.01): inside policy -> signed -> settled on chain -> charged"
pay echo demo-1
"$AG" account balance --account "$ACCT"

say "2. premium (\$0.50): over the \$0.30 daily budget -> refused before signing"
pay premium demo-2
"$AG" account balance --account "$ACCT"

say "3. the same Idempotency-Key again: 409, nothing signed, nothing charged"
pay echo demo-1

say "ledger rows (hold -> settle for the payment; hold -> release for the refusal)"
psql -h "$PGHOST" -d "$DB" -c "select e.kind, h.kind as hold_kind, e.amount_nanos, e.balance_after, e.held_after, e.memo from ledger_entries e left join holds h on h.id = e.hold_id order by e.id"

say "audit rows (the refusal is recorded; it has a hold but no cost)"
psql -h "$PGHOST" -d "$DB" -c "select kind, agent_id, model as tool, status, err, cost_nanos, left(upstream_id, 14) as tx from audit_events order by id"


say "reconcile: nothing is pending, so nothing changes (a payment whose outcome is unknown is resolved from the chain here)"
"$AG" reconcile --payments "$WORK/payments.json"
