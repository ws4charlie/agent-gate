# Limits

[中文](zh/limits.md)

Here's what this project is not, so you don't have to find out the hard way.

**It hasn't seen real traffic.** There's been one small live run against OpenRouter. No real workload has gone through it yet, so any performance numbers come from a controlled setup.

**It's testnet only.** The wallet refuses mainnet on purpose, and chain reorganisations aren't handled. A public testnet run is written but hasn't happened.

**It runs as one process.** Several gateways could share the ledger safely, but the kill switch assumes one.

**Its screening is shallow.** It stops common injection attacks, not a careful adversary. See [Tools](tools.md).

**You need to protect the host.** Anyone who controls the gateway machine has the provider keys and the database. Run it on a trusted network, behind a proxy that handles TLS.

**It's deliberately small.** There's no dashboard, provider failover, load balancing or multi-tenant management.
