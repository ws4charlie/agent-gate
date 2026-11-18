# One GPT Researcher run for the AgentGate console. Every request carries the
# run id, so the gateway applies the agent's per-run budget and groups the
# spend under this run.
#
# With AG_SPIFFE_CERT set (the console fetched this run's SPIFFE certificate),
# every connection to the gateway uses mutual TLS with that certificate and no
# API key is sent. Without it, the run uses the agent's key as before.
#
# With AG_SPIFFE_SELF_FETCH=1 (the runner is its own unix user), the run fetches
# its own certificate from the SPIRE agent, checks it is the expected identity
# and keeps it in a private temporary directory that is deleted when the run
# ends, also on SIGTERM. There is no API key to fall back to.
import asyncio, atexit, os, re, shutil, signal, ssl, subprocess, sys, tempfile


class SelfFetchError(Exception):
    pass


def uri_sans(pem_file):
    """The URI subject alternative names of the first (leaf) certificate in the file.

    Uses `openssl x509 -noout -ext subjectAltName` when openssl is there, else
    Python's ssl._ssl._test_decode_cert (an internal helper; the only parser
    in the standard library).
    """
    if shutil.which("openssl"):
        r = subprocess.run(["openssl", "x509", "-noout", "-ext", "subjectAltName", "-in", pem_file],
                           capture_output=True, text=True)
        if r.returncode == 0:
            return re.findall(r"URI:([^\s,]+)", r.stdout)
        # openssl too old for -ext, or it cannot read the file: try Python
    try:
        info = ssl._ssl._test_decode_cert(pem_file)
    except Exception as e:  # noqa: BLE001
        raise SelfFetchError(f"cannot read the certificate: {e}") from e
    return [v for k, v in info.get("subjectAltName", ()) if k == "URI"]


def fetch_own_certificate(env, workdir):
    """Ask the SPIRE agent for this process's SVID and return (cert, key, bundle) paths.

    `spire-agent api fetch x509` writes svid.N.pem, svid.N.key and
    bundle.N.pem. The one used is the SVID whose only URI name is the expected
    ID. Raises SelfFetchError with the reason otherwise.
    """
    expect = env.get("AG_SPIFFE_EXPECT_ID", "")
    sock = env.get("AG_SPIFFE_SOCKET", "")
    if not expect or not sock:
        raise SelfFetchError("AG_SPIFFE_EXPECT_ID and AG_SPIFFE_SOCKET must be set")
    sock = sock[len("unix://"):] if sock.startswith("unix://") else sock
    agent = env.get("AG_SPIRE_AGENT_BIN") or "/opt/spire/bin/spire-agent"
    try:
        r = subprocess.run([agent, "api", "fetch", "x509", "-socketPath", sock, "-write", workdir],
                           capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired) as e:
        raise SelfFetchError(f"cannot run {agent}: {e}") from e
    if r.returncode != 0:
        raise SelfFetchError(f"{agent} failed: {(r.stderr or r.stdout).strip()[:300]}")
    seen = []
    for name in sorted(os.listdir(workdir)):
        m = re.fullmatch(r"svid\.(\d+)\.pem", name)
        if not m:
            continue
        n = m.group(1)
        cert = os.path.join(workdir, name)
        ids = uri_sans(cert)
        seen.append(ids)
        if ids == [expect]:
            return cert, os.path.join(workdir, f"svid.{n}.key"), os.path.join(workdir, f"bundle.{n}.pem")
    raise SelfFetchError(f"the SPIRE agent has no certificate for {expect} for this process (it has {seen or 'none'})")


_selfdir = None


def _remove_selfdir():
    global _selfdir
    d, _selfdir = _selfdir, None
    if d:
        shutil.rmtree(d, ignore_errors=True)


def self_fetch(env):
    """Fetch this run's certificate into a private temporary directory (mode
    0700) that is removed when the process ends. Returns (cert, key, bundle)."""
    global _selfdir
    _selfdir = tempfile.mkdtemp(prefix="agentgate-svid-")
    atexit.register(_remove_selfdir)
    return fetch_own_certificate(env, _selfdir)


def build_ssl_context(cert, key, bundle):
    """TLS settings for the gateway's mutual-TLS listener.

    Presents the run's certificate, trusts only the SPIFFE bundle (not the
    system's CAs) and requires the gateway to present a certificate. The name
    check is off: an SVID carries a SPIFFE URI, not a host name, and the
    connection goes to 127.0.0.1.
    """
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_REQUIRED
    ctx.load_verify_locations(cafile=bundle)
    ctx.load_cert_chain(cert, key)
    # Only our own bundle is trusted, so the strict X.509 profile (which some
    # SPIRE CA certificates do not meet) adds nothing here.
    strict = getattr(ssl, "VERIFY_X509_STRICT", 0)
    ctx.verify_flags &= ~strict
    return ctx


def context_from_env(env):
    """The SSL context for this run, or None when it uses the API key."""
    cert = env.get("AG_SPIFFE_CERT")
    if not cert:
        return None
    return build_ssl_context(cert, env["AG_SPIFFE_KEY"], env["AG_SPIFFE_BUNDLE"])


def _drop_credentials(request):
    # The certificate is the credential; the OpenAI client adds its API key anyway.
    for h in ("authorization", "x-api-key"):
        request.headers.pop(h, None)


async def _drop_credentials_async(request):
    _drop_credentials(request)


def patch_openai(openai, tags, ctx):
    """Every OpenAI client GPT Researcher builds, directly or through
    LangChain, sends the run headers (and the certificate, when there is one).

    Each async client also gets its own connection pool. LangChain shares one
    pool across all its clients, but GPT Researcher runs its MCP search on a
    second event loop in a thread and closes that loop afterwards. A later
    model call then picks up a connection from the closed loop and fails
    ("Event loop is closed"); GPT Researcher retries it, while the first
    request still runs to the end upstream, so that call is paid for twice.
    """
    import httpx
    for cls in (openai.OpenAI, openai.AsyncOpenAI):
        def patched(self, *a, _orig=cls.__init__, _cls=cls, **kw):
            kw["default_headers"] = {**(kw.get("default_headers") or {}), **tags}
            is_async = _cls is openai.AsyncOpenAI
            if ctx is not None:
                if is_async:  # its own client, so its own pool
                    kw["http_client"] = httpx.AsyncClient(verify=ctx, follow_redirects=True,
                                                          event_hooks={"request": [_drop_credentials_async]})
                else:
                    kw["http_client"] = httpx.Client(verify=ctx, follow_redirects=True,
                                                     event_hooks={"request": [_drop_credentials]})
            elif is_async:
                kw.pop("http_client", None)
            _orig(self, *a, **kw)
        cls.__init__ = patched


def patch_mcp(ctx):
    """Make the MCP connections to the gateway use the certificate.

    GPT Researcher cannot pass an httpx client to langchain_mcp_adapters, but
    the adapters build each streamable-HTTP client through one module-level
    factory; replace it. Each connection gets a fresh client, so its own pool.
    """
    import httpx
    from langchain_mcp_adapters import sessions
    if not hasattr(sessions, "create_mcp_http_client"):
        sys.exit("this langchain-mcp-adapters has no create_mcp_http_client to give the certificate to: refusing to run without it")

    def factory(headers=None, timeout=None, auth=None):
        headers = {k: v for k, v in (headers or {}).items() if k.lower() not in ("authorization", "x-api-key")}
        return httpx.AsyncClient(verify=ctx, headers=headers, auth=auth,
                                 timeout=timeout or httpx.Timeout(30.0, read=300.0))
    sessions.create_mcp_http_client = factory


def main():
    import openai

    # Without its MCP support GPT Researcher quietly falls back to a web search
    # service it has no key for, finds nothing, and still writes a "report".
    # Stop instead, so the run shows as failed with the reason.
    try:
        import langchain_mcp_adapters  # noqa: F401
    except ImportError:
        sys.exit("GPT Researcher cannot reach the gateway's search tools: install langchain-mcp-adapters in its Python")

    run = os.environ["AG_RUN_ID"]
    tags = {"X-AgentGate-Run": run, "X-AgentGate-Task": run}
    if os.environ.get("AG_SPIFFE_SELF_FETCH") == "1":
        try:
            cert, key, bundle = self_fetch(os.environ)
        except SelfFetchError as e:
            _remove_selfdir()
            sys.exit(f"could not get this run's SPIFFE certificate: {e}. The API key is off for this agent, so the run stops.")
        os.environ.update(AG_SPIFFE_CERT=cert, AG_SPIFFE_KEY=key, AG_SPIFFE_BUNDLE=bundle)
        print(f"▸ certificate fetched ({os.environ['AG_SPIFFE_EXPECT_ID']})", flush=True)
    ctx = context_from_env(os.environ)
    patch_openai(openai, tags, ctx)
    if ctx is not None:
        patch_mcp(ctx)

    from gpt_researcher import GPTResearcher

    async def go():
        mcp = os.environ["AG_GATEWAY_URL"] + "/mcp/"
        headers = dict(tags) if ctx is not None else {"Authorization": "Bearer " + os.environ["AGENT_KEY"], **tags}
        servers = os.environ.get("GPTR_MCP", "litescraper firecrawl").split()
        r = GPTResearcher(query=os.environ["AG_TOPIC"], report_type="research_report",
                          role=os.environ.get("AG_INSTRUCTIONS") or None,
                          mcp_configs=[{"name": n, "connection_url": mcp + n, "connection_headers": headers} for n in servers])
        # SIGTERM (the console's kill) cancels the work; the certificate is
        # deleted and the process leaves at once.
        me = asyncio.current_task()
        asyncio.get_running_loop().add_signal_handler(signal.SIGTERM, me.cancel)
        print("▸ researching", flush=True)
        await r.conduct_research()
        print("▸ writing the report", flush=True)
        report = await r.write_report()
        with open(os.path.join(os.environ["AG_RUN_DIR"], "report.md"), "w") as f:
            f.write(report)
        print(f"▸ report written ({len(report)} characters)", flush=True)

    try:
        asyncio.run(go())
    except asyncio.CancelledError:
        _remove_selfdir()
        print("▸ stopped", flush=True)
        os._exit(143)  # do not wait for the search threads


if __name__ == "__main__":
    main()
