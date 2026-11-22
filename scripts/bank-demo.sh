#!/usr/bin/env bash
# Bank rail demo against Mercury's SANDBOX (fake money). Needs a Postgres
# (DATABASE_URL) and these in the environment:
#   MERCURY_API_TOKEN (or AG_MERCURY_TOKEN)  the sandbox token; never printed here
#   AG_MERCURY_ACCOUNT                       checking account id to pay from
#   DEMO_RECIPIENT_ID                        an ACH recipient id in that sandbox org
# Optional: DEMO_WAIT (seconds to wait for a human at Mercury, default 60),
#           DEMO_PORT (default 18095).
#
# Demo A: a normal ACH. Gate allows it, files it at Mercury, and it waits for a
#         human to approve it in the Mercury dashboard. Exits 0 if nobody does.
# Demo B: an ACH over the agent's limit. Refused on the spot, audit row, no call to Mercury.
# Demo C: runs only if A was approved and sent: a simulated ACH return.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${DATABASE_URL:?set DATABASE_URL}"
: "${AG_MERCURY_ACCOUNT:?set AG_MERCURY_ACCOUNT}"
: "${DEMO_RECIPIENT_ID:?set DEMO_RECIPIENT_ID}"
[ -n "${MERCURY_API_TOKEN:-}${AG_MERCURY_TOKEN:-}" ] || { echo "set MERCURY_API_TOKEN (sandbox token)"; exit 1; }
WAIT=${DEMO_WAIT:-60}
PORT=${DEMO_PORT:-18095}
WORK=$(mktemp -d)
trap 'kill "${GW:-}" 2>/dev/null || true; rm -rf "$WORK"' EXIT
AG="$WORK/agentgate"
go build -o "$AG" ./cmd/agentgate

"$AG" migrate >/dev/null
# The registry check is on in this demo, so the rail's tool must be approved.
"$AG" registry add tool mercury/ach --owner demo --purpose "ACH through Mercury sandbox" --status approved >/dev/null

ACCT=$("$AG" account create --name "bank-demo-$$")
"$AG" account deposit --account "$ACCT" --amount 1000 >/dev/null
KEY=$("$AG" agent create --account "$ACCT" --name "bank-demo-$$" --models '*' --tools mercury/ach \
  --payees "$DEMO_RECIPIENT_ID" --daily 100 --approval-over 50 2>/dev/null)

AG_LISTEN="127.0.0.1:$PORT" "$AG" serve --mercury --mercury-poll 5s --registry-enforce >"$WORK/gateway.log" 2>&1 &
GW=$!
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.2; done

pay() { # amount idempotency-key
  curl -s -X POST "http://127.0.0.1:$PORT/v1/bank/payments" -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"recipient_id\":\"$DEMO_RECIPIENT_ID\",\"amount_usd\":\"$1\",\"idempotency_key\":\"$2\",\"memo\":\"bank demo\"}"
}
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p"; }

echo "== Demo A: a normal \$1.00 ACH"
A=$(pay 1.00 "demo-a-$$-$(date +%s)")
echo "$A"
PID=$(echo "$A" | field payment_id)
[ -n "$PID" ] || { echo "no payment id; gateway log:"; cat "$WORK/gateway.log"; exit 1; }

echo
echo "== Demo B: a \$150 ACH, over this agent's \$100 daily limit"
pay 150 "demo-b-$$-$(date +%s)"
echo
"$AG" bank list

echo
echo "== Waiting up to ${WAIT}s for a human to approve request $(echo "$A" | field mercury_request_id) in the Mercury sandbox dashboard"
end=$((SECONDS + WAIT))
state=awaiting_bank_approval
while [ $SECONDS -lt $end ]; do
  state=$("$AG" bank show "$PID" | awk '$1=="state"{print $2}')
  case "$state" in sent|settled|cancelled|failed|returned) break ;; esac
  sleep 5
done
echo "state: $state"
"$AG" bank show "$PID"

case "$state" in
  sent|settled)
    echo
    echo "== Demo C: simulated ACH return (the sandbox cannot produce a real one)"
    "$AG" bank simulate-return "$PID" --code R01
    "$AG" bank show "$PID"
    "$AG" account balance --account "$ACCT" ;;
  awaiting_bank_approval)
    echo
    echo "waiting for Mercury approval in the dashboard: nobody approved in ${WAIT}s. Gate keeps tracking it while the gateway runs."
    echo "(The token's own user cannot approve; a second Mercury user is needed. See docs/bank.md.)" ;;
esac
exit 0
