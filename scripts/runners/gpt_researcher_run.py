# One GPT Researcher run for the AgentGate console. Every request carries the
# run id, so the gateway applies the agent's per-run budget and groups the
# spend under this run.
import asyncio, os, openai

run = os.environ["AG_RUN_ID"]
tags = {"X-AgentGate-Run": run, "X-AgentGate-Task": run}

# Every OpenAI client GPT Researcher builds, directly or through LangChain,
# sends the run headers.
for cls in (openai.OpenAI, openai.AsyncOpenAI):
    def patched(self, *a, _orig=cls.__init__, **kw):
        kw["default_headers"] = {**(kw.get("default_headers") or {}), **tags}
        _orig(self, *a, **kw)
    cls.__init__ = patched

from gpt_researcher import GPTResearcher

async def main():
    mcp = os.environ["AG_GATEWAY_URL"] + "/mcp/"
    headers = {"Authorization": "Bearer " + os.environ["AGENT_KEY"], **tags}
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

asyncio.run(main())
