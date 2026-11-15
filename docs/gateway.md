# Gateway

[中文](zh/gateway.md)

The gateway sits between your agents and the model providers. Agents call it the same way they call OpenAI or Anthropic, so most tools only need one URL changed. Claude Code works this way.

<p align="center"><img src="images/gateway.png" width="60%" alt="Agents use a gateway key. The gateway holds the real provider key and streams answers back."></p>

## The real key stays on the gateway

Each agent gets a key that is only good for talking to the gateway. The real provider key stays on the gateway. If a key leaks or an agent gets hijacked, you shut off that one agent and your provider account is untouched.

## Identity without a key

An agent can prove who it is with a short-lived SPIFFE certificate instead of a key. The gateway has an optional second port that requires mutual TLS and accepts only certificates from one trust domain. The certificate's SPIFFE ID is mapped to one agent, for example with `agentgate agent set-spiffe`. A request on that port is that agent, and any key it also sends is ignored.

Keys stay as the fallback for clients that cannot get a certificate. You can turn the key off for an agent, so the certificate is its only way in. See [deploy/spire](../deploy/spire/README.md) for a single-VM setup.

## How a request flows

1. Work out who is calling.
2. Check whether it may make this request.
3. Set aside enough money to cover the worst case.
4. Call the provider and stream the answer back as it arrives.

## Who pays when something goes wrong

| What happened | Result |
|---|---|
| The stream dies halfway | You pay for what arrived |
| The provider returns an error | You pay nothing |
| The same request is sent twice | The second is turned away before it costs anything |

## Where prices come from

The provider's own figure comes first, then our price table, then an estimate that is clearly marked as one.
