"""Typed errors. The class is chosen from the policy *rule id* the gateway
reports (``budget_daily``, ``velocity`` ...), never from the HTTP status: the
same status carries different rules, and an MCP refusal is HTTP 200."""

from __future__ import annotations


class AgentGateError(Exception):
    """Any refusal or failure the gateway reports. ``rule`` is the rule id or
    error code (``scope_model``, ``duplicate_request``, ...)."""

    def __init__(self, message: str, *, rule: str = "", status: int = 0, request_id: str = "", body: object = None):
        super().__init__(f"{rule or 'error'}: {message}" if message else rule)
        self.message, self.rule, self.status, self.request_id, self.body = message, rule, status, request_id, body


class BudgetExceeded(AgentGateError):
    """budget_daily or budget_task."""


class VelocityLimited(AgentGateError):
    """velocity: too much spend in the last 60 seconds."""


class ScopeDenied(AgentGateError):
    """scope_model, scope_tool or scope_payee: outside the agent's allowlist."""


class Killed(AgentGateError):
    """agent_killed, agent_revoked or run_killed."""


class ScreenedOutput(AgentGateError):
    """A tool result was blocked by the output screen (rule id in ``rule``). The tool ran and was charged."""


class Duplicate(AgentGateError):
    """The Idempotency-Key was already used; ``original_request_id`` names the first request."""

    @property
    def original_request_id(self) -> str:
        return (self.body or {}).get("original_request_id", "") if isinstance(self.body, dict) else ""


class ApprovalRequired(AgentGateError):
    """A human must approve. Replay the *same* request with the *same* Idempotency-Key after ``agentgate approve allow``."""

    def __init__(self, approval_id: str, **kw):
        super().__init__("a human must approve this request", rule="approval_required", **kw)
        self.id = approval_id


class SettlementPending(AgentGateError):
    """HTTP 202 from /v1/payments: the payment may have settled. It is neither success nor failure;
    the gateway resolves it from the chain. Do not retry with a new key."""

    def __init__(self, reason: str, **kw):
        super().__init__("the payment outcome is not known yet", rule="settlement_pending", **kw)
        self.reason = reason


_BY_RULE: dict[str, type[AgentGateError]] = {
    "budget_daily": BudgetExceeded, "budget_task": BudgetExceeded,
    "velocity": VelocityLimited,
    "scope_model": ScopeDenied, "scope_tool": ScopeDenied, "scope_payee": ScopeDenied,
    "agent_killed": Killed, "agent_revoked": Killed, "run_killed": Killed,
    "duplicate_request": Duplicate,
}


def from_rule(rule: str, message: str = "", **kw) -> AgentGateError:
    return _BY_RULE.get(rule, AgentGateError)(message, rule=rule, **kw)
