# Python SDK

[中文](zh/sdk.md)

You don't need an SDK, since the gateway speaks standard formats. The Python client is a small convenience for grouping calls into one job, paying for tools and checking remaining budget.

<p align="center"><img src="images/sdk.png" width="60%" alt="The SDK turns gateway refusals into clear errors."></p>

## Refusals become clear errors

When the gateway refuses something, you get a specific error such as "over budget", "outside your scope" or "this agent was killed". Your code can respond sensibly, instead of treating every refusal as worth retrying.

## Two responses that don't mean success

| Response | Meaning | What to do |
|---|---|---|
| Approval required | A person has to approve this first | Wait for approval |
| Settlement pending | A payment may have gone through, and we're waiting for the chain | Don't retry it as a new payment |
