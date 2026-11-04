"""The SDK against a real gateway (Go binaries, fakes, simulated chain)."""
import time

import pytest

from agentgate import (ApprovalRequired, BudgetExceeded, Duplicate, Killed, ScopeDenied, ScreenedOutput, VelocityLimited)

HI = [{"role": "user", "content": "hi"}]


def test_chat_and_duplicate(stack, make_client):
    _, key = stack.agent(models="fake/chat")
    c = make_client(key)
    assert c.chat("fake/chat", HI, idempotency_key="k1")["choices"]
    with pytest.raises(Duplicate) as e:
        c.chat("fake/chat", HI, idempotency_key="k1")
    assert e.value.original_request_id


def test_scope_and_budget(stack, make_client):
    _, key = stack.agent(models="fake/chat", daily="0.30")
    c = make_client(key)
    with pytest.raises(ScopeDenied) as e:
        c.chat("fake/chat-big", HI)
    assert e.value.rule == "scope_model"
    with pytest.raises(BudgetExceeded) as e:
        for _ in range(8):
            c.chat("fake/chat", HI)
    assert e.value.rule == "budget_daily"


def test_velocity(stack, make_client):
    # Spend is counted at the worst-case estimate (~$0.23) while a call is in flight, so
    # three calls at once against $0.50 a minute cannot all be admitted.
    from concurrent.futures import ThreadPoolExecutor
    _, key = stack.agent(models="fake/chat", velocity="0.50")
    c = make_client(key)

    def one(_):
        try:
            return c.chat("fake/chat", HI)
        except VelocityLimited as e:
            return e
    with ThreadPoolExecutor(3) as ex:
        out = list(ex.map(one, range(3)))
    assert sum(isinstance(o, VelocityLimited) for o in out) >= 1


def test_approval_then_replay(stack, make_client):
    _, key = stack.agent(models="fake/chat", approval_over="0.10")
    c = make_client(key)
    with pytest.raises(ApprovalRequired) as e:
        c.chat("fake/chat", HI, idempotency_key="big")
    assert e.value.id
    stack.cli("approve", "allow", e.value.id, "--by", "pytest")
    assert c.chat("fake/chat", HI, idempotency_key="big")["choices"]


def test_kill_cuts_a_stream(stack, make_client):
    name, key = stack.agent(models="fake/chat")
    c = make_client(key)
    s = c.chat_stream("fake/chat", HI, max_tokens=200)
    it = iter(s)
    assert [next(it) for _ in range(3)]
    start = time.time()
    stack.cli("kill", "--agent", name)
    rest = list(it)
    assert time.time() - start < 3 and len(rest) < 90 and not s.finished
    with pytest.raises(Killed):
        c.chat("fake/chat", HI)


def test_tools_screening_and_runs(stack, make_client):
    _, key = stack.agent(tools="up/echo,up/paid,up/inject,up/slow")
    c = make_client(key)
    names = {t["name"] for t in c.tools_list("up")}
    assert "echo" in names and "fail" not in names  # filtered by the allowlist
    with c.run() as run_id:
        assert c.tool_call("up", "echo", {"text": "hello"})["content"][0]["text"] == "hello"
        assert c.tool_call("up", "paid", {"text": "p"})
        with pytest.raises(ScopeDenied) as e:
            c.tool_call("up", "fail")
        assert e.value.rule == "scope_tool"
        with pytest.raises(ScreenedOutput) as e:
            c.tool_call("up", "inject")
        assert e.value.rule == "instruction_override"
        stack.cli("run", "kill", run_id)
        with pytest.raises(Killed) as e:
            c.tool_call("up", "echo", {"text": "x"})
        assert e.value.rule == "run_killed"


def test_pay(stack, make_client):
    _, key = stack.agent(models="*", tools="echo,premium", payees="0x70997970C51812dc3A010C7d01b50e0d17dc79C8", daily="0.30")
    c = make_client(key)
    r = c.pay("echo", {"text": "hello"})
    assert r["transaction"] and r["tool"] == "echo"
    with pytest.raises(BudgetExceeded):
        c.pay("premium", {"text": "x"})


def test_balance_follows_spending(stack, make_client):
    _, key = stack.agent(models="*", tools="echo", payees="0x70997970C51812dc3A010C7d01b50e0d17dc79C8", daily="0.30")
    c = make_client(key)
    before = c.balance()
    assert before["daily_budget_nanos"] == 300_000_000 and before["daily_spent_nanos"] == 0
    c.pay("echo", {"text": "hello"})
    after = c.balance()
    assert after["daily_spent_nanos"] == 10_000_000
    assert after["remaining_today_nanos"] == before["remaining_today_nanos"] - 10_000_000
    assert after["balance_nanos"] == before["balance_nanos"] - 10_000_000
