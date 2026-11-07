# Testing

[中文](zh/testing.md)

Testing something that spends money has an obvious problem: you don't want to spend money. So almost everything here runs with no keys, no faucet and nothing leaving the machine.

<p align="center"><img src="images/testing.png" width="60%" alt="Three levels of testing: fakes, local end to end and live."></p>

## Three levels of testing

### Fakes

A fake model provider and a simulated blockchain. They're fast and cover the unpleasant cases, such as crashes, dropped streams and two requests racing each other.

### Local end to end

A real local blockchain node with a stand-in USDC, plus a scripted model server that answers over real HTTP. The gateway can't tell the difference and has no special test mode. This shows the whole path works, and CI runs it on every change.

LiteScraper can record real results and replay them later, so testing a real agent locally spends no search credits and never hits a rate limit.

### Live

Real providers and real networks. This is the thin part so far: testing against OpenRouter has only just started, and settlement on a public testnet is still small. More is coming.

## Scripted scenarios

Each scenario has an expected verdict, such as "this should be refused for being over budget". Many are requests that should be allowed, because otherwise a gateway that refuses everything would look perfect.

## What a clean score doesn't show

We wrote the scenarios alongside the rules, so passing shows the rules do what we meant, not how they hold up against an unfamiliar attacker. Local tests prove our accounting, not how a real provider or Circle's real USDC behaves.
