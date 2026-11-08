"use strict";
// AgentGate Console. No framework: the page polls the console's JSON API and
// redraws. Everything from the API is escaped before it is put in the DOM.

const $ = (s, el = document) => el.querySelector(s);
const esc = (v) => String(v ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const fmtUSD = (u) => "$" + trimUSD(u && typeof u === "object" ? (u.usd || String((u.nanos || 0) / 1e9)) : u);
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
async function api(path, opts = {}) {
  const r = await fetch(path, { ...opts, headers: { "Content-Type": "application/json", "X-Console-Key": consoleKey, ...(opts.headers || {}) } });
  if (r.status === 401) { $("#clock").textContent = "open the link the console printed"; $(".dot").classList.add("off"); }
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

// ---- tabs ----
let tab = "overview";
document.querySelectorAll("#tabs button").forEach((b) => b.addEventListener("click", () => show(b.dataset.tab)));
function show(name) {
  tab = name;
  document.querySelectorAll("#tabs button").forEach((b) => b.classList.toggle("on", b.dataset.tab === name));
  document.querySelectorAll(".tab").forEach((s) => s.classList.toggle("on", s.id === name));
  if (name === "run") loadRunTab();
  if (name === "events") loadEvents();
}

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

// ---- overview ----
let overview = null;
async function loadOverview() {
  try {
    overview = await api("/api/overview");
    $("#clock").textContent = "live · " + hhmmss(overview.at);
    $(".dot").classList.remove("off");
    drawTiles(overview);
    drawAgents(overview);
    drawBars($("#models"), overview.models, "var(--model)");
    drawBars($("#tools"), overview.tools, "var(--tool)");
  } catch (e) {
    $("#clock").textContent = "console unreachable";
    $(".dot").classList.add("off");
  }
}

function drawTiles(o) {
  const T = o.totals;
  const tiles = [
    { k: "Spend today", n: T.today.nanos / 1e9, f: usdNum, s: "all agents, counted against budgets", g: "var(--accent)" },
    { k: "Spend · 7 days", n: T.spend_7d.nanos / 1e9, f: usdNum, s: "models + tools + payments", g: "var(--accent2)" },
    { k: "Calls · 24h", n: T.calls_24h, f: intNum, s: T.active_agents + " active agents", g: "var(--ok)" },
    { k: "Refused · 24h", n: T.refused_24h, f: intNum, s: "policy, budget, kill switch", g: "var(--warn)" },
    { k: "Errors · 24h", n: T.errors_24h, f: intNum, s: "upstream and internal", g: "var(--bad)" },
    { k: "Blocked · 24h", n: T.blocked_24h, f: intNum, s: "tool output screened out", g: "var(--block)" },
  ];
  if (o.wallet) tiles.push({ k: "Wallet USDC", text: o.wallet.usdc ? "$" + trimUSD(o.wallet.usdc) : "—",
    s: o.wallet.error ? o.wallet.error : o.wallet.address.slice(0, 6) + "…" + o.wallet.address.slice(-4) + " on chain", g: "var(--pay)" });
  const box = $("#tiles");
  if (box.children.length !== tiles.length) {
    box.innerHTML = tiles.map((t, i) => `<div class="tile" style="--glow:${t.g}"><div class="k">${esc(t.k)}</div><div class="v" id="tv${i}">0</div><div class="s" id="ts${i}"></div></div>`).join("");
  }
  tiles.forEach((t, i) => {
    $("#ts" + i).textContent = t.s;
    if (t.text !== undefined) $("#tv" + i).textContent = t.text;
    else countTo($("#tv" + i), "tile" + i, t.n, t.f);
  });
}

function spark(values, color) {
  const w = 300, h = 38, max = Math.max(...values, 1e-12);
  const pts = values.map((v, i) => [(i / (values.length - 1)) * w, h - 3 - (v / max) * (h - 8)]);
  const line = pts.map((p) => p.map((x) => x.toFixed(1)).join(",")).join(" ");
  const id = "g" + Math.random().toString(36).slice(2, 8);
  return `<svg class="spark" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none">
    <defs><linearGradient id="${id}" x1="0" x2="0" y1="0" y2="1"><stop offset="0" stop-color="${color}" stop-opacity=".45"/><stop offset="1" stop-color="${color}" stop-opacity="0"/></linearGradient></defs>
    <polygon fill="url(#${id})" points="0,${h} ${line} ${w},${h}"/>
    <polyline fill="none" stroke="${color}" stroke-width="1.6" points="${line}"/></svg>`;
}

function drawAgents(o) {
  const box = $("#agents");
  if (!o.agents.length) { box.innerHTML = `<p class="muted">No agents yet.</p>`; return; }
  box.innerHTML = o.agents.map((a) => {
    const sc = a.scope, budget = sc.daily_budget.nanos;
    const pct = budget ? Math.min(100, (a.today.nanos / budget) * 100) : 0;
    const k = a.spend_7d, tot = k.total.nanos || 1;
    const status = a.status === "active" ? `<span class="chip ok">active</span>` : `<span class="chip bad">${esc(a.status)}</span>`;
    const tags = [
      ...sc.models.map((m) => `<span class="tag">${esc(m)}</span>`),
      ...sc.tools.map((t) => `<span class="tag t">${esc(t)}</span>`),
      ...sc.payees.map((p) => `<span class="tag p">pay ${esc(p.slice(0, 6))}…${esc(p.slice(-4))}</span>`),
    ].join("") || `<span class="muted">nothing allowed</span>`;
    return `<div class="agent ${a.status !== "active" ? "killed" : ""}">
      <div class="row between"><span class="name">${esc(a.name)}</span><span class="row">${a.console ? `<span class="chip acc">template</span>` : ""}${status}</span></div>
      <div class="muted" style="font-size:12px">last seen ${esc(ago(a.last_seen))} · balance ${esc(fmtUSD(a.available))}</div>
      <div style="margin-top:10px" class="row between"><span>Today ${esc(fmtUSD(a.today))}</span><span class="muted">${budget ? "of " + esc(fmtUSD(sc.daily_budget)) + " daily" : "no daily cap"}</span></div>
      <div class="meter"><i class="${pct > 80 ? "hot" : ""}" style="width:${budget ? pct : 0}%"></i></div>
      <div class="kv">
        <div><b>${a.calls_24h}</b><span>calls 24h</span></div>
        <div><b style="color:var(--warn)">${a.refused_24h}</b><span>refused</span></div>
        <div><b style="color:var(--bad)">${a.errors_24h}</b><span>errors</span></div>
        <div><b style="color:var(--block)">${a.blocked_24h}</b><span>blocked</span></div>
      </div>
      <div class="row between" style="font-size:12px"><span>7 days ${esc(fmtUSD(k.total))}</span>
        <span class="muted"><span style="color:var(--model)">■</span> model ${esc(fmtUSD(k.model))} <span style="color:var(--tool)">■</span> tool ${esc(fmtUSD(k.tool))} <span style="color:var(--pay)">■</span> pay ${esc(fmtUSD(k.payment))}</span></div>
      <div class="stack"><i style="width:${(k.model.nanos / tot) * 100}%;background:var(--model)"></i><i style="width:${(k.tool.nanos / tot) * 100}%;background:var(--tool)"></i><i style="width:${(k.payment.nanos / tot) * 100}%;background:var(--pay)"></i></div>
      ${spark(a.hourly, a.status === "active" ? "#22d3ee" : "#f87171")}
      <div class="muted" style="font-size:11px">spend per hour, last 24h</div>
      <div class="tags">${tags}</div>
      <div class="actions">
        <button class="ghost small" data-edit="${esc(a.id)}">Limits &amp; allowlists</button>
        ${a.status === "active" ? `<button class="btn danger small" data-kill="${esc(a.id)}">Kill</button>` : a.status === "killed" ? `<button class="btn go small" data-resume="${esc(a.id)}">Resume</button>` : ""}
      </div></div>`;
  }).join("");
  box.querySelectorAll("[data-edit]").forEach((b) => b.addEventListener("click", () => editAgent(b.dataset.edit)));
  box.querySelectorAll("[data-kill]").forEach((b) => b.addEventListener("click", () => agentAction(b.dataset.kill, "kill")));
  box.querySelectorAll("[data-resume]").forEach((b) => b.addEventListener("click", () => agentAction(b.dataset.resume, "resume")));
}

function drawBars(box, items, color) {
  if (!items.length) { box.innerHTML = `<p class="muted">Nothing in the last 7 days.</p>`; return; }
  const max = Math.max(...items.map((i) => i.spend.nanos), 1);
  box.innerHTML = items.slice(0, 10).map((i) => `<div class="bar">
    <div class="lbl"><span>${esc(i.name)} <span class="muted">· ${i.calls} calls${i.kind === "payment" ? " · x402" : ""}</span></span><span>${esc(fmtUSD(i.spend))}</span></div>
    <div class="meter"><i style="width:${(i.spend.nanos / max) * 100}%;background:${i.kind === "payment" ? "var(--pay)" : color}"></i></div></div>`).join("");
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

// ---- run tab ----
let templates = [], knownTools = [], defaultPayees = [], picked = null, watching = null;

async function loadRunTab() {
  try {
    const t = await api("/api/templates");
    templates = t.templates; knownTools = t.known_tools; defaultPayees = t.default_payees;
    if (!picked || !templates.find((x) => x.id === picked)) picked = templates[0] && templates[0].id;
    drawTemplates(); drawLauncher(); loadRunList();
  } catch (e) { toast(e.message, true); }
}

function drawTemplates() {
  $("#tmpl-list").innerHTML = templates.map((t) => `<button class="tmpl ${t.id === picked ? "on" : ""}" data-id="${esc(t.id)}">
    <div class="row between"><span class="n">${esc(t.name)}</span>${t.builtin ? `<span class="chip">built-in</span>` : `<span class="chip acc">yours</span>`}</div>
    <div class="d">${esc(t.description || t.default_topic)}</div></button>`).join("");
  $("#tmpl-list").querySelectorAll(".tmpl").forEach((b) => b.addEventListener("click", () => { picked = b.dataset.id; drawTemplates(); drawLauncher(); }));
}

function drawLauncher() {
  const t = templates.find((x) => x.id === picked);
  if (!t) { $("#launcher").innerHTML = `<p class="muted">No templates.</p>`; return; }
  const ready = overview ? overview.run : { enabled: true };
  $("#launcher").innerHTML = `<div class="row between"><h3>${esc(t.name)}</h3>
      <span class="row"><button class="ghost small" id="t-edit">${t.builtin ? "Copy &amp; edit" : "Edit"}</button>${t.builtin ? "" : `<button class="ghost small" id="t-del">Delete</button>`}</span></div>
    <div class="tags" style="margin:-4px 0 12px"><span class="tag">${esc(t.model)}</span>${t.tools.map((x) => `<span class="tag t">${esc(x)}</span>`).join("")}
      <span class="tag p">run ≤ $${esc(trimUSD(t.run_budget_usd))}</span><span class="tag p">day ≤ $${esc(trimUSD(t.daily_budget_usd))}</span><span class="tag">${t.max_steps} steps</span></div>
    <div class="field"><label>Topic</label><textarea id="topic">${esc(t.default_topic)}</textarea></div>
    <div class="row between"><span class="muted">Runs as agent <b>console-${esc(t.id)}</b>. The gateway enforces the budgets.</span>
      <button class="btn go" id="go" ${ready.enabled ? "" : "disabled"}>▶ Run</button></div>
    ${ready.enabled ? "" : `<p class="muted" style="color:var(--warn)">${esc(ready.reason)}</p>`}`;
  $("#go").onclick = () => startRun(t.id);
  $("#t-edit").onclick = () => editTemplate(t);
  if (!t.builtin) $("#t-del").onclick = () => deleteTemplate(t);
}

$("#new-tmpl").addEventListener("click", () => editTemplate({ id: "", name: "", description: "", system_prompt: "", default_topic: "",
  model: "openai/gpt-4o-mini", tools: ["litescraper/search"], payees: defaultPayees.slice(), run_budget_usd: "0.05", daily_budget_usd: "0.50", max_steps: 6, builtin: false }, true));

function editTemplate(t, fresh = false) {
  const copy = t.builtin;
  const id = copy ? t.id + "-copy" : t.id;
  openModal(`<h3>${fresh ? "New template" : copy ? "Copy of " + esc(t.name) : "Edit " + esc(t.name)}</h3>
    <p class="muted" style="margin-top:-6px">Like a skill: a system prompt plus the tools and budgets it may use. It runs as its own agent.</p>
    <div class="grid3">
      <div class="field"><label>Id</label><input id="f-id" value="${esc(id)}" ${fresh || copy ? "" : "disabled"} placeholder="my-topic"></div>
      <div class="field" style="grid-column: span 2"><label>Name</label><input id="f-name" value="${esc(copy ? t.name + " (copy)" : t.name)}"></div>
    </div>
    <div class="field"><label>Description</label><input id="f-desc" value="${esc(t.description)}"></div>
    <div class="field"><label>System prompt</label><textarea id="f-sys" style="min-height:150px">${esc(t.system_prompt)}</textarea></div>
    <div class="field"><label>Default topic</label><input id="f-topic" value="${esc(t.default_topic)}"></div>
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
    <div class="row between"><button class="ghost" id="f-cancel">Cancel</button><button class="btn" id="f-save">Save template</button></div>`);
  $("#f-cancel").onclick = closeModal;
  $("#f-save").onclick = async () => {
    const body = { id: $("#f-id").value.trim(), name: $("#f-name").value, description: $("#f-desc").value, system_prompt: $("#f-sys").value,
      default_topic: $("#f-topic").value, model: $("#f-model").value, tools: lines($("#f-tools").value), payees: lines($("#f-payees").value),
      run_budget_usd: $("#f-run").value, daily_budget_usd: $("#f-day").value, max_steps: parseInt($("#f-steps").value, 10) || 0, builtin: false };
    try {
      const saved = await api("/api/templates", { method: "POST", body: JSON.stringify(body) });
      picked = saved.id; closeModal(); toast("template saved"); loadRunTab();
    } catch (e) { toast(e.message, true); }
  };
}

async function deleteTemplate(t) {
  if (!confirm(`Delete template ${t.name}? Its agent and past runs stay.`)) return;
  try { await api(`/api/templates/${encodeURIComponent(t.id)}`, { method: "DELETE" }); picked = null; toast("deleted"); loadRunTab(); }
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
  $("#runview").innerHTML = `<div class="row between"><div><h3 style="margin:0">${esc(r.template_name)}</h3><div class="muted" style="font-size:12px">${esc(r.topic)}</div></div>
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

async function loadRunList() {
  try {
    const { runs } = await api("/api/runs");
    $("#runlist").innerHTML = runs.length ? runs.map((r) => `<div class="runrow" data-id="${esc(r.id)}">
      <span class="status ${esc(r.status)}">${esc(r.status)}</span><span>${esc(r.template_name)} <span class="muted">· ${esc(r.topic.slice(0, 70))}</span></span>
      <span class="muted">${esc(ago(r.started_at))}</span><span>${esc(fmtUSD(r.spend))}</span></div>`).join("") : `<p class="muted">No runs yet.</p>`;
    $("#runlist").querySelectorAll(".runrow").forEach((el) => el.addEventListener("click", () => watch(el.dataset.id)));
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
let evFilter = "all", lastEvents = [];
document.querySelectorAll("#ev-filters button").forEach((b) => b.addEventListener("click", () => {
  evFilter = b.dataset.f;
  document.querySelectorAll("#ev-filters button").forEach((x) => x.classList.toggle("on", x === b));
  drawEvents();
}));
async function loadEvents() {
  try {
    const { events } = await api("/api/events");
    lastEvents = events;
    const recent = events.filter((e) => Date.now() - new Date(e.at) < 3600e3).length;
    const badge = $("#ev-badge");
    badge.textContent = recent; badge.classList.toggle("hide", !recent);
    if (tab === "events") drawEvents();
  } catch (e) { /* the overview shows the connection state */ }
}
function drawEvents() {
  const rows = lastEvents.filter((e) => evFilter === "all" || e.class === evFilter);
  $("#ev-body").innerHTML = rows.length ? rows.map((e) => `<tr><td>${esc(new Date(e.at).toLocaleString())}</td><td>${esc(e.agent)}</td>
    <td><span class="chip ${e.class === "refused" ? "warn" : e.class === "blocked" ? "blk" : e.class === "notice" ? "" : "bad"}">${esc(e.class)}</span></td>
    <td>${esc(e.kind)}</td><td>${esc(e.name)}</td><td>${esc(e.status)}</td><td class="muted">${esc(e.detail)}</td></tr>`).join("")
    : `<tr><td colspan="7" class="muted">Nothing here. Good.</td></tr>`;
}

// ---- start ----
loadOverview(); loadEvents();
setInterval(() => { if (tab === "overview") loadOverview(); }, 4000);
setInterval(loadEvents, 10000);
