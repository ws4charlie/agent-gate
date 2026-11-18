# Identity: certificate or API key

[中文](zh/identity.md)

Every agent must prove who it is. Use a certificate where you can and an API key where you cannot. Once an agent's certificate works, turn its key off, so the agent has one way in and not two.

## Certificate (SPIFFE)

A SPIFFE certificate lasts about an hour and renews itself, so a leaked one is useless soon. The gateway has an optional mutual-TLS port that accepts only certificates from the trust domains you list. You map each certificate ID to one agent; a request on that port is that agent, and any key it sends is ignored.

Use it when SPIRE can attest the workload:

- **Our own runs.** Each research run is a systemd unit under its own unix user and fetches its own certificate. See [runner-user](../deploy/runner-user.md).
- **A customer's own SPIRE.** Federate the two SPIRE servers, list both trust domains on the gateway, and map the customer's IDs to agents. See [SPIRE](../deploy/spire/README.md).

## API key

Use a key for clients that cannot get a certificate, such as Claude Code or a customer without SPIRE. Make it safer:

| Safeguard | How |
|---|---|
| Expiry | Set one with `--key-ttl` or `agent key-expiry`; a key past it is refused. It never expires by default. |
| Rotation | `agentgate agent rotate-key` shows the new key once; the old one works for 24 hours (`--grace`). |
| Warning | In its last 7 days, or while an old key is in use, every response carries `X-AgentGate-Key-Expires`. |
| Limits | Each agent has its own budget, scope and approval rules. See [Policy](policy.md). |

## Turning the key off

When the certificate works, run `agentgate agent api-key <agent> off`. Every key of that agent is then refused.

If SPIRE is down, the gateway keeps serving its plain port, so key agents are unaffected. A certificate-only agent cannot connect until SPIRE is back. Keep the key on for an agent that must never be locked out.

## Where to look

[Gateway](gateway.md) · [Policy](policy.md) · [SPIRE setup](../deploy/spire/README.md) · [Runner user](../deploy/runner-user.md)
