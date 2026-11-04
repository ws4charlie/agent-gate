"""An agent that does not stop: spends until a limit says no, or streams until it is killed.

  runaway.py burn       calls the model in a loop until the daily budget refuses
  runaway.py velocity   fires calls at once until the per-minute limit refuses
  runaway.py stream     reads a long stream; the operator kills it from outside

Last line is `RESULT key=value ...` for demo.sh to check."""
import sys
import time
from concurrent.futures import ThreadPoolExecutor

from agentgate import AgentGateError, BudgetExceeded, Killed, VelocityLimited
from _env import HI, client, say

c = client()
mode = sys.argv[1] if len(sys.argv) > 1 else "burn"

if mode == "burn":
    with c.run() as run_id:
        for n in range(1, 50):
            try:
                c.chat("fake/chat", HI)
                say(f"call {n}: ok")
            except BudgetExceeded as e:
                say(f"call {n}: refused by {e.rule}")
                print(f"RESULT rule={e.rule} calls={n - 1} run={run_id}")
                break
        else:
            sys.exit("the budget never ran out")

elif mode == "velocity":
    def one(i):
        try:
            c.chat("fake/chat", HI)
            return "ok"
        except VelocityLimited as e:
            return e.rule
    with ThreadPoolExecutor(3) as ex:
        out = list(ex.map(one, range(3)))
    for i, o in enumerate(out, 1):
        say(f"parallel call {i}: {o}")
    print(f"RESULT velocity_refusals={out.count('velocity')} ok={out.count('ok')}")

elif mode == "stream":
    stream = c.chat_stream("fake/chat", HI, max_tokens=200)
    t0, n = time.time(), 0
    for _ in stream:
        n += 1
        if n == 1:
            print("STREAMING", flush=True)  # demo.sh waits for this, then kills us
    say(f"stream ended after {n} events, {time.time() - t0:.1f}s, finished normally: {stream.finished}")
    try:
        c.chat("fake/chat", HI)
    except Killed as e:
        say(f"next request refused: {e.rule}")
        print(f"RESULT events={n} finished={stream.finished} rule={e.rule}")
    except AgentGateError as e:
        sys.exit(f"unexpected {e}")
else:
    sys.exit(__doc__)
