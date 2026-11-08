# Limits

[中文](zh/limits.md)

Here's what this project can't do, so you don't have to find out the hard way.

- **It has seen only a little real traffic.** Two days of the author's own agents, with the numbers in the [README](../README.md#status). All performance numbers still come from a controlled setup.
- **It's testnet only.** The wallet refuses mainnet on purpose, and chain reorganisations aren't handled. Payments have settled on a public testnet, but only at small scale.
- **It runs as one process.** Several gateways could share the ledger safely, but the kill switch assumes one.
- **Its screening is shallow.** It stops common injection attacks, not a careful adversary. See [Tools](tools.md).
- **You need to protect the host.** Anyone who controls the machine has the provider keys and the database. Run it on a trusted network, behind a proxy that handles TLS.
- **It's deliberately small.** There's a console for one operator and a few people they let in, but no provider failover, load balancing or multi-tenant management.
