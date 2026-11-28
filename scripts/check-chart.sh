#!/usr/bin/env bash
# check-chart: lint and render the Helm chart (deploy/helm/agentgate) with
# both signers and check what it renders:
#   - every env var name it sets is listed in .env.example (so the chart and
#     the code cannot drift apart: cmd/agentgate/envlist_test.go ties
#     .env.example to the code);
#   - signer=vault sets every AG_VAULT_* in .env.example and the Gate
#     credential key; signer=wallet sets none of them;
#   - the chart never renders a Secret;
#   - the defaults alone refuse to render (the database Secret is required);
#   - every other validation refuses its bad setting (must_fail cases).
# Needs helm. Run from anywhere: scripts/check-chart.sh
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
chart=deploy/helm/agentgate
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
fail=0
bad() { echo "check-chart: $*"; fail=1; }

listed=$(sed -n 's/^\([A-Z][A-Z0-9_]*\)=.*/\1/p' .env.example | sort -u)
envnames() { sed -n 's/^ *- {*name: \([A-Z][A-Z0-9_]*\)[,}]*.*/\1/p' "$1" | sort -u; }

for mode in wallet vault; do
  helm lint --strict "$chart" -f "$chart/ci/$mode-values.yaml" >/dev/null || bad "helm lint ($mode) failed"
  helm template t "$chart" -f "$chart/ci/$mode-values.yaml" > "$tmp/$mode.yaml"
  n=$(envnames "$tmp/$mode.yaml" | wc -l | tr -d ' ')
  [ "$n" -gt 5 ] || bad "$mode: found only $n env names; the pattern no longer matches the templates"
  for v in $(envnames "$tmp/$mode.yaml"); do
    grep -qx "$v" <<<"$listed" || bad "$mode: env $v is not in .env.example"
  done
  if grep -q '^kind: Secret$' "$tmp/$mode.yaml"; then bad "$mode: the chart renders a Secret"; fi
  echo "check-chart: $mode: $n env names checked"
done

for v in $(grep -o '^AG_VAULT_[A-Z_]*' .env.example) AG_GATE_CRED_KEY_FILE AG_GATE_CRED_KID; do
  envnames "$tmp/vault.yaml" | grep -qx "$v" || bad "vault: $v is not set"
  if envnames "$tmp/wallet.yaml" | grep -qx "$v"; then bad "wallet: $v is set"; fi
done
envnames "$tmp/wallet.yaml" | grep -qx AG_WALLET_KEY_FILE || bad "wallet: AG_WALLET_KEY_FILE is not set"
if envnames "$tmp/vault.yaml" | grep -qx AG_WALLET_KEY_FILE; then bad "vault: AG_WALLET_KEY_FILE is set"; fi

if helm template t "$chart" >/dev/null 2>&1; then bad "the defaults render without database.existingSecret"; fi

# Each validation refuses a bad setting with its own message.
# must_fail <base: wallet|vault> <expected message part> <helm args...>
must_fail() {
  local base=$1 want=$2; shift 2
  local out
  if out=$(helm template t "$chart" -f "$chart/ci/$base-values.yaml" "$@" 2>&1); then
    bad "renders, but should fail ($want): $*"
  elif ! grep -qF -- "$want" <<<"$out"; then
    bad "fails with the wrong message for $*: $(tail -n1 <<<"$out")"
  fi
}
must_fail vault "not both" --set chain.rpcUrl=http://anvil:8545
must_fail vault "chain.rpcUrl or chain.rpcUrlSecret.name is required" --set chain.rpcUrlSecret.name=
must_fail wallet "chain.rpcUrl or chain.rpcUrlSecret.name is required" --set paidtool.network=anvil,payments.network=anvil
must_fail wallet "differ" --set paidtool.network=anvil,chain.rpcUrl=http://anvil:8545
must_fail wallet "networkPolicy.egress.postgres is required" --set networkPolicy.egress.postgres=null
must_fail vault "networkPolicy.egress.vault is required" --set networkPolicy.egress.vault=null
must_fail vault "networkPolicy.egress.tokenProvider is required" --set networkPolicy.egress.tokenProvider=null
printf 'networkPolicy:\n  egress:\n    vault: [{to: [{namespaceSelector: {}}]}]\n' > "$tmp/noports.yaml"
must_fail vault "every rule needs ports" -f "$tmp/noports.yaml"
must_fail wallet "image.digest must be" --set image.digest=latest
must_fail wallet "wallet.key must be a plain file name" --set wallet.key=../x.key
must_fail wallet "must end in .cedar" --set gateway.cedarPolicies.policy=x
must_fail wallet "paidtool.fund needs paidtool.fundAddress" --set paidtool.fundAddress=

# The settings that pass: the literal RPC URL, and the image by digest.
lit=$(helm template t "$chart" -f "$chart/ci/vault-values.yaml" --set chain.rpcUrlSecret.name=,chain.rpcUrl=http://anvil.qv.svc:8545) ||
  bad "vault with a literal chain.rpcUrl does not render"
grep -q 'name: AG_RPC_URL, value: "http://anvil.qv.svc:8545"' <<<"$lit" || bad "literal chain.rpcUrl is not rendered"
d=sha256:$(printf '%064d' 0)
helm template t "$chart" -f "$chart/ci/wallet-values.yaml" --set image.digest=$d | grep -q "image: \"agentgate@$d\"" ||
  bad "image.digest is not rendered as repository@digest"

[ $fail -eq 0 ] && echo "check-chart: ok"
exit $fail
