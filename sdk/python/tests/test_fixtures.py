"""The SDK and the demo agent against a real gateway whose provider is the fixture mock (testdata/llm).
Proves the gateway's accounting for recorded prompts; says nothing about a real provider's behaviour."""
import os
import subprocess
import sys

from conftest import ROOT


def test_fixture_agent_balance_matches_the_bills(fx_stack):
    _, key = fx_stack.agent(models="*")
    env = {**fx_stack.env, "AG_URL": fx_stack.url, "AG_KEY": key, "PYTHONPATH": f"{ROOT / 'sdk' / 'python'}{os.pathsep}{ROOT / 'demo' / 'agents'}"}
    r = subprocess.run([sys.executable, str(ROOT / "demo" / "agents" / "fixtures_agent.py")], env=env, capture_output=True, text=True, timeout=120)
    assert r.returncode == 0, r.stdout + r.stderr
    last = r.stdout.strip().splitlines()[-1]
    assert last.startswith("RESULT requests=4 spent_nanos=308000 "), last  # 35200+82500+85800+104500 (0.000308 USD)


def test_unrecorded_prompt_is_a_loud_error(fx_stack):
    import pytest
    from agentgate import AgentGateError, Client
    _, key = fx_stack.agent(models="*")
    with Client(fx_stack.url, key) as c, pytest.raises(AgentGateError) as e:
        c.chat("mock/chat-small", [{"role": "user", "content": "a prompt nobody recorded"}])
    assert e.value.status >= 500, e.value  # a generic answer would have been a 200
