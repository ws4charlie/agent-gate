"""A real gateway on fakes: the repo's Go binaries, built once per session, a fake
upstream, a fake MCP server and a simulated chain. Needs `go` and DATABASE_URL
(a Postgres the tests may migrate and add accounts to; nothing is dropped).
Skips, unless AG_REQUIRE_DB=1 (CI), when either is missing."""

import json
import os
import re
import shutil
import socket
import subprocess
import time
import uuid
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
PAYEE = "0x70997970C51812dc3A010C7d01b50e0d17dc79C8"  # a public test address
# $1000 / $2000 per 1M tokens, so one call is real money against small budgets (as in demo/phase2-policy.sh).
ROW = {"input": 10**12, "output": 2 * 10**12, "cache_read": 0, "cache_write": 10**12}


def _port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _first_line(proc: subprocess.Popen) -> str:
    return proc.stdout.readline().strip()


class Stack:
    def __init__(self, work: Path):
        self.work, self.procs, self.env = work, [], dict(os.environ)
        self.ag = str(work / "agentgate")

    def start(self, cmd, **kw) -> subprocess.Popen:
        p = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=self.env, **kw)
        self.procs.append(p)
        return p

    def cli(self, *args) -> str:
        r = subprocess.run([self.ag, *args], env=self.env, capture_output=True, text=True, timeout=60)
        assert r.returncode == 0, f"agentgate {args}: {r.stderr}"
        return r.stdout.strip()

    def agent(self, **scope) -> tuple[str, str]:
        """Create an agent; scope keys are CLI flags (models='fake/chat', daily='0.30', ...). Returns (name, secret)."""
        name = "a-" + uuid.uuid4().hex[:8]
        args = ["agent", "create", "--account", self.acct, "--name", name]
        for k, v in scope.items():
            args += [f"--{k.replace('_', '-')}", str(v)]
        return name, self.cli(*args)

    def close(self):
        for p in self.procs:
            p.terminate()
        for p in self.procs:
            try:
                p.wait(5)
            except subprocess.TimeoutExpired:
                p.kill()


@pytest.fixture(scope="session")
def stack(tmp_path_factory):
    require = os.environ.get("AG_REQUIRE_DB") == "1"
    if not shutil.which("go") or not os.environ.get("DATABASE_URL"):
        msg = "need `go` and DATABASE_URL"
        pytest.fail(msg) if require else pytest.skip(msg)
    work = tmp_path_factory.mktemp("gw")
    for name in ("agentgate", "fakeupstream", "fakemcp"):
        subprocess.run(["go", "build", "-o", str(work / name), f"./cmd/{name}"], cwd=ROOT, check=True)
    s = Stack(work)
    try:
        # Long streams (100 chunks, 100 ms apart) so a kill lands mid-flight.
        up = s.start([str(work / "fakeupstream"), "-latency", "50ms", "-chunk-delay", "100ms", "-chunks", "100"])
        fake = _first_line(up)
        s.env["FAKE_API_KEY"] = "pytest-upstream-key"
        s.env["UPSTREAM_MCP_AUTH"] = "Bearer upstream-test-credential"
        mcp = s.start([str(work / "fakemcp"), "-key", s.env["UPSTREAM_MCP_AUTH"]])
        mcp_url = _first_line(mcp)

        (work / "providers.json").write_text(json.dumps({
            "providers": {"fake": {"kind": "openai-compatible", "base_url": fake + "/v1"}},
            "routes": [{"prefix": "fake/", "provider": "fake", "strip": True}]}))
        (work / "prices.json").write_text(json.dumps({"models": {"chat": ROW, "chat-big": ROW}}))
        (work / "mcp.json").write_text(json.dumps({"servers": {"up": {
            "url": mcp_url, "auth_env": "UPSTREAM_MCP_AUTH", "prices": {"paid": 5_000_000}}}}))

        # Simulated chain + paid tool + wallet (no network, key in a 0600 file).
        s.env["AG_WALLET_KEY_FILE"] = str(work / "wallet.key")
        wallet = s.cli("wallet", "new", "--file", s.env["AG_WALLET_KEY_FILE"]).split()[-1]
        tport = _port()
        s.start([s.ag, "paidtool", "--network", "sim", "--listen", f"127.0.0.1:{tport}", "--pay-to", PAYEE,
                 "--fund", "5", "--fund-address", wallet])
        (work / "payments.json").write_text(json.dumps({
            "network": "sim", "chain_query_url": f"http://127.0.0.1:{tport}",
            "tools": {"echo": f"http://127.0.0.1:{tport}/echo", "premium": f"http://127.0.0.1:{tport}/premium"}}))

        port = _port()
        s.env.update(AG_LISTEN=f"127.0.0.1:{port}", AG_PROVIDERS=str(work / "providers.json"), AG_PRICES=str(work / "prices.json"))
        s.cli("migrate")
        s.acct = s.cli("account", "create", "--name", "pytest-" + uuid.uuid4().hex[:6])
        s.cli("account", "deposit", "--account", s.acct, "--amount", "50")
        s.start([s.ag, "serve", "--mcp", str(work / "mcp.json"), "--payments", str(work / "payments.json"), "--reconcile-every", "1s"])
        s.url = f"http://127.0.0.1:{port}"
        import httpx
        for _ in range(100):
            try:
                if httpx.get(s.url + "/healthz", timeout=1).status_code == 200:
                    break
            except httpx.HTTPError:
                time.sleep(0.1)
        else:
            raise RuntimeError("gateway did not start")
        yield s
    finally:
        s.close()


@pytest.fixture
def make_client(stack):
    from agentgate import Client
    clients = []

    def make(secret):
        c = Client(stack.url, secret)
        clients.append(c)
        return c
    yield make
    for c in clients:
        c.close()


@pytest.fixture(scope="session")
def fx_stack(stack):
    """A second gateway whose only provider is the fixture mock (testdata/llm replayed over HTTP and
    SSE). Same binaries and database as `stack`; its own account, upstream and port. The key is a
    placeholder and every base URL is on 127.0.0.1: nothing here can leave the machine."""
    work = stack.work
    s = Stack(work)
    s.procs = []  # closed with the first stack's processes via its own list below
    up = s.start([str(work / "fakeupstream"), "-fixtures", str(ROOT / "testdata" / "llm")])
    fake = _first_line(up)
    providers = (ROOT / "configs" / "providers.mock.json").read_text().replace("127.0.0.1:18999", fake.removeprefix("http://"))
    assert fake.removeprefix("http://") in providers
    (work / "providers.fx.json").write_text(providers)
    port = _port()
    s.env.update(AG_LISTEN=f"127.0.0.1:{port}", AG_PROVIDERS=str(work / "providers.fx.json"),
                 AG_PRICES=str(ROOT / "configs" / "prices.mock.json"),
                 OPENROUTER_API_KEY="mock", ANTHROPIC_API_KEY="mock")
    s.acct = s.cli("account", "create", "--name", "fx-" + uuid.uuid4().hex[:6])
    s.cli("account", "deposit", "--account", s.acct, "--amount", "5")
    s.start([s.ag, "serve"])
    s.url = f"http://127.0.0.1:{port}"
    import httpx
    try:
        for _ in range(100):
            try:
                if httpx.get(s.url + "/healthz", timeout=1).status_code == 200:
                    break
            except httpx.HTTPError:
                time.sleep(0.1)
        else:
            raise RuntimeError("fixture gateway did not start")
        yield s
    finally:
        s.close()
