#!/usr/bin/env bash
# check-chart: lint and render the Helm chart (deploy/helm/agentgate) on sim
# and on a chain (QuorumVault signs) and check what it renders:
#   - every env var name it sets is listed in .env.example (so the chart and
#     the code cannot drift apart: cmd/agentgate/envlist_test.go ties
#     .env.example to the code);
#   - payments on a chain set every AG_VAULT_* in .env.example and the Gate
#     credential key; payments on sim set none of them; neither sets a
#     retired local-key setting (AG_SIGNER, AG_WALLET_KEY*);
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

for mode in sim vault; do
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
  if envnames "$tmp/sim.yaml" | grep -qx "$v"; then bad "sim: $v is set"; fi
done
for mode in sim vault; do
  if grep -q 'AG_SIGNER\|AG_WALLET_KEY' "$tmp/$mode.yaml"; then bad "$mode: renders a retired local-key setting"; fi
done

if helm template t "$chart" >/dev/null 2>&1; then bad "the defaults render without database.existingSecret"; fi

# Each validation refuses a bad setting with its own message.
# must_fail <base: sim|vault> <expected message part> <helm args...>
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
must_fail sim "chain.rpcUrl or chain.rpcUrlSecret.name is required" --set paidtool.network=anvil,payments.network=anvil
must_fail sim "differ" --set paidtool.network=anvil,chain.rpcUrl=http://anvil:8545
must_fail sim "networkPolicy.egress.postgres is required" --set networkPolicy.egress.postgres=null
must_fail vault "networkPolicy.egress.vault is required" --set networkPolicy.egress.vault=null
must_fail vault "networkPolicy.egress.tokenProvider is required" --set networkPolicy.egress.tokenProvider=null
printf 'networkPolicy:\n  egress:\n    vault: [{to: [{namespaceSelector: {}}]}]\n' > "$tmp/noports.yaml"
must_fail vault "every rule needs ports" -f "$tmp/noports.yaml"
must_fail sim "image.digest must be" --set image.digest=latest
# Payments on a chain without the Vault settings: there is no other signer.
must_fail sim "vault.url is required with payments on anvil" --set payments.network=anvil,paidtool.network=anvil,chain.rpcUrl=http://anvil:8545,networkPolicy.enabled=false
must_fail sim "gateCred.existingSecret is required" --set networkPolicy.enabled=false,payments.network=base-sepolia,payments.paidtoolTools=null,chain.rpcUrl=http://rpc,vault.url=u,vault.walletId=w,vault.walletAddress=a,vault.tokenUrl=t,vault.clientId=c,vault.audience=q,vault.existingSecret=s
must_fail vault "vault.audience is required" --set vault.audience=
# An old deployment's signer values stop the render, never pick another signer.
must_fail vault "signer was removed" --set signer=wallet
must_fail sim "wallet was removed" --set wallet.existingSecret=agentgate-wallet
must_fail sim "must end in .cedar" --set gateway.cedarPolicies.policy=x
must_fail vault "paidtool.fund needs paidtool.fundAddress" --set paidtool.fund=5
# sim in k8s cannot be funded: the gateway's sim key is ephemeral.
must_fail sim "sim payments are not payable in Kubernetes" --set paidtool.fund=5,paidtool.fundAddress=0x70997970C51812dc3A010C7d01b50e0d17dc79C8
must_fail sim "sim payments are not payable in Kubernetes" --set paidtool.fundAddress=0x70997970C51812dc3A010C7d01b50e0d17dc79C8
must_fail sim "paidtool.token is for paidtool.network=anvil only" --set paidtool.token=0x00000000000000000000000000000000000000aa
must_fail vault "paidtool.token must be a 0x address" --set paidtool.token=usdc
must_fail vault "gateCred.key must be a plain file name" --set gateCred.key=../k

# The settings that pass: the literal RPC URL, and the image by digest.
lit=$(helm template t "$chart" -f "$chart/ci/vault-values.yaml" --set chain.rpcUrlSecret.name=,chain.rpcUrl=http://anvil.qv.svc:8545) ||
  bad "vault with a literal chain.rpcUrl does not render"
grep -q 'name: AG_RPC_URL, value: "http://anvil.qv.svc:8545"' <<<"$lit" || bad "literal chain.rpcUrl is not rendered"
tok=0x00000000000000000000000000000000000000aa
withtok=$(helm template t "$chart" -f "$chart/ci/vault-values.yaml" --set paidtool.token=$tok) || bad "paidtool.token does not render"
grep -q -- "- --token=$tok" <<<"$withtok" || bad "paidtool.token is not passed as --token"
# The Gate credential init container runs this chart's own image, not another one.
grep -A3 'name: gate-cred$' "$tmp/vault.yaml" | grep -q 'image: "agentgate:' || bad "the gate-cred init container does not use the agentgate image"
d=sha256:$(printf '%064d' 0)
helm template t "$chart" -f "$chart/ci/sim-values.yaml" --set image.digest=$d | grep -q "image: \"agentgate@$d\"" ||
  bad "image.digest is not rendered as repository@digest"

[ $fail -eq 0 ] && echo "check-chart: ok"
exit $fail
