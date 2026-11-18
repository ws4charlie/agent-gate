# One GPT Researcher run for the AgentGate console. Every request carries the
# run id, so the gateway applies the agent's per-run budget and groups the
# spend under this run.
#
# With AG_SPIFFE_CERT set (the console fetched this run's SPIFFE certificate),
# every connection to the gateway uses mutual TLS with that certificate and no
# API key is sent. Without it, the run uses the agent's key as before.
import asyncio, os, ssl, sys


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
        print("▸ researching", flush=True)
        await r.conduct_research()
        print("▸ writing the report", flush=True)
        report = await r.write_report()
        with open(os.path.join(os.environ["AG_RUN_DIR"], "report.md"), "w") as f:
            f.write(report)
        print(f"▸ report written ({len(report)} characters)", flush=True)

    asyncio.run(go())


if __name__ == "__main__":
    main()
