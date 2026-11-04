#!/usr/bin/env bash
# Phase 2 demo: the MCP tool gateway and runs.
#
#   demo/phase2-mcp.sh --offline   an in-repo fake MCP server; no network, no npm (CI-safe)
#   demo/phase2-mcp.sh             `npx @modelcontextprotocol/server-everything` over Streamable
#                                  HTTP as the upstream. Needs npm access for a free local dev
#                                  server and nothing else; no paid or live calls.
#
# One agent, one run, five things shown:
#   1. tools/list is filtered by the agent's tool allowlist
#   2. a free tool and a priced tool both go through the gate; only the priced one moves money
#   3. an out-of-scope call is a JSON-RPC error and never reaches the upstream
#   4. a tool result carrying an instruction is blocked; only its rule id and hash are logged
#   5. `run show` lists the steps in order with the spend from the ledger; `run kill`
#      cuts an in-flight call within a second and refuses the run's next request
#
# Every step checks its own outcome; the script exits non-zero if one is wrong.
# Needs: go, curl, a local Postgres reachable with createdb/dropdb (PGHOST, default /tmp).
set -euo pipefail
cd "$(dirname "$0")/.."

OFFLINE=0
[ "${1:-}" = "--offline" ] && OFFLINE=1

export PGHOST="${PGHOST:-/tmp}"
DB="agentgate_demo_mcp_$$"
WORK="$(mktemp -d)"
PORT="${DEMO_PORT:-18083}"
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
fail() { echo "DEMO FAILED: $*" >&2; exit 1; }
expect() { [ "$2" = "$3" ] || fail "$1: got '$3', want '$2'"; echo "   ok: $1 = $3"; }

say "build"
go build -o "$WORK/agentgate" ./cmd/agentgate
go build -o "$WORK/fakemcp" ./cmd/fakemcp
createdb "$DB"
export DATABASE_URL="postgres:///$DB?host=$PGHOST"
export AG_LISTEN="127.0.0.1:$PORT"
AG="$WORK/agentgate"

if [ "$OFFLINE" = 1 ]; then
  say "upstream: the in-repo fake MCP server (requires its own credential, which only the gateway holds)"
  export UPSTREAM_MCP_AUTH="Bearer upstream-demo-credential"
  "$WORK/fakemcp" -key "$UPSTREAM_MCP_AUTH" > "$WORK/up.out" &
  PIDS+=($!)
  for _ in $(seq 50); do [ -s "$WORK/up.out" ] && break; sleep 0.1; done
  UP="$(head -n1 "$WORK/up.out")"
  FREE=echo PAID=paid INJECT=inject SLOW=slow OUT=stream
else
  say "upstream: npx @modelcontextprotocol/server-everything (Streamable HTTP, local dev server)"
  command -v npx >/dev/null || fail "npx not found: use --offline"
  (cd "$WORK" && PORT=3001 npx -y @modelcontextprotocol/server-everything streamableHttp > "$WORK/up.log" 2>&1) &
  PIDS+=($!)
  for _ in $(seq 150); do curl -s -o /dev/null "http://127.0.0.1:3001/mcp" && break; sleep 0.2; done
  UP="http://127.0.0.1:3001/mcp"
  export UPSTREAM_MCP_AUTH=""
  FREE=echo PAID=get-sum INJECT=none SLOW=none OUT=none
fi
echo "   upstream at $UP"

cat > "$WORK/mcp.json" <<JSON
{"servers": {"up": {"url": "$UP", "auth_env": "$([ "$OFFLINE" = 1 ] && echo UPSTREAM_MCP_AUTH)", "prices": {"$PAID": 5000000}}}}
JSON

say "migrate; one ledger account with \$5; the gateway (tool output screening is on by default)"
"$AG" migrate
ACCT="$("$AG" account create --name demo)"
"$AG" account deposit --account "$ACCT" --amount 5
# No model providers are needed for tool calls; the files only have to exist.
echo '{"providers": {}, "routes": []}' > "$WORK/providers.json"
echo '{"models": {}}' > "$WORK/prices.json"
export AG_PROVIDERS="$WORK/providers.json" AG_PRICES="$WORK/prices.json"
"$AG" serve --mcp "$WORK/mcp.json" --reconcile-every 1s > "$WORK/gw.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -sf "$GW/healthz" >/dev/null 2>&1 && break; sleep 0.1; done

# This agent may use the free and the priced tool, and the two the offline demo uses for screening and kill.
TOOLS="up/$FREE,up/$PAID"
[ "$OFFLINE" = 1 ] && TOOLS="$TOOLS,up/$INJECT,up/$SLOW"
KEY="$("$AG" agent create --account "$ACCT" --name researcher --tools "$TOOLS" 2>/dev/null)"

# rpc <method> <params-json-without-_meta> [extra curl args...]  -> sets HTTP_CODE and BODY (not in a subshell)
META='"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}'
ID=0
rpc() {
  local method="$1" params="$2"; shift 2
  local name=""; name="$(printf '%s' "$params" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p')"
  ID=$((ID + 1))
  curl -sS -m 30 -D "$WORK/hdr" -o "$WORK/body" -w '%{http_code}' -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2026-07-28' -H "Mcp-Method: $method" \
    ${name:+-H "Mcp-Name: $name"} -H "X-AgentGate-Run: ${RUNID:-}" "$@" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":$ID,\"method\":\"$method\",\"params\":{${params:+$params,}$META}}" "$GW/mcp/up" > "$WORK/code" || true
  HTTP_CODE="$(cat "$WORK/code")"
  BODY="$(cat "$WORK/body")"
}
errcode() { printf '%s' "$1" | grep -o '"code":-\?[0-9]*' | head -n1 | cut -d: -f2; }
rule() { printf '%s' "$1" | grep -o '"rule":"[a-z_]*"' | head -n1 | cut -d'"' -f4; }

RUNID="demo-run"
say "1. tools/list: the agent sees only the tools its scope allows (run id: $RUNID)"
rpc tools/list ''; out="$BODY"
names="$(printf '%s' "$out" | grep -o '"name":"[^"]*"' | cut -d'"' -f4 | sort | tr '\n' ' ')"
echo "   listed: $names"
for t in $(echo "$TOOLS" | tr ',' ' '); do
  printf '%s' "$names" | grep -qw "${t#up/}" || fail "scope tool ${t#up/} is not listed"
done
if [ "$OFFLINE" = 1 ]; then
  printf '%s' "$names" | grep -qw stream && fail "an out-of-scope tool is listed"
  printf '%s' "$names" | grep -qw fail && fail "an out-of-scope tool is listed"
  echo "   ok: stream and fail are not listed"
fi

say "2. a free tool and a priced tool"
if [ "$OFFLINE" = 1 ]; then ARGS_F='"arguments":{"text":"hello"}'; ARGS_P='"arguments":{"text":"paid hello"}'
else ARGS_F='"arguments":{"message":"hello"}'; ARGS_P='"arguments":{"a":2,"b":3}'; fi
rpc tools/call "\"name\":\"$FREE\",$ARGS_F" -H 'Idempotency-Key: demo-free'; out="$BODY"
expect "free tool answers" "200" "$HTTP_CODE"; printf '%s' "$out" | grep -q '"result"' || fail "no result: $out"
"$AG" account balance --account "$ACCT"
rpc tools/call "\"name\":\"$PAID\",$ARGS_P" -H 'Idempotency-Key: demo-paid'; out="$BODY"
expect "priced tool answers" "200" "$HTTP_CODE"; printf '%s' "$out" | grep -q '"result"' || fail "no result: $out"
BAL="$("$AG" account balance --account "$ACCT")"; echo "   $BAL"
printf '%s' "$BAL" | grep -q 'balance 4.995' || fail "the priced call should have cost \$0.005: $BAL"

if [ "$OFFLINE" = 1 ]; then
  say "3. out of scope: a JSON-RPC error, and the upstream never sees it"
  rpc tools/call '"name":"fail","arguments":{}'; out="$BODY"
  expect "HTTP status (the transport worked)" "200" "$HTTP_CODE"
  expect "JSON-RPC error code (policy denied)" "40301" "$(errcode "$out")"
  expect "rule" "scope_tool" "$(rule "$out")"

  say "4. a tool result that carries an instruction is blocked"
  rpc tools/call "\"name\":\"$INJECT\",\"arguments\":{}"; out="$BODY"
  expect "JSON-RPC error code (screened)" "40302" "$(errcode "$out")"
  expect "rule" "instruction_override" "$(rule "$out")"
  printf '%s' "$out" | grep -q 'evil.example' && fail "blocked content reached the client"
  grep -q 'evil.example' "$WORK/gw.log" && fail "blocked content reached the gateway log"
  grep 'tool output blocked' "$WORK/gw.log" | sed 's/^/   log: /'
  psql -XAt -h "$PGHOST" "$DB" -c "select '   screen_events: ' || rule || ' sha256=' || left(content_sha256, 16) || '...' from screen_events"

  say "5. run show, then kill the run while a call is in flight"
  "$AG" run show "$RUNID"
  (rpc tools/call '"name":"slow","arguments":{"ms":20000}'; echo "$BODY" > "$WORK/slow.out") &
  SLOWPID=$!
  sleep 1
  START=$SECONDS
  "$AG" run kill "$RUNID"
  wait "$SLOWPID" || true
  echo "   in-flight call ended $((SECONDS - START)) s after the kill: $(cat "$WORK/slow.out")"
  [ $((SECONDS - START)) -le 2 ] || fail "the in-flight call was not cut quickly"
  expect "killed in flight" "run_killed" "$(rule "$(cat "$WORK/slow.out")")"
  rpc tools/call "\"name\":\"$FREE\",$ARGS_F"; out="$BODY"
  expect "the next request of the killed run is refused" "run_killed" "$(rule "$out")"
  RUNID="another-run"
  rpc tools/call "\"name\":\"$FREE\",$ARGS_F"; out="$BODY"
  expect "another run is unaffected" "200" "$HTTP_CODE"; printf '%s' "$out" | grep -q '"result"' || fail "another run: $out"
else
  say "3-5. screening and kill use the fake's special tools; with the real server see the unit tests and the offline demo"
  "$AG" run show "$RUNID"
fi

say "the run, from the ledger"
"$AG" run list
"$AG" account balance --account "$ACCT"
printf '\nDemo OK (%s)\n' "$([ "$OFFLINE" = 1 ] && echo offline || echo "online: server-everything")"
