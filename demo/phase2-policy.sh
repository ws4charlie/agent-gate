#!/usr/bin/env bash
# Phase 2 demo: custody for agents. Offline: a fake upstream, no keys, no network.
#
#   1. burn a daily budget            -> the 5th call is refused (budget_daily)
#   2. trip the velocity limit        -> parallel calls past $/minute are refused
#   3. out-of-scope model             -> refused before any hold (scope_model)
#   4. over-limit request queued      -> 202, a human approves it, it runs once
#   5. kill an agent mid-stream       -> the stream is cut within a second, the
#                                        next request is refused, usage is settled
#
# Every step checks its own outcome and the script exits non-zero if one is wrong.
# Needs: go, curl, and a local Postgres reachable with createdb/dropdb
# (default: socket in /tmp; override with PGHOST).
set -euo pipefail
cd "$(dirname "$0")/.."

export PGHOST="${PGHOST:-/tmp}"
DB="agentgate_demo2_$$"
WORK="$(mktemp -d)"
PORT="${DEMO_PORT:-18082}"
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
go build -o "$WORK/fakeupstream" ./cmd/fakeupstream
createdb "$DB"
export DATABASE_URL="postgres:///$DB?host=$PGHOST"
export AG_LISTEN="127.0.0.1:$PORT"
AG="$WORK/agentgate"

say "start the fake upstream (calls take 300 ms; streams are long: 100 chunks, 100 ms apart)"
"$WORK/fakeupstream" -latency 300ms -chunk-delay 100ms -chunks 100 > "$WORK/fake.out" &
PIDS+=($!)
for _ in $(seq 50); do [ -s "$WORK/fake.out" ] && break; sleep 0.1; done
FAKE="$(head -n1 "$WORK/fake.out")"

cat > "$WORK/providers.json" <<JSON
{
  "providers": {"fake": {"kind": "openai-compatible", "base_url": "$FAKE/v1"}},
  "routes": [{"prefix": "fake/", "provider": "fake", "strip": true}]
}
JSON
# A dear model, so a handful of calls is real money: \$1000 per million input
# tokens and \$2000 per million output (Nanos per 1M tokens). One call costs
# about \$0.02; the worst case the gateway reserves for it is about \$0.23.
row='{"input": 1000000000000, "output": 2000000000000, "cache_read": 0, "cache_write": 1000000000000}'
echo "{\"models\": {\"chat\": $row, \"chat-big\": $row}}" > "$WORK/prices.json"
export AG_PROVIDERS="$WORK/providers.json" AG_PRICES="$WORK/prices.json"
export FAKE_API_KEY=demo-upstream-key

say "migrate; one ledger account with \$5; the gateway"
"$AG" migrate
ACCT="$("$AG" account create --name demo)"
"$AG" account deposit --account "$ACCT" --amount 5
"$AG" serve --reconcile-every 1s > "$WORK/gw.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 50); do curl -sf "$GW/healthz" >/dev/null 2>&1 && break; sleep 0.1; done

mkkey() { "$AG" agent create --account "$ACCT" --name "$1" "${@:2}" 2>/dev/null; }
# call <key> <idempotency-key> <model> [stream] [extra header]  ->  "<http code> <rule or error code>"
call() {
  local stream=false; [ "${4:-}" = stream ] && stream=true
  local out; out="$(curl -sS -m 20 -w '\n%{http_code}' -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: $2" ${5:+-H "$5"} \
    -d "{\"model\":\"$3\",\"stream\":$stream,\"max_tokens\":100,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" "$GW/v1/chat/completions" || true)"
  local code="${out##*$'\n'}"
  local body="${out%$'\n'*}"
  local what; what="$(printf '%s' "$body" | grep -o '"code":"[a-z_]*"' | head -n1 | cut -d'"' -f4 || true)"
  [ -z "$what" ] && what="$(printf '%s' "$body" | grep -o '"status":"[a-z_]*"' | head -n1 | cut -d'"' -f4 || true)"
  echo "$code ${what:-ok}"
}

say "1. burn a daily budget: \$0.30 a day, each call really costs ~\$0.02, and each admission reserves ~\$0.23"
BURN="$(mkkey burner --models fake/chat --daily 0.30)"
for i in 1 2 3 4 5 6; do
  r="$(call "$BURN" "burn-$i" fake/chat)"; echo "   call $i: $r"
  last="$r"
done
expect "6th call is refused by the daily budget" "429 budget_daily" "$last"

say "2. trip the velocity limit: \$0.50 a minute; three calls at once, each reserving ~\$0.23"
RUN="$(mkkey sprinter --models fake/chat --velocity 0.50)"
for i in 1 2 3; do ( call "$RUN" "vel-$i" fake/chat > "$WORK/vel-$i.out" ) & sleep 0.05; done
for _ in $(seq 100); do [ "$(cat "$WORK"/vel-*.out 2>/dev/null | wc -l)" -ge 3 ] && break; sleep 0.1; done
cat "$WORK"/vel-*.out | sort | uniq -c
expect "exactly one of three is refused by velocity" "1" "$(grep -h 'velocity' "$WORK"/vel-*.out | wc -l | tr -d ' ')"

say "3. out-of-scope model: this agent may use fake/chat only"
r="$(call "$BURN" "scope-1" fake/chat-big)"; echo "   $r"
expect "fake/chat-big is outside the scope" "403 scope_model" "$r"

say "4. an over-limit request is queued; a human approves it; it runs once"
APPR="$(mkkey spender --models fake/chat --approval-over 0.10)"
r="$(call "$APPR" "big-1" fake/chat)"; echo "   first try: $r"
expect "the request waits for a human" "202 approval_required" "$r"
"$AG" approve list
ID="$("$AG" approve list | awk 'NR==2 {print $1}')"
"$AG" approve allow "$ID" --by demo-operator
r="$(call "$APPR" "big-1" fake/chat)"; echo "   replay: $r"
expect "the replay runs" "200 ok" "$r"
r="$(call "$APPR" "big-1" fake/chat)"; echo "   replay again: $r"
expect "a second replay is a duplicate" "409 duplicate_request" "$r"
r="$(call "$APPR" "big-2" fake/chat)"; echo "   another request: $r"
"$AG" approve deny "$("$AG" approve list | awk 'NR==2 {print $1}')" --by demo-operator
r="$(call "$APPR" "big-2" fake/chat)"; echo "   its replay: $r"
expect "a denied request is refused (and its hold released)" "403 approval_denied" "$r"

say "5. kill an agent mid-stream"
RUNAWAY="$(mkkey runaway --models fake/chat)"
curl -sSN -m 30 -H "Authorization: Bearer $RUNAWAY" -H 'Content-Type: application/json' -H 'Idempotency-Key: stream-1' \
  -d '{"model":"fake/chat","stream":true,"max_tokens":200,"messages":[{"role":"user","content":"go on forever"}]}' \
  "$GW/v1/chat/completions" > "$WORK/stream.out" 2>/dev/null &
CURL=$!
sleep 1.5
FLOW="$(grep -c '^data:' "$WORK/stream.out" || true)"
echo "   stream is flowing: $FLOW events so far (a full run is ~100 events, ~10 s)"
[ "$FLOW" -gt 3 ] || fail "the stream never started"
START=$SECONDS
"$AG" kill --agent runaway
wait "$CURL" || true
echo "   stream ended $((SECONDS - START)) s after the kill with $(grep -c '^data:' "$WORK/stream.out" || true) events"
[ $((SECONDS - START)) -le 2 ] || fail "the stream was not cut quickly"
r="$(call "$RUNAWAY" "after-kill" fake/chat)"; echo "   next request: $r"
expect "the killed agent is refused" "403 agent_killed" "$r"

say "balance and holds (the killed stream settled what it received; nothing is left held)"
"$AG" account balance --account "$ACCT"
sleep 0.5
held="$(psql -h "$PGHOST" -d "$DB" -Atc "select count(*) from holds where status='active'")"
expect "no active holds" "0" "$held"

say "agents"
"$AG" agent list

say "every decision, allow included, with the rule that made it"
psql -h "$PGHOST" -d "$DB" -c "select d.rule, d.decision, a.name as agent, d.kind, d.subject, d.amount_nanos
  from policy_decisions d join agents a on a.id = d.agent_id order by d.id" | head -n 60

say "audit log: statuses"
psql -h "$PGHOST" -d "$DB" -c "select status, count(*) from audit_events group by status order by 2 desc, 1"
echo
echo "phase 2 demo: all checks passed"
