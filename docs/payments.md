# Payments

[中文](zh/payments.md)

Some tools charge per use, typically a few cents a call. The gateway can pay on the agent's behalf, so the agent needs no crypto setup. Ordinary tools aren't affected. Only tools built to charge take part.

<p align="center"><img src="images/payments.png" width="60%" alt="A payment goes through policy, then the wallet, then the chain. If it fails, the chain decides."></p>

## The basics

- **USDC:** a digital dollar. One USDC is meant to be worth one US dollar.
- **Blockchain:** a public record of who holds what, which anyone can read.
- **Wallet:** holds the private key that authorizes spending. Ours sits inside the gateway, away from the agents.
- **x402:** an open standard for charging over the web. The tool answers with HTTP 402 and a price, and the caller repeats the request with payment attached.

USDC also lets a wallet pay by signature. The wallet doesn't send the money itself. It signs a note that says "pay this tool three cents", much like writing a check. The tool cashes it on the blockchain and covers the network fee.

## One payment, step by step

1. The gateway calls the tool. The tool answers with 402 and a price.
2. The gateway holds that amount in the ledger.
3. Policy decides. If the answer is no, the hold is released and nothing is signed.
4. The wallet signs, and the gateway repeats the request with the signature.
5. The tool checks the signature and cashes it. USDC moves to the tool.
6. The tool returns its result, and the ledger turns the hold into a charge.

## The wallet signs only after policy passes

This is built into the code's structure: the signing path can't run without proof of approval, and each proof covers one payment. The wallet also has a cap per transaction, so a mistake in policy still can't drain it.

## The chain decides whether the money moved

Once signed, the tool can cash the check until the signature expires. It might report a failure and cash it a minute later, so a failure report proves nothing.

| Tool's report | What we do |
|---|---|
| Success | Charge |
| Anything else | Keep the hold until the signature expires, then check the blockchain: charge if the money moved, release if it didn't |

Charging and releasing are ledger entries, so nothing is sent back on the blockchain. A tool gains nothing by claiming a payment it didn't collect, and the reconciliation report would show it if one did.

The cost is that a failed payment takes a few minutes to come back. We accept that, because the alternative is giving back money that has already left.

## LiteScraper, the demo seller

Our wallet only works on testnets, and real paid tools want real money. So the repository includes a paid search tool, LiteScraper, to play the seller.

It charges the agent in test USDC, which goes to our own address, then runs the search on Firecrawl with an ordinary API key. The results are real, but Firecrawl never sees a payment or a signature.

This proves the payment path works on a public network. It doesn't prove it works with a seller we don't control.

## Good to know

- This runs on testnets only, and the wallet refuses mainnet.
- An agent's balance is a number in our ledger, while the USDC in the wallet is the money itself. A payment needs both.
- All agents share one wallet: budgets are separate, funds aren't.
