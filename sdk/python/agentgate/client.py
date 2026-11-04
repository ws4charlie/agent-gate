"""Synchronous client. One dependency (httpx). No retries: a retry is the caller's
decision, and the gateway's Idempotency-Key makes it safe to take."""

from __future__ import annotations

import contextlib
import itertools
import json
import uuid
from typing import Any, Iterator

import httpx

from .errors import (AgentGateError, ApprovalRequired, ScreenedOutput, SettlementPending, from_rule)

MCP_VERSION = "2026-07-28"  # the only revision the gateway speaks
_RPC_APPROVAL, _RPC_SCREENED = 40202, 40302


class Client:
    def __init__(self, base_url: str, key: str, *, timeout: float = 60.0, transport: httpx.BaseTransport | None = None):
        self.base_url = base_url.rstrip("/")
        self._http = httpx.Client(timeout=timeout, transport=transport, headers={"Authorization": f"Bearer {key}"})
        self._run: str | None = None
        self._ids = itertools.count(1)

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    # ---- runs -------------------------------------------------------------

    @contextlib.contextmanager
    def run(self, run_id: str | None = None) -> Iterator[str]:
        """Every call inside the block carries one run id (``X-AgentGate-Run``), so the
        run can be inspected or killed as a unit. Yields the id."""
        prev, self._run = self._run, run_id or f"run-{uuid.uuid4().hex[:12]}"
        try:
            yield self._run
        finally:
            self._run = prev

    # ---- model calls ------------------------------------------------------

    def chat(self, model: str, messages: list[dict], *, max_tokens: int = 100, idempotency_key: str | None = None,
             task: str | None = None, **params: Any) -> dict:
        """POST /v1/chat/completions. Returns the parsed response."""
        body = {"model": model, "messages": messages, "max_tokens": max_tokens, "stream": False, **params}
        r = self._http.post(self.base_url + "/v1/chat/completions", json=body, headers=self._headers(idempotency_key, task))
        return self._json(r)

    def chat_stream(self, model: str, messages: list[dict], *, max_tokens: int = 100, idempotency_key: str | None = None,
                    task: str | None = None, **params: Any) -> "Stream":
        """Streaming chat. Refusals raise here, before the first chunk. A stream the gateway cuts
        short (kill switch) simply ends: check ``stream.finished`` for whether ``[DONE]`` arrived."""
        body = {"model": model, "messages": messages, "max_tokens": max_tokens, "stream": True, **params}
        req = self._http.build_request("POST", self.base_url + "/v1/chat/completions", json=body, headers=self._headers(idempotency_key, task))
        r = self._http.send(req, stream=True)
        if r.status_code != 200:
            try:
                r.read()
                return self._json(r)  # raises
            finally:
                r.close()
        return Stream(r)

    # ---- tools ------------------------------------------------------------

    def tool_call(self, server: str, name: str, arguments: dict | None = None, *, idempotency_key: str | None = None,
                  task: str | None = None) -> dict:
        """MCP ``tools/call`` through /mcp/{server}. Returns the result object. The request carries the
        2026-07-28 ``_meta`` fields and the three mirror headers the gateway requires."""
        return self._rpc(server, "tools/call", {"name": name, "arguments": arguments or {}}, name, idempotency_key, task)

    def tools_list(self, server: str) -> list[dict]:
        """Only the tools the agent's allowlist permits."""
        return self._rpc(server, "tools/list", {}, None, None, None).get("tools", [])

    def _rpc(self, server, method, params, name, idem, task) -> dict:
        meta = {"io.modelcontextprotocol/protocolVersion": MCP_VERSION, "io.modelcontextprotocol/clientCapabilities": {}}
        body = {"jsonrpc": "2.0", "id": next(self._ids), "method": method, "params": {**params, "_meta": meta}}
        h = self._headers(idem, task) | {"MCP-Protocol-Version": MCP_VERSION, "Mcp-Method": method,
                                        "Accept": "application/json, text/event-stream"}
        if name:
            h["Mcp-Name"] = name
        r = self._http.post(f"{self.base_url}/mcp/{server}", json=body, headers=h)
        if r.headers.get("content-type", "").startswith("text/event-stream"):
            data = _last_sse_message(r.text)
        else:
            data = self._json(r, raise_http=r.status_code != 200 and r.status_code != 202)
        if isinstance(data, dict) and "error" in data:
            raise _rpc_error(data["error"], r)
        return data.get("result", {})

    # ---- payments ---------------------------------------------------------

    def pay(self, tool: str, input: dict | None = None, *, idempotency_key: str | None = None, task: str | None = None) -> dict:
        """POST /v1/payments: the gateway pays the named paid tool (x402) if policy allows.
        202 is never success: ``approval_required`` raises ApprovalRequired, ``settlement_pending``
        raises SettlementPending."""
        r = self._http.post(self.base_url + "/v1/payments", json={"tool": tool, "input": input or {}},
                            headers=self._headers(idempotency_key, task))
        return self._json(r)

    # ---- balance ----------------------------------------------------------

    def balance(self) -> dict:
        """GET /v1/balance: this agent's account ``balance``, ``held``, ``available`` and
        ``daily_spent`` (USD strings, with ``*_nanos`` integers). ``daily_budget`` and
        ``remaining_today`` are present only when the agent has a daily budget. Read-only; lets an
        agent plan inside its budget instead of finding the limit by being refused."""
        return self._json(self._http.get(self.base_url + "/v1/balance"))

    # ---- plumbing ---------------------------------------------------------

    def _headers(self, idem: str | None, task: str | None) -> dict[str, str]:
        # A fresh key per call unless the caller supplies one: replaying an approved request needs the original.
        h = {"Idempotency-Key": idem or uuid.uuid4().hex}
        if task:
            h["X-AgentGate-Task"] = task
        if self._run:
            h["X-AgentGate-Run"] = self._run
        return h

    def _json(self, r: httpx.Response, raise_http: bool = True) -> dict:
        try:
            body = r.json()
        except ValueError:
            body = {}
        rid = r.headers.get("X-Request-Id", "")
        if r.status_code == 202:
            if body.get("status") == "approval_required":
                raise ApprovalRequired(body.get("approval_id", ""), status=202, request_id=rid, body=body)
            if body.get("status") == "settlement_pending":
                raise SettlementPending(body.get("reason", ""), status=202, request_id=rid, body=body)
            raise AgentGateError("unexpected 202", rule=str(body.get("status", "accepted")), status=202, request_id=rid, body=body)
        if r.status_code >= 400 and raise_http:
            err = body.get("error", {}) if isinstance(body, dict) else {}
            rule = err.get("rule") or err.get("code") or f"http_{r.status_code}"
            raise from_rule(rule, err.get("message", ""), status=r.status_code, request_id=err.get("request_id", rid), body=err)
        return body


class Stream:
    """Iterate to get parsed SSE ``data:`` chunks. ``finished`` is True once ``[DONE]`` was seen."""

    def __init__(self, resp: httpx.Response):
        self._r, self.finished, self.request_id = resp, False, resp.headers.get("X-Request-Id", "")

    def __iter__(self) -> Iterator[dict]:
        try:
            for line in self._r.iter_lines():
                if not line.startswith("data:"):
                    continue
                data = line[5:].strip()
                if data == "[DONE]":
                    self.finished = True
                    return
                yield json.loads(data)
        except httpx.HTTPError:
            return  # cut by the gateway: what arrived was settled; finished stays False
        finally:
            self._r.close()


def _rpc_error(e: dict, r: httpx.Response) -> AgentGateError:
    data = e.get("data") or {}
    kw = {"status": r.status_code, "request_id": data.get("request_id", ""), "body": data}
    if e.get("code") == _RPC_APPROVAL:
        return ApprovalRequired(data.get("approval_id", ""), **kw)
    if e.get("code") == _RPC_SCREENED:
        return ScreenedOutput(e.get("message", ""), rule=data.get("rule", "screened"), **kw)
    return from_rule(data.get("rule") or data.get("code") or str(e.get("code")), e.get("message", ""), **kw)


def _last_sse_message(text: str) -> dict:
    """The response to a request is the last JSON-RPC message of its SSE stream (earlier ones are notifications)."""
    last: dict = {}
    for line in text.splitlines():
        if line.startswith("data:"):
            last = json.loads(line[5:].strip())
    return last
