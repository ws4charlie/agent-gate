#!/usr/bin/env bash
# The five-minute demo: the Phase 2 acceptance scenario, end to end, offline.
# Scripted Python agents (no LLM) drive a real gateway on fakes; nothing leaves the machine.
#
#   1. burns budget            a runaway agent calls until budget_daily says no
#   2. trips velocity          three calls at once against a per-minute limit
#   3. over-limit call queued  a payment above the threshold waits; a human approves; it runs once
#   4. killed mid-run          the operator kills a streaming agent; it is cut within a second
#   5. out-of-scope tool       the tool is refused and the upstream never sees it; an injected result is blocked
#
# Every step checks its own outcome; the script exits non-zero if one is wrong.
# DEMO_PAUSE=5 waits that many seconds between steps for live narration (about 5 minutes in all).
# Needs: go, curl, Python 3.11+ with httpx (AG_PYTHON, default python3), a local Postgres reachable
# with createdb/dropdb/psql (PGHOST, default /tmp). CI-safe: nothing is downloaded at run time.
set -euo pipefail
cd "$(dirname "$0")/.."

PY="${AG_PYTHON:-python3}"
"$PY" -c 'import sys, httpx; assert sys.version_info >= (3, 11)' 2>/dev/null \
  || { echo "need Python 3.11+ with httpx: set AG_PYTHON, or: pip install ./sdk/python" >&2; exit 2; }
export PYTHONPATH="$PWD/sdk/python:$PWD/demo/agents${PYTHONPATH:+:$PYTHONPATH}" PYTHONDONTWRITEBYTECODE=1

export PGHOST="${PGHOST:-/tmp}"
DB="agentgate_demo_$$"
WORK="$(mktemp -d)"
PORT="${DEMO_PORT:-18090}"
TOOLPORT="${DEMO_TOOL_PORT:-19490}"
export AG_URL="http://127.0.0.1:$PORT"
PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  wait 2>/dev/null || true
  dropdb --if-exists --force "$DB" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; sleep "${DEMO_PAUSE:-0}"; }
fail() { echo "DEMO FAILED: $*" >&2; exit 1; }
expect() { [ "$2" = "$3" ] || fail "$1: got '$3', want '$2'"; echo "   ok: $1 = $3"; }
result() { sed -n 's/^RESULT //p' | tail -n1; }          # the agent's last RESULT line
field() { tr ' ' '\n' | sed -n "s/^$1=//p"; }            # field <name> < "k=v k=v"

say "build, database, fakes"
go build -o "$WORK/agentgate" ./cmd/agentgate
go build -o "$WORK/fakeupstream" ./cmd/fakeupstream
go build -o "$WORK/fakemcp" ./cmd/fakemcp
AG="$WORK/agentgate"
createdb "$DB"
export DATABASE_URL="postgres:///$DB?host=$PGHOST" AG_LISTEN="127.0.0.1:$PORT"
export FAKE_API_KEY=demo-upstream-key UPSTREAM_MCP_AUTH="Bearer demo-mcp-credential"
export AG_WALLET_KEY_FILE="$WORK/wallet.key"

# Model upstream: calls take 300 ms and streams are long (100 events, 100 ms apart), so overlap and kill are visible.
"$WORK/fakeupstream" -latency 300ms -chunk-delay 100ms -chunks 100 > "$WORK/fake.out" &
PIDS+=($!)
"$WORK/fakemcp" -key "$UPSTREAM_MCP_AUTH" > "$WORK/mcp.out" &
PIDS+=($!)
for _ in $(seq 50); do [ -s "$WORK/fake.out" ] && [ -s "$WORK/mcp.out" ] && break; sleep 0.1; done
FAKE="$(head -n1 "$WORK/fake.out")"; MCPURL="$(head -n1 "$WORK/mcp.out")"
cat > "$WORK/providers.json" <<JSON
{"providers": {"fake": {"kind": "openai-compatible", "base_url": "$FAKE/v1"}},
 "routes": [{"prefix": "fake/", "provider": "fake", "strip": true}]}
JSON
# A dear model ($1000 / $2000 per 1M tokens): a call really costs ~$0.02 and the worst case it reserves is ~$0.23.
row='{"input": 1000000000000, "output": 2000000000000, "cache_read": 0, "cache_write": 1000000000000}'
echo "{\"models\": {\"chat\": $row}}" > "$WORK/prices.json"
echo "{\"servers\": {\"up\": {\"url\": \"$MCPURL\", \"auth_env\": \"UPSTREAM_MCP_AUTH\"}}}" > "$WORK/mcp.json"
export AG_PROVIDERS="$WORK/providers.json" AG_PRICES="$WORK/prices.json"

# A simulated chain with a paid tool; the wallet key stays in a 0600 file.
WALLET="$("$AG" wallet new --file "$AG_WALLET_KEY_FILE")"
PAYEE=0x70997970C51812dc3A010C7d01b50e0d17dc79C8   # a public test address, not a secret
"$AG" paidtool --network sim --listen "127.0.0.1:$TOOLPORT" --pay-to "$PAYEE" --fund 5 --fund-address "$WALLET" > "$WORK/tool.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do curl -sf "http://127.0.0.1:$TOOLPORT/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
cat > "$WORK/payments.json" <<JSON
{"network": "sim", "chain_query_url": "http://127.0.0.1:$TOOLPORT",
 "tools": {"echo": "http://127.0.0.1:$TOOLPORT/echo", "premium": "http://127.0.0.1:$TOOLPORT/premium"}}
JSON

"$AG" migrate >/dev/null
ACCT="$("$AG" account create --name demo)"
"$AG" account deposit --account "$ACCT" --amount 20 >/dev/null
"$AG" serve --mcp "$WORK/mcp.json" --payments "$WORK/payments.json" --reconcile-every 1s > "$WORK/gw.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 100); do curl -sf "$AG_URL/healthz" >/dev/null 2>&1 && break; sleep 0.1; done
curl -sf "$AG_URL/healthz" >/dev/null || { cat "$WORK/gw.log"; fail "the gateway did not start"; }
mkkey() { "$AG" agent create --account "$ACCT" --name "$1" "${@:2}" 2>/dev/null; }
agent() { local key="$1"; shift; AG_KEY="$key" "$PY" "demo/agents/$@"; }   # agent <key> <script> [args]

say "1. burns budget: a runaway agent, \$0.30 a day"
KEY="$(mkkey runaway --models fake/chat --daily 0.30)"
OUT="$(agent "$KEY" runaway.py burn | tee /dev/stderr)" 2>&1 || fail "runaway burn"
R="$(printf '%s\n' "$OUT" | result)"
expect "the daily budget stops it" "budget_daily" "$(printf '%s' "$R" | field rule)"

say "2. trips velocity: \$0.50 a minute, three calls at once (each reserves ~\$0.23)"
KEY="$(mkkey sprinter --models fake/chat --velocity 0.50)"
OUT="$(agent "$KEY" runaway.py velocity | tee /dev/stderr)"
R="$(printf '%s\n' "$OUT" | result)"
[ "$(printf '%s' "$R" | field velocity_refusals)" -ge 1 ] || fail "velocity never tripped: $R"
echo "   ok: velocity refused $(printf '%s' "$R" | field velocity_refusals) of 3"

say "3. over-limit call queued: a \$0.01 payment above a \$0.005 threshold waits for a human"
KEY="$(mkkey spender --tools echo,premium --payees "$PAYEE" --daily 0.30 --approval-over 0.005)"
OUT="$(agent "$KEY" paying.py queued big-1 | tee /dev/stderr)"
APPROVAL="$(printf '%s\n' "$OUT" | result | field approval)"
[ -n "$APPROVAL" ] || fail "nothing was queued"
"$AG" approve list
"$AG" approve allow "$APPROVAL" --by demo-operator
OUT="$(agent "$KEY" paying.py replay big-1 | tee /dev/stderr)"
expect "the approved replay runs once and pays" "yes" "$(printf '%s\n' "$OUT" | result | field tx)"
KEY2="$(mkkey saver --tools echo,premium --payees "$PAYEE" --daily 0.30)"
OUT="$(agent "$KEY2" paying.py premium | tee /dev/stderr)"
expect "a \$0.50 payment is refused before signing" "budget_daily" "$(printf '%s\n' "$OUT" | result | field rule)"

say "4. killed mid-run: the operator kills a streaming agent"
KEY="$(mkkey streamer --models fake/chat)"
( agent "$KEY" runaway.py stream > "$WORK/stream.out" 2>&1 ) &
STREAMER=$!
for _ in $(seq 100); do grep -q STREAMING "$WORK/stream.out" 2>/dev/null && break; sleep 0.1; done
grep -q STREAMING "$WORK/stream.out" || fail "the stream never started"
sleep 1.5
START=$SECONDS
"$AG" kill --agent streamer
wait "$STREAMER" || true
cat "$WORK/stream.out"
echo "   the agent was gone $((SECONDS - START)) s after the kill"
[ $((SECONDS - START)) -le 3 ] || fail "the stream was not cut quickly"
R="$(result < "$WORK/stream.out")"
expect "the stream did not finish" "False" "$(printf '%s' "$R" | field finished)"
expect "the next request is refused" "agent_killed" "$(printf '%s' "$R" | field rule)"

say "5. out-of-scope tool refused; an injected tool result blocked"
KEY="$(mkkey researcher --tools up/echo,up/inject)"
OUT="$(agent "$KEY" injected.py | tee /dev/stderr)"
R="$(printf '%s\n' "$OUT" | result)"
expect "fail is outside the tool scope" "scope_tool" "$(printf '%s' "$R" | field scope)"
expect "the injected result is blocked" "instruction_override" "$(printf '%s' "$R" | field screen)"
grep -q 'Ignore all previous' "$WORK/gw.log" && fail "blocked content reached the gateway log"

say "the books"
sleep 1
"$AG" account balance --account "$ACCT"
expect "no active holds" "0" "$(psql -XAt -h "$PGHOST" "$DB" -c "select count(*) from holds where status='active'")"
psql -X -h "$PGHOST" "$DB" -c "select a.name as agent, d.rule, d.decision, count(*) from policy_decisions d join agents a on a.id=d.agent_id group by 1,2,3 order by 1,2"
printf '\ndemo OK: budget, velocity, approval, kill and scope all held\n'
