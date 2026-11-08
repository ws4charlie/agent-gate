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
      return `<${tagName} class="tile${t.href ? " link" : ""}" ${t.href ? `href="${esc(t.href)}"` : ""} style="--glow:${t.g}">
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
    s: o.wallet.error ? o.wallet.error : short(o.wallet.address), g: "var(--pay)" });
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
        <span class="row">${a.console ? `<span class="chip acc">template</span>` : ""}${a.status !== "active" ? `<span class="chip bad">${esc(a.status)}</span>` : ""}<span class="chev">›</span></span></div>
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
        ${a.console ? `<span class="chip acc">template</span>` : ""}${a.status !== "active" ? `<span class="chip bad">${esc(a.status)}</span>` : ""}
        ${a.status === "active" ? `<button class="btn danger small" data-kill="${esc(a.id)}">Kill</button>` : a.status === "killed" ? `<button class="btn go small" data-resume="${esc(a.id)}">Resume</button>` : ""}</div>
      <div class="muted">last seen ${esc(ago(a.last_seen))} · balance ${esc(fmtUSD(a.available))}</div>
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
      <div class="arow"><span class="muted">Payees</span><div class="tags">${tagList(sc.payees, "p", short)}</div></div>
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
  openModal(`<h3>${esc(a.name)} · limits and allowlists</h3>
    <p class="muted" style="margin-top:-6px">Takes effect on the agent's next request. Empty amount = no limit. An empty allowlist allows nothing.</p>
    <div class="grid3">
      <div class="field"><label>Daily budget (USD)</label><input id="e-daily" value="${esc(usdField(sc.daily_budget))}" placeholder="no limit"></div>
      <div class="field"><label>Per-task budget (USD)</label><input id="e-task" value="${esc(usdField(sc.task_budget))}" placeholder="no limit"></div>
      <div class="field"><label>Per minute (USD)</label><input id="e-vel" value="${esc(usdField(sc.velocity_per_minute))}" placeholder="no limit"></div>
    </div>
    <div class="field"><label>Human approval above (USD, one request)</label><input id="e-appr" value="${esc(usdField(sc.approval_threshold))}" placeholder="never"></div>
    <div class="field"><label>Models (one per line; a trailing * is a prefix)</label><textarea id="e-models">${esc(sc.models.join("\n"))}</textarea></div>
    <div class="field"><label>Tools (server/tool for MCP, a name for a paid x402 tool)</label><textarea id="e-tools">${esc(sc.tools.join("\n"))}</textarea></div>
    <div class="field"><label>Payees (0x addresses)</label><textarea id="e-payees">${esc(sc.payees.join("\n"))}</textarea></div>
    ${a.console ? `<p class="muted">This is a template agent: saving the template sets these again.</p>` : ""}
    <div class="row between"><button class="ghost" id="e-cancel">Cancel</button><button class="btn" id="e-save">Save</button></div>`);
  $("#e-cancel").onclick = closeModal;
  $("#e-save").onclick = async () => {
    const body = { daily_budget_usd: $("#e-daily").value, task_budget_usd: $("#e-task").value, velocity_per_minute_usd: $("#e-vel").value,
      approval_threshold_usd: $("#e-appr").value, models: lines($("#e-models").value), tools: lines($("#e-tools").value), payees: lines($("#e-payees").value) };
    try {
      await api(`/api/agents/${encodeURIComponent(id)}/scope`, { method: "PUT", body: JSON.stringify(body) });
      closeModal(); toast(`${a.name}: saved`); loadOverview();
    } catch (e) { toast(e.message, true); }
  };
}

// ---- tasks tab ----
// A task is a reusable research job: its own instructions, model, tools and
// budgets. Each one runs as its own agent (console-<id>). The API still calls
// them templates.
let templates = [], knownTools = [], defaultPayees = [], picked = null, watching = null;

async function loadRunTab() {
  try {
    const t = await api("/api/templates");
    templates = t.templates; knownTools = t.known_tools; defaultPayees = t.default_payees;
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
  const agent = "console-" + t.id;
  $("#launcher").innerHTML = `<div class="row wrap tname"><h3>${esc(t.name)}</h3>
      <button class="btn go small" id="go" ${ready.enabled ? "" : "disabled"}>▶ Run</button>
      <button class="ghost small" id="t-edit">Edit</button>
      ${t.edited ? `<button class="ghost small" id="t-reset" title="Go back to the built-in version">Reset</button>` : t.builtin ? "" : `<button class="ghost small" id="t-del">Delete</button>`}</div>
    ${t.description ? `<p class="muted tdesc">${esc(t.description)}</p>` : ""}
    <div class="tags"><span class="tag">${esc(t.model)}</span>${t.tools.map((x) => `<span class="tag t">${esc(x)}</span>`).join("")}
      <span class="tag p">run ≤ $${esc(trimUSD(t.run_budget_usd))}</span><span class="tag p">day ≤ $${esc(trimUSD(t.daily_budget_usd))}</span><span class="tag">${t.max_steps} steps</span>
      <a class="tag a" href="${esc(agentHref(agent))}" title="Every call goes through the gateway as this agent, under its budgets">agent ${esc(agent)}</a></div>
    <div class="field topic"><label for="topic">This run looks into</label><textarea id="topic" rows="2">${esc(t.default_topic)}</textarea>
      <div class="hint">The task's instructions stay the same. Change this to point one run at something else.</div></div>
    ${ready.enabled ? "" : `<p style="color:var(--warn)">${esc(ready.reason)}</p>`}`;
  $("#go").onclick = () => startRun(t.id);
  $("#t-edit").onclick = () => editTemplate(t);
  if (t.edited) $("#t-reset").onclick = () => resetTemplate(t);
  else if (!t.builtin) $("#t-del").onclick = () => deleteTemplate(t);
}

$("#new-tmpl").addEventListener("click", () => editTemplate({ id: "", name: "", description: "", system_prompt: "", default_topic: "",
  model: "openai/gpt-4o-mini", tools: ["litescraper/search"], payees: defaultPayees.slice(), run_budget_usd: "0.05", daily_budget_usd: "0.50", max_steps: 6, builtin: false }, true));

function editTemplate(t, fresh = false) {
  openModal(`<h3>${fresh ? "New task" : "Edit " + esc(t.name)}</h3>
    <p class="muted" style="margin-top:-6px">${t.builtin ? "This is a built-in task. Your changes replace it here; Reset brings the original back." : "Instructions plus the tools and budgets they may use. The task runs as its own agent."}</p>
    <div class="grid3">
      <div class="field"><label>Id</label><input id="f-id" value="${esc(t.id)}" ${fresh ? "" : "disabled"} placeholder="my-topic"></div>
      <div class="field" style="grid-column: span 2"><label>Name</label><input id="f-name" value="${esc(t.name)}"></div>
    </div>
    <div class="field"><label>Description</label><input id="f-desc" value="${esc(t.description)}"></div>
    <div class="field"><label>Instructions (system prompt)</label><textarea id="f-sys" style="min-height:150px">${esc(t.system_prompt)}</textarea></div>
    <div class="field"><label>What a run looks into, unless you change it</label><input id="f-topic" value="${esc(t.default_topic)}"></div>
    <div class="grid3">
      <div class="field"><label>Model</label><input id="f-model" value="${esc(t.model)}"></div>
      <div class="field"><label>Run budget (USD)</label><input id="f-run" value="${esc(t.run_budget_usd)}"></div>
      <div class="field"><label>Daily budget (USD)</label><input id="f-day" value="${esc(t.daily_budget_usd)}"></div>
    </div>
    <div class="grid3">
      <div class="field"><label>Max steps</label><input id="f-steps" type="number" min="1" max="20" value="${esc(t.max_steps)}"></div>
      <div class="field" style="grid-column: span 2"><label>Tools, one per line${knownTools.length ? " · known: " + esc(knownTools.join(", ")) : ""}</label><textarea id="f-tools" style="min-height:60px">${esc(t.tools.join("\n"))}</textarea></div>
    </div>
    <div class="field"><label>Payees for paid tools (0x addresses)</label><textarea id="f-payees" style="min-height:50px">${esc(t.payees.join("\n"))}</textarea></div>
    <div class="row between"><button class="ghost" id="f-cancel">Cancel</button><button class="btn" id="f-save">Save</button></div>`);
  $("#f-cancel").onclick = closeModal;
  $("#f-save").onclick = async () => {
    const body = { id: $("#f-id").value.trim(), name: $("#f-name").value, description: $("#f-desc").value, system_prompt: $("#f-sys").value,
      default_topic: $("#f-topic").value, model: $("#f-model").value, tools: lines($("#f-tools").value), payees: lines($("#f-payees").value),
      run_budget_usd: $("#f-run").value, daily_budget_usd: $("#f-day").value, max_steps: parseInt($("#f-steps").value, 10) || 0 };
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

async function startRun(id) {
  $("#go").disabled = true;
  try {
    const r = await api("/api/runs", { method: "POST", body: JSON.stringify({ template_id: id, topic: $("#topic").value }) });
    toast("run started");
    watch(r.id);
    loadRunList();
  } catch (e) { toast(e.message, true); }
  finally { const g = $("#go"); if (g) g.disabled = false; }
}

let runTimer = null, openFile = null, seenEvents = 0;
function watch(id) {
  watching = id; openFile = null; seenEvents = 0;
  clearTimeout(runTimer);
  pollRun();
}
async function pollRun() {
  if (!watching) return;
  try {
    const r = await api(`/api/runs/${watching}`);
    drawRun(r);
    if (r.status === "running") runTimer = setTimeout(pollRun, 1200);
    else { loadRunList(); if (!openFile && r.files.find((f) => f.name === "report.md")) showFile(r.id, "report.md"); }
  } catch (e) { toast(e.message, true); }
}

function drawRun(r) {
  const running = r.status === "running";
  const evs = r.events.map((e, i) => `<div class="ev ${esc(e.type)}${i >= seenEvents ? " new" : ""}"><span class="t">${esc(hhmmss(e.at))}</span>${esc(e.text)}</div>`).join("");
  const gw = r.gateway_events.length ? `<h3 style="margin-top:14px">Gateway said</h3><table class="tbl"><tbody>${r.gateway_events.map((e) =>
    `<tr><td>${esc(hhmmss(e.at))}</td><td><span class="chip ${e.class === "refused" ? "warn" : e.class === "blocked" ? "blk" : "bad"}">${esc(e.class)}</span></td><td>${esc(e.name)}</td><td>${esc(e.status)}</td><td class="muted">${esc(e.detail)}</td></tr>`).join("")}</tbody></table>` : "";
  const steps = r.steps.length ? `<div class="tags" style="margin-top:6px">${r.steps.map((s) =>
    `<span class="tag ${s.kind === "model" ? "" : s.kind === "payment" ? "p" : "t"}" title="${esc(s.status)}">${esc(s.kind)} ${esc(s.name || "…")} · ${esc(fmtUSD(s.cost))}${s.status !== "ok" && s.status !== "running" ? " · " + esc(s.status) : ""}</span>`).join("")}</div>` : "";
  $("#runview").classList.remove("hide");
  $("#runview").innerHTML = `<div class="row between"><div><h3 style="margin:0">Run · ${esc(when(r.started_at))}</h3><div class="muted" style="font-size:12px">${esc(r.topic)}</div></div>
      <div class="row"><span class="status ${esc(r.status)}">${running ? `<span class="spinner"></span> ` : ""}${esc(r.status)}</span>
      <span class="chip">spent ${esc(fmtUSD(r.spend))}</span>${running ? `<button class="btn danger small" id="r-kill">Kill run</button>` : ""}</div></div>
    ${r.error ? `<p style="color:var(--warn)">${esc(r.error)}</p>` : ""}
    <div class="timeline">${evs}</div>${steps}${gw}
    <div class="files">${r.files.map((f) => `<button class="file ${openFile === f.name ? "on" : ""}" data-f="${esc(f.name)}">📄 ${esc(f.name)} <span class="muted">${(f.size / 1024).toFixed(1)} KB</span></button>`).join("")}</div>
    <div id="filebox"></div>`;
  seenEvents = r.events.length;
  if (running) $("#r-kill").onclick = async () => {
    try { await api(`/api/runs/${r.id}/kill`, { method: "POST" }); toast("run killed"); } catch (e) { toast(e.message, true); }
  };
  $("#runview").querySelectorAll("[data-f]").forEach((b) => b.addEventListener("click", () => showFile(r.id, b.dataset.f)));
  if (openFile && fileCache[r.id + openFile]) { $("#filebox").innerHTML = fileCache[r.id + openFile]; bindDownload(r.id, openFile, fileText[r.id + openFile]); }
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
    openFile = name;
    const body = name.endsWith(".md") ? markdown(text) : `<pre>${esc(text)}</pre>`;
    fileCache[id + name] = `<div class="row between" style="margin-top:14px"><b>${esc(name)}</b><button class="ghost small" data-dl="${esc(name)}">download</button></div><div class="report">${body}</div>`;
    $("#filebox").innerHTML = fileCache[id + name];
    bindDownload(id, name, text);
    $("#runview").querySelectorAll("[data-f]").forEach((b) => b.classList.toggle("on", b.dataset.f === name));
  } catch (e) { toast(e.message, true); }
}

// The list shows the picked task's runs only, so it does not repeat the task's
// name; it shows what a run looked into only when that was changed.
async function loadRunList() {
  const t = templates.find((x) => x.id === picked);
  if (!t) { $("#runlist").innerHTML = ""; return; }
  try {
    const { runs } = await api("/api/runs?template=" + encodeURIComponent(t.id));
    $("#runlist").innerHTML = runs.length ? runs.map((r) => `<div class="runrow ${r.id === watching ? "on" : ""}" data-id="${esc(r.id)}">
      <span class="status ${esc(r.status)}">${esc(r.status)}</span><span class="muted">${esc(ago(r.started_at))}</span>
      <span class="topic">${r.topic !== t.default_topic ? esc(r.topic) : ""}</span><span>${esc(fmtUSD(r.spend))}</span></div>`).join("")
      : `<p class="muted">No runs yet. Press ▶ Run.</p>`;
    $("#runlist").querySelectorAll(".runrow").forEach((el) => el.addEventListener("click", () => { watch(el.dataset.id); loadRunList(); }));
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
  $("#ev-since").classList.toggle("hide", !evQuery.since);
  $("#ev-since").textContent = (evQuery.since === "24h" ? "last 24 hours" : "last " + evQuery.since) + " ✕";
}
$("#ev-agent").addEventListener("change", (e) => { location.hash = evHref({ ...evQuery, agent: e.target.value }); });
document.querySelectorAll("#ev-filters button").forEach((b) => b.addEventListener("click", () => { location.hash = evHref({ ...evQuery, class: b.dataset.f }); }));
$("#ev-since").addEventListener("click", () => { location.hash = evHref({ ...evQuery, since: "" }); });

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
const when = (t) => new Date(t).toLocaleString([], { month: "numeric", day: "numeric", hour: "numeric", minute: "2-digit" });
function drawEvents() {
  setHTML($("#ev-body"), lastEvents.length ? lastEvents.map((e) => `<tr><td class="nowrap">${esc(when(e.at))}</td>
    <td>${e.agent ? `<a href="${esc(agentHref(e.agent))}">${esc(e.agent)}</a>` : `<span class="muted">—</span>`}</td>
    <td><span class="chip ${e.class === "refused" ? "warn" : e.class === "blocked" ? "blk" : e.class === "notice" ? "" : "bad"}">${esc(e.class)}</span></td>
    <td>${esc(e.kind)}</td><td>${esc(e.name)}</td><td>${esc(e.status)}</td><td class="muted">${esc(e.detail)}</td></tr>`).join("")
    : `<tr><td colspan="7" class="muted">Nothing here. Good.</td></tr>`);
}

// ---- start ----
route();
loadOverview(); loadBadge();
setInterval(() => { if (tab === "overview" || tab === "agents") loadOverview(); }, 4000);
setInterval(() => { loadBadge(); if (tab === "events") loadEvents(); }, 10000);
