"""An agent that pays for tools (x402, on a simulated chain) through the gateway.

  paying.py echo            a $0.01 payment inside policy
  paying.py premium         a $0.50 payment over the daily budget: refused before signing
  paying.py queued IDEM     a payment over the approval threshold: waits for a human
  paying.py replay IDEM     the same request again, same key, after approval

Last line is `RESULT key=value ...`."""
import sys

from agentgate import ApprovalRequired, BudgetExceeded
from _env import client, say

c = client()
mode = sys.argv[1] if len(sys.argv) > 1 else "echo"
idem = sys.argv[2] if len(sys.argv) > 2 else None

try:
    if mode in ("echo", "replay"):
        r = c.pay("echo", {"text": "hello"}, idempotency_key=idem)
        say(f"paid {r['charged']} USD, tx {r['transaction'][:14]}...")
        print(f"RESULT charged={r['charged']} tx={'yes' if r['transaction'] else 'no'}")
    elif mode == "premium":
        c.pay("premium", {"text": "hello"}, idempotency_key=idem)
        sys.exit("premium should have been refused")
    elif mode == "queued":
        c.pay("echo", {"text": "hello"}, idempotency_key=idem)
        sys.exit("should have been queued")
    else:
        sys.exit(__doc__)
except BudgetExceeded as e:
    say(f"refused before signing: {e.rule}")
    print(f"RESULT rule={e.rule}")
except ApprovalRequired as e:
    say(f"waiting for a human, approval id {e.id}")
    print(f"RESULT approval={e.id}")
