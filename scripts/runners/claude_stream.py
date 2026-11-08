# Reads Claude Code's stream-json on stdin. Keeps the whole stream as
# trace.jsonl, prints one "▸ " progress line per tool call, and writes the
# final answer to report.md. Exits non-zero if Claude Code reported an error.
import json, os, sys

out = sys.argv[1]
ok = False
with open(os.path.join(out, "trace.jsonl"), "w") as trace:
    for line in sys.stdin:
        trace.write(line)
        try:
            e = json.loads(line)
        except ValueError:
            continue
        if e.get("type") == "assistant":
            for c in e.get("message", {}).get("content", []):
                if c.get("type") == "tool_use":
                    arg = c.get("input", {})
                    detail = arg.get("url") or arg.get("query") or arg.get("command") or arg.get("pattern") or arg.get("file_path") or ""
                    print(f"▸ {c['name']} {str(detail)[:160]}", flush=True)
        elif e.get("type") == "result":
            ok = not e.get("is_error")
            with open(os.path.join(out, "report.md"), "w") as f:
                f.write(e.get("result") or "")
            print(f"▸ report written ({len(e.get('result') or '')} characters, {e.get('num_turns')} turns)", flush=True)
sys.exit(0 if ok else 1)
