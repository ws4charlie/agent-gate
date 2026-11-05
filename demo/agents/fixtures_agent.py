"""A scripted agent that replays the chat-format fixtures (testdata/llm) through the SDK, then checks
the account balance fell by exactly what the fixtures bill. No LLM: the gateway talks to the fixture
mock at the HTTP boundary, so routing, SSE and usage parsing all run.

Needs AG_URL and AG_KEY (the gateway and an agent key), and FIXTURES (default testdata/llm).
Last line is `RESULT requests=N spent_nanos=N`. Exits non-zero if the balance is off by even one nano."""
import json
import os
import sys
import time
from decimal import Decimal
from pathlib import Path

from _env import client, say

MODEL = "mock/chat-small"
DIR = Path(os.environ.get("FIXTURES", Path(__file__).resolve().parents[2] / "testdata" / "llm"))
fx = {f["id"]: f for f in (json.loads(p.read_text()) for p in sorted(DIR.glob("*.json")))}


def nanos(f) -> int:
    n = Decimal(str(f["cost"])) * 10**9
    return int(n.to_integral_value(rounding="ROUND_CEILING"))


c = client()
before = c.balance()["balance_nanos"]
expected = 0


def user(text):
    return {"role": "user", "content": text}


with c.run() as run_id:
    # 1. a plain turn
    r = c.chat(MODEL, [user(fx["chat-hello"]["match"]["prompt"])])
    say("chat-hello -> " + r["choices"][0]["message"]["content"])
    expected += nanos(fx["chat-hello"])

    # 2. a streamed turn
    s = c.chat_stream(MODEL, [user(fx["chat-stream-summary"]["match"]["prompt"])])
    text = "".join((ch["choices"][0]["delta"].get("content") or "") for ch in s if ch.get("choices"))
    assert s.finished, "the stream did not end with [DONE]"
    say("chat-stream-summary -> " + text)
    expected += nanos(fx["chat-stream-summary"])

    # 3. a tool-call turn, then the tool result goes back and the model answers
    q = fx["chat-tool-call"]["match"]["prompt"]
    r = c.chat(MODEL, [user(q)])
    msg = r["choices"][0]["message"]
    call = msg["tool_calls"][0]
    say(f"chat-tool-call -> {call['function']['name']}({call['function']['arguments']})")
    expected += nanos(fx["chat-tool-call"])
    result = fx["chat-tool-result"]["match"]["prompt"]
    r = c.chat(MODEL, [user(q), msg, {"role": "tool", "tool_call_id": call["id"], "content": result}])
    say("chat-tool-result -> " + r["choices"][0]["message"]["content"])
    expected += nanos(fx["chat-tool-result"])

for _ in range(100):  # settlement can land just after the response ends
    after = c.balance()
    if after["held_nanos"] == 0:
        break
    time.sleep(0.05)
spent = before - after["balance_nanos"]
if spent != expected:
    sys.exit(f"balance fell by {spent} nanos, the fixtures bill {expected}")
print(f"RESULT requests=4 spent_nanos={spent} run={run_id}")
