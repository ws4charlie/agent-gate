"use strict";
// AgentGate Console. No framework: the page polls the console's JSON API and
// redraws. Everything from the API is escaped before it is put in the DOM.

const $ = (s, el = document) => el.querySelector(s);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
// Money on screen: cents above a dollar, four places below it. Exact values
// stay in the ledger; the limits form shows them unrounded.
const fmtUSD = (u) => {
  const n = Number(u && typeof u === "object" ? (u.nanos || 0) / 1e9 : u) || 0;
  return n === 0 ? "$0" : "$" + (Math.abs(n) >= 1 ? n.toFixed(2) : trimUSD(n.toFixed(4)));
};
function trimUSD(s) {
  s = String(s ?? "0");
  if (!s.includes(".")) return s;
  s = s.replace(/0+$/, "").replace(/\.$/, "");
  const [i, f = ""] = s.split(".");
  return f.length < 2 ? i + "." + f.padEnd(2, "0") : s;
}
const ago = (t) => {
  if (!t) return "never";
  const s = Math.max(0, (Date.now() - new Date(t)) / 1000);
  if (s < 60) return Math.floor(s) + "s ago";
  if (s < 3600) return Math.floor(s / 60) + "m ago";
  if (s < 86400) return Math.floor(s / 3600) + "h ago";
  return Math.floor(s / 86400) + "d ago";
};
const hhmmss = (t) => new Date(t).toLocaleTimeString([], { hour12: false });

// The console key arrives once in the link the console prints (#k=...). It is
// kept in this browser so a reload works, and taken out of the address bar.
let consoleKey = "";
try { consoleKey = localStorage.getItem("agentgate-console-key") || ""; } catch { /* storage off */ }
{
  const m = location.hash.match(/k=([A-Za-z0-9_-]{32,})/);
  if (m) {
    consoleKey = m[1];
    try { localStorage.setItem("agentgate-console-key", consoleKey); } catch { /* storage off */ }
    history.replaceState(null, "", location.pathname);
  }
}
// Without a working key the page asks for one: the user pastes the key (or the
// printed link) once, and it is kept like the one from the link.
let needKey = false;
function askForKey() {
  needKey = true;
  $("#clock").textContent = "console key needed";
  $(".dot").classList.add("off");
  $("#keymsg").textContent = consoleKey
    ? "The key saved in this browser no longer works. The console key was probably replaced."
    : "This browser has no console key yet.";
  if ($("#keygate").classList.contains("hide")) {
    $("#keygate").classList.remove("hide");
    $("#keyinput").focus();
  }
}
// The page cannot read the key file (that is the point of the key), so it can
// only copy the command that does.
$("#keycopy").addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText($("#keycmd").textContent);
    $("#keycopy").textContent = "Copied";
  } catch { $("#keycopy").textContent = "Select and copy it"; }
  setTimeout(() => { $("#keycopy").textContent = "Copy command"; }, 2000);
});
$("#keyform").addEventListener("submit", async (e) => {
  e.preventDefault();
  const raw = $("#keyinput").value.trim();
  const err = $("#keyerr");
  if (/^pbcopy\b/.test(raw)) {
    err.textContent = "That is the command, not the key. Run it in Terminal first, then paste again.";
    err.classList.remove("hide"); $("#keyinput").value = ""; return;
  }
  const m = raw.match(/(?:k=)?([A-Za-z0-9_-]{32,})\s*$/);
  if (!m) { err.textContent = "That does not look like a console key."; err.classList.remove("hide"); return; }
  const r = await fetch("/api/overview", { headers: { "X-Console-Key": m[1] } }).catch(() => null);
  if (!r || r.status === 401) {
    err.textContent = r ? "The console refused that key." : "The console is not reachable.";
    err.classList.remove("hide");
    return;
  }
  try { localStorage.setItem("agentgate-console-key", m[1]); } catch { /* storage off: works until reload */ }
  consoleKey = m[1];
  location.reload();
});

async function api(path, opts = {}) {
  const r = await fetch(path, { ...opts, headers: { "Content-Type": "application/json", "X-Console-Key": consoleKey, ...(opts.headers || {}) } });
  if (r.status === 401) { askForKey(); throw new Error("console key needed"); }
  if (r.status === 204) return null;
  const text = await r.text();
  let body;
  try { body = JSON.parse(text); } catch { body = text; }
  if (!r.ok) throw new Error((body && body.error) || text || r.statusText);
  return body;
}
// An address opens on its token transfers: payees and the wallet move USDC,
// not ETH, so the explorer's default tab would look empty.
const explorerURL = (ex, kind, v) => `${ex}/${kind}/${v}${kind === "address" ? "#tokentxns" : ""}`;

// Addresses and transaction hashes: short, linked to the block explorer in a
// new tab, with a button that copies the full value.
function chainLink(v) {
  const kind = v.length === 66 ? "tx" : "address";
  const shortV = v.slice(0, 6) + "…" + v.slice(-4);
  const ex = overview && overview.explorer;
  const text = ex
    ? `<a href="${esc(explorerURL(ex, kind, v))}" target="_blank" rel="noopener noreferrer" title="Open ${esc(v)} in the block explorer">${esc(shortV)}</a>`
    : `<span title="${esc(v)}">${esc(shortV)}</span>`;
  return `<span class="addr">${text}<button class="copy" data-copy="${esc(v)}" title="Copy the full ${kind === "tx" ? "transaction hash" : "address"}" aria-label="Copy">⧉</button></span>`;
}
// linkChain finds addresses and hashes in text that is already escaped.
const linkChain = (escaped) => escaped.replace(/\b0x[0-9a-fA-F]{64}\b|\b0x[0-9a-fA-F]{40}\b/g, (m) => chainLink(m));
document.addEventListener("click", async (e) => {
  const b = e.target.closest("[data-copy]");
  if (!b) return;
  e.preventDefault(); e.stopPropagation();
  try { await navigator.clipboard.writeText(b.dataset.copy); toast("copied"); } catch { toast("could not copy", true); }
});

function toast(msg, err = false) {
  const t = $("#toast");
  t.textContent = msg;
  t.className = "toast" + (err ? " err" : "");
  clearTimeout(toast.h);
  toast.h = setTimeout(() => t.classList.add("hide"), err ? 6000 : 2500);
}

// ---- routes ----
// The address says what is on screen, so a count can link to its rows and the
// back button works: #overview, #agents, #agents/<name>,
// #events?agent=<name>&class=<class>&since=24h, #tasks.
let tab = "overview", agentSel = "", evQuery = { agent: "", class: "", since: "" };
const evHref = (q) => "#events?" + new URLSearchParams(Object.entries(q).filter(([, v]) => v)).toString();
const agentHref = (name) => "#agents/" + encodeURIComponent(name);
document.querySelectorAll("#tabs button").forEach((b) => b.addEventListener("click", () => { location.hash = b.dataset.tab; }));
function route() {
  const h = location.hash.replace(/^#/, "");
  const [path, qs = ""] = h.split("?");
  const [name, sub = ""] = path.split("/");
  tab = ["overview", "agents", "tasks", "events"].includes(name) ? name : "overview";
  if (tab === "agents") agentSel = decodeURIComponent(sub);
  if (tab === "events") {
    const q = new URLSearchParams(qs);
    evQuery = { agent: q.get("agent") || "", class: q.get("class") || "", since: q.get("since") || "" };
  }
  document.querySelectorAll("#tabs button").forEach((b) => b.classList.toggle("on", b.dataset.tab === tab));
  document.querySelectorAll(".tab").forEach((s) => s.classList.toggle("on", s.id === tab));
  if (tab === "tasks") loadRunTab();
  if (tab === "events") { drawEventFilters(); loadEvents(); }
  if (tab === "agents" && overview) drawAgentsTab(overview);
  window.scrollTo(0, 0);
}
window.addEventListener("hashchange", route);

// ---- animated numbers ----
const shown = new Map();
function countTo(el, key, value, fmt) {
  const from = shown.has(key) ? shown.get(key) : 0;
  shown.set(key, value);
  if (from === value) { el.textContent = fmt(value); return; }
  const t0 = performance.now(), d = 700;
  const step = (t) => {
    const p = Math.min(1, (t - t0) / d), e = 1 - Math.pow(1 - p, 3);
    el.textContent = fmt(from + (value - from) * e);
    if (p < 1) requestAnimationFrame(step);
  };
  requestAnimationFrame(step);
}
const usdNum = (n) => "$" + (n < 1 ? n.toFixed(4) : n.toFixed(2));
const intNum = (n) => Math.round(n).toLocaleString();
const usdOf = (u) => u.nanos / 1e9;

// ---- data ----
let overview = null;
async function loadOverview() {
  try {
    overview = await api("/api/overview");
    $("#clock").textContent = "live · " + hhmmss(overview.at);
    $(".dot").classList.remove("off");
    drawOverview(overview);
    if (tab === "agents") drawAgentsTab(overview);
    fillAgentSelect(overview);
  } catch (e) {
    if (needKey) return;
    $("#clock").textContent = "console unreachable";
    $(".dot").classList.add("off");
  }
}

// Tiles keep their elements between polls so the numbers can count up.
// A tile with href is a link: every count opens the rows behind it.
function drawTiles(box, id, tiles) {
  const sig = tiles.map((t) => t.k + (t.href || "")).join("|");
  if (box.dataset.sig !== sig) {
    box.dataset.sig = sig;
    box.innerHTML = tiles.map((t, i) => {
      const tagName = t.href ? "a" : "div";
      return `<${tagName} class="tile${t.href ? " link" : ""}" ${t.href ? `href="${esc(t.href)}"` : ""} ${t.ext ? `target="_blank" rel="noopener noreferrer"` : ""} style="--glow:${t.g}">
        <div class="k">${esc(t.k)}</div><div class="v" id="${id}v${i}">0</div><div class="s" id="${id}s${i}"></div></${tagName}>`;
    }).join("");
  }
  tiles.forEach((t, i) => {
    $(`#${id}s${i}`).textContent = t.s || "";
    // A problem count takes its event colour once it is not zero.
    $(`#${id}v${i}`).className = "v" + (t.c && t.n > 0 ? " " + t.c : "");
    if (t.text !== undefined) $(`#${id}v${i}`).textContent = t.text;
    else countTo($(`#${id}v${i}`), id + i, t.n, t.f);
  });
}

function activityTiles(x, agent) {
  const q = (cls) => evHref({ agent, class: cls, since: "24h" });
  return [
    { k: "Calls", n: x.calls_24h, f: intNum, s: agent ? "every request" : x.active_agents + " active agents", g: "var(--ok)", href: agent ? "" : "#agents" },
    { k: "Refused", n: x.refused_24h, f: intNum, s: "said no on purpose", g: "var(--warn)", c: "warn", href: q("refused") },
    { k: "Errors", n: x.errors_24h, f: intNum, s: "upstream and internal", g: "var(--bad)", c: "bad", href: q("error") },
    { k: "Blocked", n: x.blocked_24h, f: intNum, s: "tool output screened", g: "var(--block)", c: "blk", href: q("blocked") },
  ];
}

// ---- overview tab: totals across every agent ----
function drawOverview(o) {
  const T = o.totals;
  const spend = [
    { k: "Today", n: usdOf(T.today), f: usdNum, s: "all agents", g: "var(--accent)" },
    { k: "7 days", n: usdOf(T.spend_7d), f: usdNum, s: "models, tools, payments", g: "var(--accent2)" },
  ];
  if (o.wallet) spend.push({ k: "Wallet USDC", text: o.wallet.usdc ? "$" + trimUSD(o.wallet.usdc) : "—",
    s: o.wallet.error ? o.wallet.error : short(o.wallet.address), g: "var(--pay)",
    href: o.explorer ? explorerURL(o.explorer, "address", o.wallet.address) : "", ext: true });
  drawTiles($("#ov-spend"), "ovs", spend);
  drawTiles($("#ov-activity"), "ova", activityTiles(T, ""));
  drawBars($("#ov-models"), o.models, "var(--model)");
  drawBars($("#ov-tools"), o.tools, "var(--tool)");
}

function drawBars(box, items, color) {
  const html = !items.length ? `<p class="muted">Nothing in the last 7 days.</p>` : (() => {
    const max = Math.max(...items.map((i) => i.spend.nanos), 1);
    return items.slice(0, 10).map((i) => `<div class="bar">
      <div class="lbl"><span>${esc(i.name)} <span class="muted">· ${i.calls} calls${i.kind === "payment" ? " · x402" : ""}</span></span><span>${esc(fmtUSD(i.spend))}</span></div>
      <div class="meter"><i style="width:${(i.spend.nanos / max) * 100}%;background:${i.kind === "payment" ? "var(--pay)" : color}"></i></div></div>`).join("");
  })();
  if (box.dataset.html !== html) { box.dataset.html = html; box.innerHTML = html; }
}

// ---- agents tab ----
const statusDot = (a) => `<span class="sdot ${a.status === "active" ? "ok" : "bad"}" title="${esc(a.status)}"></span>`;

function drawAgentsTab(o) {
  const a = o.agents.find((x) => x.name === agentSel);
  if (agentSel && !a) { agentSel = ""; }
  const pills = [`<a class="pill ${agentSel ? "" : "on"}" href="#agents">All agents</a>`,
    ...o.agents.map((x) => `<a class="pill ${x.name === agentSel ? "on" : ""}" href="${agentHref(x.name)}">${statusDot(x)}${esc(x.name)}</a>`)].join("");
  setHTML($("#agent-pills"), pills);
  if (a) drawAgentDetail(a); else drawAgentCards(o.agents);
}

// setHTML redraws only when something changed, so a poll does not disturb
// hover or focus.
function setHTML(el, html) { if (el.dataset.html !== html) { el.dataset.html = html; el.innerHTML = html; return true; } return false; }

function budgetLine(a) {
  const b = a.scope.daily_budget.nanos, pct = b ? Math.min(100, (a.today.nanos / b) * 100) : 0;
  return `<div class="row between"><span>Today <b>${esc(fmtUSD(a.today))}</b></span><span class="muted">${b ? "of " + esc(fmtUSD(a.scope.daily_budget)) + " daily" : "no daily cap"}</span></div>
    <div class="meter"><i class="${pct > 80 ? "hot" : ""}" style="width:${pct}%"></i></div>`;
}

// A card answers two questions: how much, and is anything wrong. Only counts
// that are not zero are shown, and each opens its events.
function problemLinks(a) {
  const parts = [`${intNum(a.calls_24h)} calls`];
  for (const [n, cls, word, color] of [[a.refused_24h, "refused", "refused", "warn"], [a.errors_24h, "error", "errors", "bad"], [a.blocked_24h, "blocked", "blocked", "blk"]]) {
    if (n) parts.push(`<a class="plink ${color}" href="${evHref({ agent: a.name, class: cls, since: "24h" })}">${intNum(n)} ${word}</a>`);
  }
  return parts.join(" · ");
}

function drawAgentCards(agents) {
  const html = agents.length ? `<div class="agents">${agents.map((a) => `
    <div class="agent ${a.status !== "active" ? "killed" : ""}" data-go="${esc(agentHref(a.name))}" tabindex="0">
      <div class="row between"><span class="name">${statusDot(a)}${esc(a.name)}</span>
        <span class="row">${a.console ? `<span class="chip acc">task</span>` : ""}${a.status !== "active" ? `<span class="chip bad">${esc(a.status)}</span>` : ""}<span class="chev">›</span></span></div>
      ${budgetLine(a)}
      <div class="sub">7 days ${esc(fmtUSD(a.spend_7d.total))} · ${problemLinks(a)}</div>
    </div>`).join("")}</div>` : `<p class="muted">No agents yet.</p>`;
  if (setHTML($("#agents-body"), html)) {
    $("#agents-body").querySelectorAll("[data-go]").forEach((el) => {
      const go = (e) => { if (!e.target.closest("a")) location.hash = el.dataset.go; };
      el.addEventListener("click", go);
      el.addEventListener("keydown", (e) => { if (e.key === "Enter") go(e); });
    });
  }
}

function spark(values, color, h = 38) {
  const w = 300, max = Math.max(...values, 1e-12);
  const pts = values.map((v, i) => [(i / (values.length - 1)) * w, h - 3 - (v / max) * (h - 8)]);
  const line = pts.map((p) => p.map((x) => x.toFixed(1)).join(",")).join(" ");
  const id = "g" + color.replace(/\W/g, "") + h;
  return `<svg class="spark" style="height:${h}px" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
    <defs><linearGradient id="${id}" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="${color}" stop-opacity=".45"/><stop offset="1" stop-color="${color}" stop-opacity="0"/></linearGradient></defs>
    <polygon fill="url(#${id})" points="0,${h} ${line} ${w},${h}"/>
    <polyline fill="none" stroke="${color}" stroke-width="1.6" points="${line}"/></svg>`;
}

const tagList = (items, cls, fmt = (x) => x) => items.length ? items.map((x) => `<span class="tag ${cls}">${esc(fmt(x))}</span>`).join("") : `<span class="muted">none</span>`;
const short = (p) => p.slice(0, 6) + "…" + p.slice(-4);

// Agents can share one ledger account; say so rather than show the same
// balance as if each had its own.
function balanceText(a) {
  const n = overview.agents.filter((x) => x.account_id === a.account_id).length;
  return n > 1 ? `account balance ${esc(fmtUSD(a.available))}, shared by ${n} agents` : `balance ${esc(fmtUSD(a.available))}`;
}

function drawAgentDetail(a) {
  const sc = a.scope, k = a.spend_7d, peak = Math.max(...a.hourly);
  const limit = (u, word) => (u.nanos ? `${esc(fmtUSD(u))} ${word}` : "");
  const limits = [limit(sc.daily_budget, "per day"), limit(sc.task_budget, "per task"), limit(sc.velocity_per_minute, "per minute"),
    sc.approval_threshold.nanos ? "approval above " + esc(fmtUSD(sc.approval_threshold)) : ""].filter(Boolean).join(" · ") || "no spending limits";
  // Actions sit next to what they act on: Kill beside the agent's name, Edit
  // beside what it is allowed to do (the one place limits are changed).
  const html = `
    <div class="card dhead">
      <div class="row wrap"><span class="name big">${statusDot(a)}${esc(a.name)}</span>
        ${a.console ? `<span class="chip acc">task</span>` : ""}${a.status !== "active" ? `<span class="chip bad">${esc(a.status)}</span>` : ""}
        <button class="ghost small" data-edit="${esc(a.id)}">Edit</button>
        ${a.status === "active" ? `<button class="btn danger small" data-kill="${esc(a.id)}">Kill</button>` : a.status === "killed" ? `<button class="btn go small" data-resume="${esc(a.id)}">Resume</button>` : ""}</div>
      <div class="muted">last seen ${esc(ago(a.last_seen))} · ${balanceText(a)}</div>
    </div>
    <div class="groups">
      <div class="group"><h2>Spend</h2><div class="card">${budgetLine(a)}
        <div class="row between sub"><span>7 days <b>${esc(fmtUSD(k.total))}</b></span>
          <span class="muted"><span style="color:var(--model)">■</span> model ${esc(fmtUSD(k.model))} <span style="color:var(--tool)">■</span> tool ${esc(fmtUSD(k.tool))} <span style="color:var(--pay)">■</span> pay ${esc(fmtUSD(k.payment))}</span></div></div></div>
      <div class="group"><h2>Activity · last 24 hours</h2><div class="tiles" id="ag-activity"></div></div>
    </div>
    <div class="card allowed"><div class="row"><h3>Allowed</h3><button class="ghost small" data-edit="${esc(a.id)}">Edit</button></div>
      <div class="arow"><span class="muted">Models</span><div class="tags">${tagList(sc.models, "")}</div></div>
      <div class="arow"><span class="muted">Tools</span><div class="tags">${tagList(sc.tools, "t")}</div></div>
      <div class="arow"><span class="muted">Payees</span><div class="tags">${sc.payees.length ? sc.payees.map((x) => `<span class="tag p">${chainLink(x)}</span>`).join("") : `<span class="muted">none</span>`}</div></div>
      <div class="arow"><span class="muted">Limits</span><div>${limits}</div></div>
    </div>
    <div class="card"><div class="row between"><h3>Spend per hour · last 24 hours</h3><span class="muted">peak ${esc(usdNum(peak))}/h</span></div>
      ${spark(a.hourly, a.status === "active" ? "#22d3ee" : "#f87171", 90)}
      <div class="row between axis"><span>24 hours ago</span><span>12 hours ago</span><span>now</span></div></div>
    <div class="card"><h3>Model spend · 7 days</h3><div id="ag-models" class="bars"></div></div>
    <div class="card"><h3>Tool &amp; payment spend · 7 days</h3><div id="ag-tools" class="bars"></div></div>`;
  const body = $("#agents-body");
  if (setHTML(body, html)) {
    body.querySelectorAll("[data-edit]").forEach((b) => b.addEventListener("click", () => editAgent(b.dataset.edit)));
    body.querySelectorAll("[data-kill]").forEach((b) => b.addEventListener("click", () => agentAction(b.dataset.kill, "kill")));
    body.querySelectorAll("[data-resume]").forEach((b) => b.addEventListener("click", () => agentAction(b.dataset.resume, "resume")));
  }
  drawTiles($("#ag-activity"), "aga", activityTiles(a, a.name));
  drawBars($("#ag-models"), a.models, "var(--model)");
  drawBars($("#ag-tools"), a.tools, "var(--tool)");
}

async function agentAction(id, what) {
  const a = overview.agents.find((x) => x.id === id);
  if (what === "kill" && !confirm(`Kill ${a.name}? Its next request is refused and calls in flight are cancelled.`)) return;
  try {
    await api(`/api/agents/${encodeURIComponent(id)}/${what}`, { method: "POST" });
    toast(`${a.name}: ${what === "kill" ? "killed" : "resumed"}`);
    loadOverview();
  } catch (e) { toast(e.message, true); }
}

function openModal(html) { $("#sheet").innerHTML = html; $("#modal").classList.remove("hide"); }
function closeModal() { $("#modal").classList.add("hide"); }
$("#modal").addEventListener("click", (e) => { if (e.target.id === "modal") closeModal(); });
const lines = (v) => v.split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
const usdField = (u) => (u.nanos ? trimUSD(u.usd) : "");

function editAgent(id) {
  const a = overview.agents.find((x) => x.id === id), sc = a.scope;
  openModal(`<h3>Edit ${esc(a.name)}</h3>
    <p class="muted" style="margin-top:-6px">Takes effect on the agent's next request. Empty amount = no limit. An empty allowlist allows nothing.</p>
    <div class="field"><label>Name${a.console ? " · named after its task" : " · its key keeps working after a rename"}</label><input id="e-name" value="${esc(a.name)}" ${a.console ? "disabled" : ""}></div>
    <div class="grid3">
      <div class="field"><label>Daily budget (USD)</label><input id="e-daily" value="${esc(usdField(sc.daily_budget))}" placeholder="no limit"></div>
      <div class="field"><label>Per-task budget (USD)</label><input id="e-task" value="${esc(usdField(sc.task_budget))}" placeholder="no limit"></div>
      <div class="field"><label>Per minute (USD)</label><input id="e-vel" value="${esc(usdField(sc.velocity_per_minute))}" placeholder="no limit"></div>
    </div>
    <div class="field"><label>Human approval above (USD, one request)</label><input id="e-appr" value="${esc(usdField(sc.approval_threshold))}" placeholder="never"></div>
    <div class="field"><label>Models (one per line; a trailing * is a prefix)</label><textarea id="e-models">${esc(sc.models.join("\n"))}</textarea></div>
    <div class="field"><label>Tools (server/tool for MCP, a name for a paid x402 tool)</label><textarea id="e-tools">${esc(sc.tools.join("\n"))}</textarea></div>
    <div class="field"><label>Payees (0x addresses)</label><textarea id="e-payees">${esc(sc.payees.join("\n"))}</textarea></div>
    ${a.console ? `<p class="muted">This agent belongs to a task. Editing the task sets these again.</p>` : ""}
    <div class="row between"><button class="ghost" id="e-cancel">Cancel</button><button class="btn" id="e-save">Save</button></div>`);
  $("#e-cancel").onclick = closeModal;
  $("#e-save").onclick = async () => {
    const body = { daily_budget_usd: $("#e-daily").value, task_budget_usd: $("#e-task").value, velocity_per_minute_usd: $("#e-vel").value,
      approval_threshold_usd: $("#e-appr").value, models: lines($("#e-models").value), tools: lines($("#e-tools").value), payees: lines($("#e-payees").value) };
    const name = $("#e-name").value.trim();
    try {
      if (!a.console && name !== a.name) {
        await api(`/api/agents/${encodeURIComponent(id)}/name`, { method: "PUT", body: JSON.stringify({ name }) });
        if (agentSel === a.name) history.replaceState(null, "", agentHref(name));
        agentSel = agentSel === a.name ? name : agentSel;
      }
      await api(`/api/agents/${encodeURIComponent(id)}/scope`, { method: "PUT", body: JSON.stringify(body) });
      closeModal(); toast(`${name}: saved`); loadOverview();
    } catch (e) { toast(e.message, true); }
  };
}

// ---- tasks tab ----
// A task is a reusable research job: its own instructions, model, tools and
// budgets. Each one runs as its own agent (console-<id>). The API still calls
// them templates.
let templates = [], runners = [], picked = null, watching = null;

async function loadRunTab() {
  try {
    const t = await api("/api/templates");
    templates = t.templates; runners = t.runners || [];
    if (!picked || !templates.find((x) => x.id === picked)) picked = templates[0] && templates[0].id;
    drawTemplates(); drawLauncher(); loadRunList();
  } catch (e) { toast(e.message, true); }
}

function pick(id) {
  if (id === picked) return;
  picked = id; watching = null; clearTimeout(runTimer);
  $("#runview").classList.add("hide");
  drawTemplates(); drawLauncher(); loadRunList();
}

const taskChip = (t) => t.edited ? `<span class="chip acc">edited</span>` : t.builtin ? `<span class="chip">built-in</span>` : "";
let dragging = null;
function drawTemplates() {
  $("#tmpl-list").innerHTML = templates.map((t) => `<button class="tmpl ${t.id === picked ? "on" : ""}" data-id="${esc(t.id)}" draggable="true" title="Drag to reorder">
    <div class="row between"><span class="n"><span class="grip" aria-hidden="true">⋮⋮</span>${esc(t.name)}</span>${taskChip(t)}</div>
    <div class="d">${esc(t.description || t.default_topic)}</div></button>`).join("");
  $("#tmpl-list").querySelectorAll(".tmpl").forEach((b) => {
    b.addEventListener("click", () => pick(b.dataset.id));
    // Drag to reorder: the list moves as you drag, and the order is saved on drop.
    b.addEventListener("dragstart", (e) => { dragging = b.dataset.id; b.classList.add("dragging"); e.dataTransfer.effectAllowed = "move"; });
    b.addEventListener("dragend", () => { b.classList.remove("dragging"); if (dragging) saveOrder(); dragging = null; });
    b.addEventListener("dragover", (e) => {
      e.preventDefault();
      if (!dragging || dragging === b.dataset.id) return;
      const from = templates.findIndex((x) => x.id === dragging), to = templates.findIndex((x) => x.id === b.dataset.id);
      const [moved] = templates.splice(from, 1);
      templates.splice(to, 0, moved);
      drawTemplates();
      $(`#tmpl-list [data-id="${CSS.escape(dragging)}"]`).classList.add("dragging");
    });
  });
}
async function saveOrder() {
  try { await api("/api/templates/order", { method: "PUT", body: JSON.stringify({ ids: templates.map((t) => t.id) }) }); }
  catch (e) { toast(e.message, true); }
}

function drawLauncher() {
  const t = templates.find((x) => x.id === picked);
  if (!t) { $("#launcher").innerHTML = `<p class="muted">No tasks yet. Press + New.</p>`; return; }
  const ready = overview ? overview.run : { enabled: true };
  const hasRunner = runners.includes(t.runner);
  $("#launcher").innerHTML = `<div class="row wrap tname"><h3>${esc(t.name)}</h3>
      <button class="btn go small" id="go" ${ready.enabled && hasRunner ? "" : "disabled"}>▶ Run</button>
      <button class="ghost small" id="t-other" ${ready.enabled && hasRunner ? "" : "disabled"} title="Run once with your own question; the task does not change">Run with a different question…</button>
      <button class="ghost small" id="t-edit">Edit</button>
      ${t.edited ? `<button class="ghost small" id="t-reset" title="Go back to the built-in version">Reset</button>` : t.builtin ? "" : `<button class="ghost small" id="t-del">Delete</button>`}</div>
    ${t.description ? `<p class="muted tdesc">${esc(t.description)}</p>` : ""}
    <div class="tags"><a class="tag a" href="${esc(agentHref(t.runner))}" title="This agent does the work. Its own budgets limit each run and each day.">run by ${esc(t.runner)}</a>${agentLimits(t.runner)}</div>
    <p class="asks"><span class="muted">Each run looks into</span> ${esc(t.default_topic)}</p>
    ${!ready.enabled ? `<p style="color:var(--warn)">${esc(ready.reason)}</p>`
      : !hasRunner ? `<p style="color:var(--warn)">No runner for ${esc(t.runner)} on this machine. Start the console with --runners ${esc(t.runner)}=…</p>` : ""}`;
  $("#go").onclick = () => startRun(t.id, t.default_topic);
  $("#t-other").onclick = () => askOnce(t);
  $("#t-edit").onclick = () => editTemplate(t);
  if (t.edited) $("#t-reset").onclick = () => resetTemplate(t);
  else if (!t.builtin) $("#t-del").onclick = () => deleteTemplate(t);
}

// The limits that apply to a run are the agent's own: shown so nobody wonders
// what stops a runaway task.
function agentLimits(name) {
  const a = overview && overview.agents.find((x) => x.name === name);
  if (!a) return "";
  const run = a.scope.task_budget, day = a.scope.daily_budget;
  return (run.nanos ? `<span class="tag p">run ≤ ${esc(fmtUSD(run))}</span>` : `<span class="tag warn" title="Set a per-task budget on the agent to cap each run">no per-run cap</span>`)
    + (day.nanos ? `<span class="tag p">day ≤ ${esc(fmtUSD(day))}</span>` : "");
}

$("#new-tmpl").addEventListener("click", () => editTemplate({ id: "", name: "", description: "", system_prompt: "", default_topic: "",
  runner: runners[0] || "", builtin: false }, true));

function editTemplate(t, fresh = false) {
  openModal(`<h3>${fresh ? "New task" : "Edit " + esc(t.name)}</h3>
    <p class="muted" style="margin-top:-6px">${t.builtin ? "This is a built-in task. Your changes replace it here; Reset brings the original back." : "What to look into, how, and which of your agents does it."}</p>
    <div class="grid3">
      <div class="field"><label>Id</label><input id="f-id" value="${esc(t.id)}" ${fresh ? "" : "disabled"} placeholder="my-topic"></div>
      <div class="field" style="grid-column: span 2"><label>Name</label><input id="f-name" value="${esc(t.name)}"></div>
    </div>
    <div class="field"><label>Description</label><input id="f-desc" value="${esc(t.description)}"></div>
    <div class="field"><label>Instructions (system prompt)</label><textarea id="f-sys" style="min-height:150px">${esc(t.system_prompt)}</textarea></div>
    <div class="field"><label>What a run looks into, unless you change it</label><input id="f-topic" value="${esc(t.default_topic)}"></div>
    <div class="field"><label>Run by · the agent's own budgets limit each run and each day</label><select id="f-runner">${
      [...new Set([...runners, t.runner].filter(Boolean))].map((n) => `<option ${n === t.runner ? "selected" : ""}>${esc(n)}</option>`).join("")}</select></div>
    <div class="row between"><button class="ghost" id="f-cancel">Cancel</button><button class="btn" id="f-save">Save</button></div>`);
  $("#f-cancel").onclick = closeModal;
  $("#f-save").onclick = async () => {
    const body = { id: $("#f-id").value.trim(), name: $("#f-name").value, description: $("#f-desc").value, system_prompt: $("#f-sys").value,
      default_topic: $("#f-topic").value, runner: $("#f-runner").value };
    try {
      const saved = await api("/api/templates", { method: "POST", body: JSON.stringify(body) });
      picked = saved.id; closeModal(); toast("saved"); loadRunTab();
    } catch (e) { toast(e.message, true); }
  };
}

async function deleteTemplate(t) {
  if (!confirm(`Delete the task ${t.name}? Its agent and past runs stay.`)) return;
  try { await api(`/api/templates/${encodeURIComponent(t.id)}`, { method: "DELETE" }); picked = null; toast("deleted"); loadRunTab(); }
  catch (e) { toast(e.message, true); }
}
async function resetTemplate(t) {
  if (!confirm(`Reset ${t.name} to the built-in version? Your changes to it are lost.`)) return;
  try { await api(`/api/templates/${encodeURIComponent(t.id)}`, { method: "DELETE" }); toast("reset"); loadRunTab(); }
  catch (e) { toast(e.message, true); }
}

// One run can look into something else; the task itself stays as it is.
function askOnce(t) {
  openModal(`<h3>${esc(t.name)} · one run</h3>
    <p class="muted" style="margin-top:-6px">Same instructions, tools and budget. Only this run looks into what you write here; the task does not change.</p>
    <div class="field"><label>This run looks into</label><textarea id="once-topic" rows="3">${esc(t.default_topic)}</textarea></div>
    <div class="row between"><button class="ghost" id="once-cancel">Cancel</button><button class="btn go" id="once-go">▶ Run</button></div>`);
  $("#once-topic").focus();
  $("#once-cancel").onclick = closeModal;
  $("#once-go").onclick = () => { const topic = $("#once-topic").value.trim(); if (!topic) return; closeModal(); startRun(t.id, topic); };
}

async function startRun(id, topic) {
  $("#go").disabled = true;
  try {
    const r = await api("/api/runs", { method: "POST", body: JSON.stringify({ template_id: id, topic }) });
    toast("run started");
    watch(r.id);
    loadRunList();
  } catch (e) { toast(e.message, true); }
  finally { const g = $("#go"); if (g) g.disabled = false; }
}

let runTimer = null, openFile = null, seenEvents = 0, stepsOpen = null;
const closedSteps = new Set();
function watch(id) {
  watching = id; openFile = null; seenEvents = 0; stepsOpen = null; closedSteps.clear();
  clearTimeout(runTimer);
  pollRun();
}
function closeRun() {
  watching = null; openFile = null; clearTimeout(runTimer);
  $("#runview").classList.add("hide");
  loadRunList();
}
async function pollRun() {
  if (!watching) return;
  try {
    const r = await api(`/api/runs/${watching}`);
    if (r.id !== watching) return;
    drawRun(r);
    if (r.status === "running") runTimer = setTimeout(pollRun, 1200);
    else { loadRunList(); if (!openFile && r.files.find((f) => f.name === "report.md")) showFile(r.id, "report.md"); }
  } catch (e) { toast(e.message, true); }
}

// What the gateway saw of a run, by kind: how many calls and what they cost.
function callSummary(steps) {
  const by = {};
  for (const s of steps) {
    const k = s.kind === "model" ? "model" : "tool";
    by[k] = by[k] || { n: 0, nanos: 0 };
    by[k].n++; by[k].nanos += s.cost.nanos;
  }
  return Object.entries(by).map(([k, v]) => `${v.n} ${k} ${v.n === 1 ? "call" : "calls"} ${esc(fmtUSD({ nanos: v.nanos }))}`).join(" · ");
}
const evLine = (e) => `<div class="ev ${esc(e.type)}${e.i >= seenEvents ? " new" : ""}"><span class="t">${esc(hhmmss(e.at))}</span>${linkChain(esc(e.text))}</div>`;

// What each saved file is, in words, for the buttons.
const fileInfo = {
  "report.md": ["Report", "The answer the task wrote"],
  "trace.jsonl": ["Trace", "Every message and tool call of the run, in order"],
  "log.txt": ["Log", "Everything the agent printed while it worked"],
};

function drawRun(r) {
  const running = r.status === "running";
  if (stepsOpen === null) stepsOpen = running;
  const nSteps = r.events.filter((e) => e.type === "step").length;
  const stepsHTML = r.events.map((e, i) => evLine({ ...e, i })).join("");
  const calls = r.steps.length ? `<div class="calls"><span class="muted">Through the gateway:</span> ${callSummary(r.steps)}</div>` : "";
  const gw = r.gateway_events.length ? `<h3 style="margin-top:14px">The gateway said no</h3><table class="tbl"><tbody>${r.gateway_events.map((e) =>
    `<tr><td class="nowrap">${esc(hhmmss(e.at))}</td><td><span class="chip ${e.class === "refused" ? "warn" : e.class === "blocked" ? "blk" : "bad"}">${esc(e.class)}</span></td><td>${esc(e.name)}</td><td>${esc(e.status)}</td><td class="muted">${linkChain(esc(e.detail))}</td></tr>`).join("")}</tbody></table>` : "";
  const files = r.files.length ? `<div class="files">${r.files.map((f) => {
      const [label, what] = fileInfo[f.name] || [f.name, ""];
      return `<button class="file ${openFile === f.name ? "on" : ""}" data-f="${esc(f.name)}" title="${esc(what)}">${esc(label)} <span class="muted">${(f.size / 1024).toFixed(1)} KB</span></button>`;
    }).join("")}
</div>
    <p class="muted fine">Saved only on this computer, readable only by your user. Nothing here is sent anywhere.</p>` : "";
  $("#runview").classList.remove("hide");
  $("#runview").innerHTML = `<div class="row between"><div><h3 style="margin:0">Run · ${esc(when(r.started_at))}</h3><div class="muted" style="font-size:12px">${esc(r.topic)}</div></div>
      <div class="row"><span class="status ${esc(r.status)}">${running ? `<span class="spinner"></span> ` : ""}${esc(r.status)}</span>
      <span class="chip">spent ${esc(fmtUSD(r.spend))}</span>${running ? `<button class="btn danger small" id="r-kill">Kill run</button>` : ""}
      <button class="ghost small" id="r-close" title="Close">✕</button></div></div>
    ${r.error ? `<p style="color:var(--warn)">${esc(r.error)}</p>` : ""}
    ${files}<div id="filebox"></div>
    <details class="steps" id="r-steps" ${stepsOpen ? "open" : ""}><summary>Steps <span class="muted">· ${nSteps}</span></summary>
      <div class="timeline">${stepsHTML}</div>${calls}</details>${gw}`;
  seenEvents = r.events.length;
  $("#r-close").onclick = closeRun;
  $("#r-steps").addEventListener("toggle", (e) => { stepsOpen = e.target.open; });
  $("#runview").querySelectorAll("details.step").forEach((d) => d.addEventListener("toggle", () => {
    if (d.open) closedSteps.delete(+d.dataset.g); else closedSteps.add(+d.dataset.g);
  }));
  if (running) $("#r-kill").onclick = async () => {
    try { await api(`/api/runs/${r.id}/kill`, { method: "POST" }); toast("run killed"); } catch (e) { toast(e.message, true); }
  };
  $("#runview").querySelectorAll("[data-f]").forEach((b) => b.addEventListener("click", () => {
    if (openFile === b.dataset.f) { openFile = null; $("#filebox").innerHTML = ""; b.classList.remove("on"); return; }
    showFile(r.id, b.dataset.f);
  }));
  if (openFile && fileCache[r.id + openFile]) { $("#filebox").innerHTML = fileCache[r.id + openFile]; bindDownload(r.id, openFile, fileText[r.id + openFile]); }
}

async function deleteRun(id) {
  if (!confirm("Delete this run's report, tool results and trace from this computer? Its cost stays in the gateway's records.")) return;
  try {
    await api(`/api/runs/${id}`, { method: "DELETE" });
    toast("run deleted");
    if (id === watching) closeRun(); else loadRunList();
  } catch (e) { toast(e.message, true); }
}

const fileCache = {}, fileText = {};
function bindDownload(id, name, text) {
  fileText[id + name] = text;
  const b = $("#filebox [data-dl]");
  if (!b) return;
  b.onclick = () => {
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
    a.download = id.slice(4, 12) + "-" + name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  };
}
async function showFile(id, name) {
  try {
    const r = await fetch(`/api/runs/${id}/files/${name}`, { headers: { "X-Console-Key": consoleKey } });
    const text = await r.text();
    if (!r.ok) throw new Error(text);
    if (id !== watching) return;
    openFile = name;
    const [label, what] = fileInfo[name] || [name, ""];
    const body = name.endsWith(".md") ? markdown(text) : `<pre>${esc(text)}</pre>`;
    fileCache[id + name] = `<div class="row between" style="margin-top:12px"><span><b>${esc(label)}</b> <span class="muted">· ${esc(what)}</span></span><button class="ghost small" data-dl="${esc(name)}">Download</button></div><div class="report">${body}</div>`;
    $("#filebox").innerHTML = fileCache[id + name];
    bindDownload(id, name, text);
    $("#runview").querySelectorAll("[data-f]").forEach((b) => b.classList.toggle("on", b.dataset.f === name));
  } catch (e) { toast(e.message, true); }
}

// A line-drawn bin; it takes the button's colour.
const trashIcon = `<svg viewBox="0 0 24 24" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M4 7h16"/><path d="M10 11v6M14 11v6"/><path d="M6 7l1 12a2 2 0 0 0 2 2h6a2 2 0 0 0 2-2l1-12"/><path d="M9 7V4h6v3"/></svg>`;

// The list shows the picked task's runs only, so it does not repeat the task's
// name; it shows what a run looked into only when that was changed. An open
// run unfolds right under its row; clicking the row again folds it.
async function loadRunList() {
  const t = templates.find((x) => x.id === picked);
  const list = $("#runlist"), view = $("#runview");
  list.after(view); // keep the run view out of the list while it is redrawn
  if (!t) { list.innerHTML = ""; return; }
  try {
    const { runs } = await api("/api/runs?template=" + encodeURIComponent(t.id));
    list.innerHTML = runs.length ? runs.map((r) => `<div class="runrow ${r.id === watching ? "on" : ""}" data-id="${esc(r.id)}">
      <span class="status ${esc(r.status)}">${esc(r.status)}</span><span class="muted">${esc(ago(r.started_at))}</span>
      <span class="topic">${r.topic !== t.default_topic ? esc(r.topic) : ""}</span><span>${esc(fmtUSD(r.spend))}</span>
      ${r.status === "running" ? "<span></span>" : `<button class="icon" data-del="${esc(r.id)}" title="Delete this run" aria-label="Delete this run">${trashIcon}</button>`}</div>`).join("")
      : `<p class="muted">No runs yet. Press ▶ Run.</p>`;
    list.querySelectorAll(".runrow").forEach((el) => el.addEventListener("click", (e) => {
      if (e.target.closest("[data-del]")) return;
      if (el.dataset.id === watching) { closeRun(); return; }
      watch(el.dataset.id); loadRunList();
    }));
    list.querySelectorAll("[data-del]").forEach((b) => b.addEventListener("click", () => deleteRun(b.dataset.del)));
    const open = watching && list.querySelector(`.runrow[data-id="${CSS.escape(watching)}"]`);
    if (open) open.after(view); else view.classList.add("hide");
  } catch (e) { toast(e.message, true); }
}

// A small Markdown renderer. The text is escaped first, so only the markup
// made here becomes HTML; links are kept only for http(s).
function markdown(src) {
  const inline = (s) => esc(s)
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*]+)\*\*/g, "<b>$1</b>")
    .replace(/\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)/g, (_, t, u) => `<a href="${u}" target="_blank" rel="noopener noreferrer">${t}</a>`)
    .replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, (_, p, u) => `${p}<a href="${u}" target="_blank" rel="noopener noreferrer">${u}</a>`);
  const out = []; let list = null, code = null;
  for (const line of src.split("\n")) {
    if (line.startsWith("```")) { if (code) { out.push(`<pre><code>${esc(code.join("\n"))}</code></pre>`); code = null; } else code = []; continue; }
    if (code) { code.push(line); continue; }
    const m = line.match(/^(#{1,4})\s+(.*)/), li = line.match(/^\s*(?:[-*]|\d+\.)\s+(.*)/);
    if (!li && list) { out.push(`</${list}>`); list = null; }
    if (m) out.push(`<h${Math.min(3, m[1].length)}>${inline(m[2])}</h${Math.min(3, m[1].length)}>`);
    else if (li) { const kind = /^\s*\d+\./.test(line) ? "ol" : "ul"; if (!list) { list = kind; out.push(`<${kind}>`); } out.push(`<li>${inline(li[1])}</li>`); }
    else if (line.trim()) out.push(`<p>${inline(line)}</p>`);
  }
  if (list) out.push(`</${list}>`);
  if (code) out.push(`<pre><code>${esc(code.join("\n"))}</code></pre>`);
  return out.join("\n");
}

// ---- events tab ----
let lastEvents = [];
function fillAgentSelect(o) {
  const html = `<option value="">All agents</option>` + o.agents.map((a) => `<option value="${esc(a.name)}">${esc(a.name)}</option>`).join("");
  if (setHTML($("#ev-agent"), html)) $("#ev-agent").value = evQuery.agent;
}
function drawEventFilters() {
  const sel = $("#ev-agent");
  if (evQuery.agent && ![...sel.options].some((o) => o.value === evQuery.agent)) sel.insertAdjacentHTML("beforeend", `<option value="${esc(evQuery.agent)}">${esc(evQuery.agent)}</option>`);
  sel.value = evQuery.agent;
  document.querySelectorAll("#ev-filters button").forEach((b) => b.classList.toggle("on", b.dataset.f === evQuery.class));
  const since = $("#ev-since");
  if (evQuery.since && ![...since.options].some((o) => o.value === evQuery.since)) since.insertAdjacentHTML("beforeend", `<option value="${esc(evQuery.since)}">Last ${esc(evQuery.since)}</option>`);
  since.value = evQuery.since;
}
$("#ev-agent").addEventListener("change", (e) => { location.hash = evHref({ ...evQuery, agent: e.target.value }); });
document.querySelectorAll("#ev-filters button").forEach((b) => b.addEventListener("click", () => { location.hash = evHref({ ...evQuery, class: b.dataset.f }); }));
$("#ev-since").addEventListener("change", (e) => { location.hash = evHref({ ...evQuery, since: e.target.value }); });

async function loadEvents() {
  try {
    const q = new URLSearchParams(Object.entries(evQuery).filter(([, v]) => v)).toString();
    const { events } = await api("/api/events" + (q ? "?" + q : ""));
    lastEvents = events;
    drawEvents();
  } catch (e) { /* the header shows the connection state */ }
}
// The badge counts problems in the last hour, whatever the filters say.
async function loadBadge() {
  try {
    const { events } = await api("/api/events?since=1h");
    const badge = $("#ev-badge");
    badge.textContent = events.length; badge.classList.toggle("hide", !events.length);
  } catch (e) { /* the header shows the connection state */ }
}
// The money in an event row: charged, refused before it was paid, or settled
// on chain before the gateway could record it.
function amountCell(e) {
  if (!e.amount || !e.amount.nanos) return `<span class="muted">—</span>`;
  const v = esc(fmtUSD(e.amount));
  if (e.amount_note === "not_paid") return `<span class="muted" title="The gateway refused it, so nothing was paid"><s>${v}</s> not paid</span>`;
  if (e.amount_note === "settled_late") return `${v} <span class="muted" title="Paid on chain; the gateway recorded it when it reconciled">paid</span>`;
  return v;
}
const when = (t) => new Date(t).toLocaleString([], { month: "numeric", day: "numeric", hour: "numeric", minute: "2-digit" });
function drawEvents() {
  setHTML($("#ev-body"), lastEvents.length ? lastEvents.map((e) => `<tr><td class="nowrap">${esc(when(e.at))}</td>
    <td>${e.agent ? `<a href="${esc(agentHref(e.agent))}">${esc(e.agent)}</a>` : `<span class="muted">—</span>`}</td>
    <td><span class="chip ${e.class === "refused" ? "warn" : e.class === "blocked" ? "blk" : e.class === "notice" ? "" : "bad"}">${esc(e.class)}</span></td>
    <td>${esc(e.kind)}</td><td>${esc(e.name)}</td><td class="nowrap">${amountCell(e)}</td><td>${esc(e.status)}</td><td class="muted">${linkChain(esc(e.detail))}</td></tr>`).join("")
    : `<tr><td colspan="8" class="muted">Nothing here. Good.</td></tr>`);
}

// ---- start ----
route();
loadOverview(); loadBadge();
setInterval(() => { if (tab === "overview" || tab === "agents") loadOverview(); }, 4000);
setInterval(() => { loadBadge(); if (tab === "events") loadEvents(); }, 10000);
