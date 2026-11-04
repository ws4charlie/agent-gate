# Sourced by the demos for `--network anvil`: start a local anvil node with the
# Base Sepolia chain id if none is running, and stop only what we started.
# Needs `anvil` from Foundry. Nothing here touches a real network: the
# agentgate binary itself refuses a node that does not call itself anvil.

ANVIL_URL="${AG_RPC_URL:-http://127.0.0.1:8545}"
ANVIL_PID=""

anvil_rpc() { # anvil_rpc <method>: the raw JSON-RPC reply, or empty if nothing answers
  curl -sf -m 2 -X POST -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":[]}" "$ANVIL_URL" 2>/dev/null || true
}

anvil_up() { # anvil_up <workdir>
  export AG_RPC_URL="$ANVIL_URL"
  if [ -n "$(anvil_rpc eth_chainId)" ]; then
    echo "anvil already running at $ANVIL_URL (left running)"
  else
    command -v anvil >/dev/null || { echo "anvil not found: install Foundry (https://getfoundry.sh)" >&2; exit 2; }
    local port="${ANVIL_URL##*:}"
    anvil --chain-id 84532 --port "${port%%/*}" --silent > "$1/anvil.log" 2>&1 &
    ANVIL_PID=$!
    for _ in $(seq 100); do [ -n "$(anvil_rpc eth_chainId)" ] && break; sleep 0.1; done
    [ -n "$(anvil_rpc eth_chainId)" ] || { cat "$1/anvil.log" >&2; echo "anvil did not start" >&2; exit 1; }
    echo "started anvil (chain id 84532) at $ANVIL_URL"
  fi
}

anvil_down() { [ -z "$ANVIL_PID" ] || kill "$ANVIL_PID" 2>/dev/null || true; }
