"""A tool-using agent whose tools misbehave: one is out of its scope, one returns an injected instruction.

Last line is `RESULT key=value ...`."""
from agentgate import ScopeDenied, ScreenedOutput
from _env import client, say

c = client()
with c.run() as run_id:
    say("tools it may see: " + ", ".join(sorted(t["name"] for t in c.tools_list("up"))))
    say("echo -> " + c.tool_call("up", "echo", {"text": "hello"})["content"][0]["text"])
    try:
        c.tool_call("up", "fail")
    except ScopeDenied as e:
        say(f"fail -> refused: {e.rule} (the upstream never saw it)")
        scope = e.rule
    try:
        c.tool_call("up", "inject")
    except ScreenedOutput as e:
        say(f"inject -> blocked: {e.rule} (the agent never read the text)")
        print(f"RESULT scope={scope} screen={e.rule} run={run_id}")
