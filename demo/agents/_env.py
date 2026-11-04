"""Shared by the demo agents: where the gateway is and who we are. No LLM anywhere:
the agents are scripts that drive the fake upstream, to show what the gateway does."""
import os
import sys

from agentgate import Client


def client() -> Client:
    try:
        return Client(os.environ["AG_URL"], os.environ["AG_KEY"])
    except KeyError as e:
        sys.exit(f"set {e.args[0]} (demo/demo.sh does)")


def say(msg: str) -> None:
    print(f"   {msg}", flush=True)


HI = [{"role": "user", "content": "keep going"}]
