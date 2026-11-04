"""No gateway: the error mapping and the wire format, on httpx's mock transport."""
import json

import httpx
import pytest

from agentgate import (ApprovalRequired, BudgetExceeded, Client, Duplicate, Killed, ScopeDenied, ScreenedOutput,
                       SettlementPending, VelocityLimited)
from agentgate.errors import AgentGateError


def client(handler):
    return Client("http://gw", "ag_key", transport=httpx.MockTransport(handler))


def err(status, rule, **extra):
    return httpx.Response(status, json={"error": {"type": "policy_denied", "code": rule, "rule": rule, "message": "m", **extra}})


@pytest.mark.parametrize("status,rule,exc", [
    (429, "budget_daily", BudgetExceeded), (429, "budget_task", BudgetExceeded), (429, "velocity", VelocityLimited),
    (403, "scope_model", ScopeDenied), (403, "scope_tool", ScopeDenied), (403, "scope_payee", ScopeDenied),
    (403, "agent_killed", Killed), (403, "agent_revoked", Killed), (403, "run_killed", Killed),
    (409, "duplicate_request", Duplicate), (402, "insufficient_funds", AgentGateError)])
def test_class_comes_from_the_rule_not_the_status(status, rule, exc):
    # The same status carries different rules: only the rule id decides the class.
    with pytest.raises(exc) as e:
        client(lambda r: err(status, rule)).chat("m", [])
    assert type(e.value) is exc and e.value.rule == rule


def test_a_202_is_never_success():
    with pytest.raises(ApprovalRequired) as a:
        client(lambda r: httpx.Response(202, json={"status": "approval_required", "approval_id": "ap1"})).pay("echo")
    assert a.value.id == "ap1"
    with pytest.raises(SettlementPending) as s:
        client(lambda r: httpx.Response(202, json={"status": "settlement_pending", "reason": "http_500"})).pay("echo")
    assert s.value.reason == "http_500" and not isinstance(s.value, ApprovalRequired)


def test_tool_call_sends_the_2026_07_28_fields_and_maps_rpc_errors():
    seen = {}

    def handler(r):
        seen.update(headers=r.headers, body=json.loads(r.content), url=str(r.url))
        return httpx.Response(200, json={"jsonrpc": "2.0", "id": 1, "result": {"content": []}})
    with client(handler) as c, c.run("r1"):
        assert c.tool_call("up", "echo", {"text": "x"}, task="t1") == {"content": []}
    h, b = seen["headers"], seen["body"]
    assert (h["mcp-protocol-version"], h["mcp-method"], h["mcp-name"]) == ("2026-07-28", "tools/call", "echo")
    assert h["x-agentgate-run"] == "r1" and h["x-agentgate-task"] == "t1" and h["idempotency-key"]
    assert b["params"]["_meta"]["io.modelcontextprotocol/protocolVersion"] == "2026-07-28"
    assert seen["url"] == "http://gw/mcp/up"

    def rpc(code, data):
        return httpx.Response(200, json={"jsonrpc": "2.0", "id": 1, "error": {"code": code, "message": "m", "data": data}})
    with pytest.raises(ScopeDenied):
        client(lambda r: rpc(40301, {"rule": "scope_tool"})).tool_call("up", "x")
    with pytest.raises(ScreenedOutput) as s:
        client(lambda r: rpc(40302, {"rule": "instruction_override"})).tool_call("up", "x")
    assert s.value.rule == "instruction_override"
    with pytest.raises(ApprovalRequired):
        client(lambda r: rpc(40202, {"approval_id": "a"})).tool_call("up", "x")


def test_run_is_scoped_to_the_block():
    heads = []
    c = client(lambda r: (heads.append(r.headers.get("x-agentgate-run")), httpx.Response(200, json={}))[1])
    with c.run("r9"):
        c.chat("m", [])
    c.chat("m", [])
    assert heads == ["r9", None]


def test_balance_is_a_read_only_get_with_the_key():
    seen = {}

    def handler(r):
        seen["m"], seen["p"], seen["auth"] = r.method, r.url.path, r.headers["authorization"]
        return httpx.Response(200, json={"balance": "9.99", "available": "9.99", "daily_spent": "0.01"})
    b = client(handler).balance()
    assert (seen["m"], seen["p"], seen["auth"]) == ("GET", "/v1/balance", "Bearer ag_key")
    assert b["balance"] == "9.99"


def test_balance_refused_raises():
    with pytest.raises(AgentGateError):
        client(lambda r: httpx.Response(401, json={"error": {"code": "unauthorized", "message": "m"}})).balance()
