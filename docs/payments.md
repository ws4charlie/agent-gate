# Payments

[中文](zh/payments.md)

Sometimes a tool charges per use, typically a few cents a call. The gateway can pay it on the agent's behalf, so the agent needs no crypto setup of its own. Ordinary tools know nothing about this. Only tools built to charge take part.

## The basics

- **USDC** is a digital dollar. One USDC is meant to be worth one US dollar.
- **A blockchain** is a public record of who holds what. Anyone can read it, which is why we can check it ourselves.
- **A wallet** holds the private key that authorizes spending. Ours sits inside the gateway, away from the agents.
- **x402** is an open standard for charging over the web. The tool answers with HTTP's 402 Payment Required status and a price. The caller repeats the request with payment attached.

USDC also lets a wallet pay by signature, and this is the part that surprises people. The wallet doesn't send the money itself. It signs a note that says "pay this tool three cents", much like writing a check. The tool cashes the check on the blockchain, either itself or through a service it uses, and covers the network fee.

## One payment, step by step

1. The gateway calls the tool. The tool answers with 402 and a price.
2. The gateway holds that amount from the agent's balance in the ledger.
3. Policy decides. If the answer is no, the hold is released and nothing is signed.
4. The wallet signs, and the gateway repeats the request with the signature attached.
5. The tool checks the signature and cashes it. USDC moves from our wallet to the tool.
6. The tool returns its result, and the ledger turns the hold into a charge.

<p align="center"><img src="images/payments.png" width="60%" alt="A payment goes through policy, then the wallet, then the chain. If it fails, the chain decides."></p>

The rule that matters most is that a payment has to pass policy before the wallet will sign it. This is built into the code's structure, not just a check someone remembered to add: the signing path can't run without proof of approval, and that proof works once, for one payment. The wallet also has its own spending cap per transaction, so a mistake in policy still can't drain it.

## Who decides whether the money moved

Payments have a nasty edge case. Once the wallet signs, the tool holds a check it can cash until the signature expires. It might say "sorry, that failed" and cash it anyway a minute later, by accident or on purpose.

So a failure report from the tool proves nothing. If the tool reports success, we charge. For anything else, the hold stays until the signature has expired, and then we check the blockchain. If the money moved, we charge. If it didn't, we release the hold.

Charging and releasing are both entries in our ledger. Nothing is sent back on the blockchain, because in the release case the money never left.

We take a reported success at its word because a tool gains nothing by claiming a payment it didn't collect. If one did, the agent would be overcharged, and the report that compares the ledger with the blockchain would show it.

The cost of waiting is that a failed payment takes a few minutes to come back. We accept that, because the alternative is giving back money that has already left.

## LiteScraper, the demo seller

Real paid tools want real money, and our wallet only works on testnets. So the repository includes its own paid search tool, LiteScraper, to play the seller.

LiteScraper is a small reseller. It charges the agent through x402 in test USDC, which goes to our own address. It then runs the search on Firecrawl with an ordinary API key. The search results are real, but Firecrawl never sees a payment or a signature.

This proves the gateway's payment path works on a public network. It doesn't prove it works with a seller we don't control.

## Good to know

This runs on testnets only, practice networks where the tokens have no real value. The wallet refuses to work on mainnet.

An agent's balance is a number in our ledger. The USDC in the wallet is the money itself. These are two different things, and a payment needs both.

All agents share one wallet. Their budgets are separate, but the funds aren't.
