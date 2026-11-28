# Infrastructure

The GCP deployment is described as code in `infra/terraform`. It covers the network, the SSH firewall rule, the VM, its service account and the one secret it may read. It does not cover what runs inside the VM or the secret's contents.

The code was written after the deployment existed, so it adopts the live resources instead of creating new ones. A plan against the real project should show no changes. Any difference is drift, and either the code or the project is wrong.

## What the policies guard

- the VM may have a public address only while every inbound rule admits Google's IAP range alone;
- nothing accepts traffic from the whole internet, and SSH comes only through the IAP range;
- the VM stays a Shielded VM;
- no one is granted the owner or editor role;
- no secret value is stored in Terraform;
- no startup script carries app configuration.

Each rule has a passing and a failing test.

## What CI does not do

CI never talks to GCP and holds no credentials. It checks formatting, validity and policy only. Plans and applies run on the lead's machine, with state in a bucket the human creates once.

## Container image and Helm chart

The `Dockerfile` at the repository root builds one image that holds only the static `agentgate` binary on a distroless base. It runs as a non-root user and has no shell. Migrations, the console's web files and the mock USDC contract are embedded in the binary. The build context is an allowlist (`.dockerignore`), so keys, `data/` and `.env` never reach the image.

```bash
docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t agentgate:local .
```

The chart in `deploy/helm/agentgate` runs that image twice: the gateway (`agentgate serve`) and, when `paidtool.enabled`, the x402 paid tool (`agentgate paidtool`). The gateway applies migrations at start and answers `/healthz` (process up) and `/readyz` (database reachable). Logs are JSON.

The chart is configured only through values that become env vars and mounted files. It never creates a secret: the database URL, the wallet key, the QuorumVault certificates and client secret, the Gate credential key, the facilitator key and the provider keys each come from a Secret you name in the values. `signer: wallet` or `signer: vault` picks the signer; in vault mode the chart sets every `AG_VAULT_*` setting and mounts the files, and in wallet mode it sets none of them. The gateway accepts the Gate credential key only as a private file it owns, so an init container copies it from its Secret into an in-memory directory first. `networkPolicy.enabled` denies all traffic except DNS, the egress groups you list (Postgres, Vault, the token endpoint, the chain RPC) and gateway to paid tool.

To try it in kind, load the image and install with your own values file. `values-local.yaml` is the example for the local cluster next to QuorumVault.

```bash
kind load docker-image agentgate:local --name <cluster>
helm upgrade --install agentgate deploy/helm/agentgate -n agentgate --create-namespace -f <your values>
```

`scripts/check-chart.sh` lints and renders the chart with both signers and checks that every env var it sets is listed in `.env.example`. CI runs it in the `helm` job.
