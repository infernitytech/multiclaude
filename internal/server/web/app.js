"use strict";
// MultiClaude web UI — dependency-free, no build step.

// ---------- tiny DOM helpers ----------
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  if (attrs) for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k === "style" && typeof v === "object") Object.assign(el.style, v);
    else if (k === "html") el.innerHTML = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "value") el.value = v;
    else if (k === "checked" || k === "disabled" || k === "selected" || k === "autofocus") el[k] = !!v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  add(el, kids);
  return el;
}
function add(el, kids) {
  for (const k of kids) {
    if (k == null || k === false) continue;
    if (Array.isArray(k)) add(el, k);
    else el.appendChild(k instanceof Node ? k : document.createTextNode(String(k)));
  }
}
const $ = (s, r = document) => r.querySelector(s);
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
function link(href, ...kids) { return h("a", { href, onclick: e => { if (e.metaKey || e.ctrlKey) return; e.preventDefault(); go(href); } }, ...kids); }

function md(src) {
  if (!src) return "";
  const blocks = [];
  let s = String(src).replace(/```[^\n]*\n([\s\S]*?)```/g, (_, code) => { blocks.push(code); return `\u0000${blocks.length - 1}\u0000`; });
  s = esc(s);
  s = s.replace(/`([^`\n]+)`/g, "<code>$1</code>")
    .replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>")
    .replace(/(^|[\s(])\*([^*\n]+)\*/g, "$1<i>$2</i>")
    .replace(/\[([^\]]+)\]\((https?:[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>')
    .replace(/(^|\s)(https?:\/\/[^\s<]+)/g, '$1<a href="$2" target="_blank" rel="noopener">$2</a>');
  const out = [];
  let list = null;
  for (const line of s.split("\n")) {
    const m = line.match(/^\s*[-*] (.*)/) || line.match(/^\s*\d+\. (.*)/);
    if (m) { if (!list) { list = []; } list.push(`<li>${m[1]}</li>`); continue; }
    if (list) { out.push(`<ul>${list.join("")}</ul>`); list = null; }
    const hd = line.match(/^(#{1,3}) (.*)/);
    if (hd) out.push(`<h${hd[1].length}>${hd[2]}</h${hd[1].length}>`);
    else if (line.trim() === "") out.push("\n");
    else out.push(`<p>${line}</p>`);
  }
  if (list) out.push(`<ul>${list.join("")}</ul>`);
  return out.join("").replace(/\u0000(\d+)\u0000/g, (_, i) => `<pre><code>${esc(blocks[+i])}</code></pre>`);
}

function ago(ms) {
  if (!ms) return "never";
  const s = (Date.now() - ms) / 1000;
  if (s < 45) return "just now";
  if (s < 3600) return Math.round(s / 60) + "m ago";
  if (s < 86400) return Math.round(s / 3600) + "h ago";
  return Math.round(s / 86400) + "d ago";
}
function dur(ms) {
  const s = Math.round(ms / 1000);
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m " + (s % 60) + "s";
  return Math.floor(s / 3600) + "h " + Math.floor((s % 3600) / 60) + "m";
}
const money = v => "$" + (v || 0).toFixed(2);

function toast(msg, err) {
  const d = h("div", { class: err ? "err" : "" }, msg);
  $("#toast").appendChild(d);
  setTimeout(() => d.remove(), err ? 6000 : 2500);
}

async function api(method, path, body) {
  const r = await fetch(path, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined });
  if (r.status === 401 && path !== "/api/login") { showLogin(); throw new Error("not logged in"); }
  if (!r.ok) {
    let msg = r.statusText;
    try { msg = (await r.json()).error || msg; } catch {}
    toast(msg, true);
    throw new Error(msg);
  }
  if (r.status === 204) return null;
  const ct = r.headers.get("content-type") || "";
  return ct.includes("json") ? r.json() : r.text();
}

function debounce(fn, ms) { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; }
function copy(text) { navigator.clipboard?.writeText(text).then(() => toast("Copied")); }

// ---------- constants ----------
const STATUSES = [
  ["backlog", "Backlog"], ["todo", "To do"], ["in_progress", "In progress"], ["review", "Review"], ["blocked", "Blocked"], ["done", "Done"],
];
const STATUS_NAME = Object.fromEntries(STATUSES);
const GPU_CAPS = ["gpu", "cuda", "nvidia", "rocm", "mps"];
const PRIOS = ["Urgent", "High", "Medium", "Low"];
const STATUS_COLOR = { backlog: "var(--faint)", todo: "var(--muted)", in_progress: "var(--accent)", review: "var(--info)", blocked: "var(--bad)", done: "var(--ok)" };

// ---------- global state ----------
const S = { overview: { projects: [], nodes: [] }, view: null, drawer: null };
const projectBySlug = slug => S.overview.projects.find(p => p.slug === slug);
const nodeName = id => (S.overview.nodes.find(n => n.id === id) || {}).name || id;

async function loadOverview() {
  S.overview = await api("GET", "/api/overview");
  renderSidebar();
}

// ---------- router ----------
function go(path) { history.pushState(null, "", path); route(); }
window.addEventListener("popstate", () => route());

function route() {
  const url = new URL(location.href);
  const parts = url.pathname.split("/").filter(Boolean);
  closeDrawer(true);
  let view;
  if (parts[0] === "p" && parts[1]) {
    const slug = parts[1];
    const tab = parts[2] || "board";
    if (tab === "i" && parts[3]) { view = projectView(slug, "board"); setTimeout(() => openIssue(slug, +parts[3], true), 0); }
    else view = projectView(slug, tab, url.searchParams);
  } else if (parts[0] === "runs" && parts[1]) view = runView(parts[1]);
  else if (parts[0] === "machines") view = machinesView();
  else if (parts[0] === "questions") view = questionsView();
  else if (parts[0] === "account") view = accountView();
  else view = dashboardView();
  mount(view);
  renderSidebar();
}

function mount(view) {
  if (S.view && S.view.destroy) S.view.destroy();
  S.view = view;
  const main = $("#main");
  main.innerHTML = "";
  main.appendChild(view.el);
  view.load && view.load();
}

// ---------- sidebar ----------
function renderSidebar() {
  const path = location.pathname;
  const sb = $("#sidebar");
  sb.innerHTML = "";
  const online = S.overview.nodes.filter(n => n.online).length;
  const busy = S.overview.nodes.filter(n => n.online && n.active.length).length; // machines, not sessions
  const openQ = S.overview.open_questions || 0;
  document.title = (openQ ? `(${openQ}) ` : "") + "MultiClaude";
  add(sb, [
    link("/", h("div", { class: "brand" }, h("div", { class: "logo", html: `<svg width="16" height="16" viewBox="0 0 32 32"><circle cx="10" cy="11" r="4" fill="white"/><circle cx="22" cy="11" r="4" fill="white"/><circle cx="16" cy="22" r="4" fill="white"/></svg>` }), "MultiClaude")),
    h("div", { class: "nav" },
      link("/", "Overview"),
      link("/questions", "Questions", openQ ? h("span", { class: "count badge", title: "Agents are waiting for your answer" }, `${openQ} open`) : h("span", { class: "count" }, "")),
      link("/machines", "Machines", h("span", { class: "count" }, `${online} on · ${busy} busy`)),
      link("/account", "Users & account")),
    h("div", { class: "nav-section" }, "Projects"),
    h("div", { class: "nav" },
      S.overview.projects.map(p => {
        const c = p.counts || {};
        const active = (c.in_progress || 0) + (c.review || 0);
        const queued = c.queued ?? c.todo ?? 0;
        const parts = [active && `${active} active`, queued && `${queued} queued`].filter(Boolean);
        return link(`/p/${p.slug}`, p.name, h("span", { class: "count", title: "active = in progress or in review · queued = To do and reviews waiting for a reviewer" }, parts.join(" · ")));
      }),
      h("a", { href: "#", onclick: e => { e.preventDefault(); newProjectModal(); } }, "+ New project")),
    h("div", { class: "sidebar-foot" },
      h("div", { class: "row", style: { gap: "6px", marginBottom: "4px" } }, "👤 ", h("b", null, S.user ? S.user.username : ""),
        h("a", { href: "#", style: { marginLeft: "auto" }, onclick: async e => { e.preventDefault(); await api("POST", "/api/logout"); location.href = "/"; } }, "Log out")),
      h("span", null, `${S.overview.version || ""} · spent ${money(S.overview.total_cost_usd)}`),
      S.overview.latest_version ? h("div", null, h("a", { href: "https://github.com/infernitytech/multiclaude/releases", target: "_blank", rel: "noopener" }, `⬆ ${S.overview.latest_version} available`)) : null),
  ]);
  sb.querySelectorAll(".nav a").forEach(a => {
    const href = a.getAttribute("href");
    if (href === "/" ? path === "/" : path === href || path.startsWith(href + "/")) a.classList.add("active");
  });
}

// ---------- events (SSE) ----------
// Survives server restarts/updates: shows a banner while the server is away,
// reconnects, refreshes everything, and reloads the page if the server version changed.
let lostSince = 0;
function connectEvents() {
  const es = new EventSource("/api/events");
  const refreshOverview = debounce(loadOverview, 300);
  const banner = () => $("#conn-banner") || document.body.appendChild(h("div", { id: "conn-banner" }));
  es.onopen = async () => {
    if (!lostSince) return;
    lostSince = 0;
    $("#conn-banner")?.remove();
    const before = S.overview.version;
    try { await loadOverview(); } catch { return; }
    if (before && S.overview.version && before !== S.overview.version) { location.reload(); return; }
    toast("Reconnected");
    S.view && S.view.load && S.view.load();
  };
  es.onerror = () => {
    if (!lostSince) lostSince = Date.now();
    const b = banner();
    b.textContent = "⚠ Server unreachable (restarting or updating?) — reconnecting… Agents keep working and report back when it's up.";
    if (es.readyState === EventSource.CLOSED) { // e.g. a proxy answered 502: the browser won't retry on its own
      setTimeout(connectEvents, 3000);
    }
  };
  es.onmessage = e => {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }
    if (["nodes", "projects", "issues", "runs", "questions"].includes(ev.type)) refreshOverview();
    if (ev.type === "load") applyLoad(ev.data.node_id, ev.data.load);
    if (ev.type === "activity") applyActivity(ev);
    S.view && S.view.onEvent && S.view.onEvent(ev);
    S.drawer && S.drawer.onEvent && S.drawer.onEvent(ev);
  };
}

// ---------- talking to one running agent ----------
function agentMessageBox(runId, onSent) {
  const ta = h("textarea", { rows: 2, placeholder: "e.g. \"Use the existing auth middleware instead\" — Enter sends, Shift+Enter new line", style: { width: "100%" } });
  const send = async () => {
    const body = ta.value.trim();
    if (!body) return;
    ta.disabled = true;
    try {
      await api("POST", `/api/runs/${runId}/message`, { body });
      ta.value = "";
      toast("Sent — the agent sees it now");
      onSent && onSent();
    } finally { ta.disabled = false; }
  };
  ta.addEventListener("keydown", e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } });
  return h("div", { class: "row", style: { gap: "8px", alignItems: "flex-end" } }, ta, h("button", { class: "btn primary small", onclick: send }, "Send"));
}
// ---------- models ----------
const MODELS = ["opus", "sonnet", "haiku", "claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-2"];
function modelInput(value, placeholder, onchange) {
  const id = "models-dl";
  if (!document.getElementById(id)) document.body.appendChild(h("datalist", { id }, MODELS.map(m => h("option", { value: m }))));
  return h("input", { list: id, value: value || "", placeholder, onchange: e => onchange(e.target.value.trim()) });
}
// "claude-sonnet-5-5[1m]" → "sonnet 5.5"
const modelLabel = m => !m ? "" : m.replace(/^claude-/, "").replace(/\[.*\]$/, "").replace(/-(\d+)-(\d+)(-\d{8})?$/, " $1.$2");
const modelChip = m => m ? h("span", { class: "chip model", title: m }, modelLabel(m)) : null;
function switchModelModal(run, onDone) {
  let model = run.issue_model || "";
  const inp = modelInput(model, "model, e.g. opus, sonnet, haiku (empty = project default)", v => model = v);
  inp.oninput = e => model = e.target.value.trim();
  modal("Switch model", h("div", null,
    h("p", { class: "hint", style: { marginTop: 0 } }, `The agent on #${run.issue_number} runs ${run.model || "the default model"}. Switching stops this session — uncommitted work is saved to the branch as a WIP commit, no failed attempt is counted — and a new session continues the issue with the chosen model (it is remembered on the issue).`),
    inp),
    close => [h("button", { class: "btn", onclick: close }, "Cancel"),
      h("button", { class: "btn primary", onclick: async () => { await api("POST", `/api/runs/${run.id}/restart`, { model }); close(); toast(`Restarting with ${model || "the default model"}…`); onDone && onDone(); } }, "Restart with this model")]);
  setTimeout(() => inp.focus(), 0);
}
function messageAgentModal(runId, what, who) {
  let closeFn;
  const box = agentMessageBox(runId, () => closeFn && closeFn());
  modal(`Message ${who || "the agent"}`, h("div", null, h("p", { class: "hint", style: { marginTop: 0 } }, `Working on ${what}. Your message is injected into its live session (like typing into its chat).`), box,
    h("p", { class: "hint" }, link(`/runs/${runId}`, "Open the live view →"))),
    close => { closeFn = close; return [h("button", { class: "btn", onclick: close }, "Close")]; });
  setTimeout(() => box.querySelector("textarea").focus(), 0);
}

// ---------- modal ----------
function modal(title, body, actions) {
  const root = $("#modal-root");
  const close = () => { root.innerHTML = ""; document.removeEventListener("keydown", onKey); };
  const onKey = e => { if (e.key === "Escape") close(); };
  document.addEventListener("keydown", onKey);
  const bd = h("div", { class: "modal-backdrop", onmousedown: e => { if (e.target === bd) close(); } },
    h("div", { class: "modal" }, h("h2", null, title), body, h("div", { class: "row", style: { justifyContent: "flex-end", marginTop: "16px" } }, actions(close))));
  root.innerHTML = "";
  root.appendChild(bd);
  setTimeout(() => bd.querySelector("input,textarea")?.focus(), 0);
  return close;
}

function checkGroup(items, selected, onChange) {
  const sel = new Set(selected);
  const wrap = h("div", { class: "checks" });
  for (const [val, label] of items) {
    const lab = h("label", { class: sel.has(val) ? "on" : "" },
      h("input", { type: "checkbox", checked: sel.has(val), onchange: e => {
        e.target.checked ? sel.add(val) : sel.delete(val);
        lab.classList.toggle("on", e.target.checked);
        onChange && onChange(val, e.target.checked, [...sel]);
      } }), label);
    wrap.appendChild(lab);
  }
  wrap.values = () => [...sel];
  return wrap;
}

function newProjectModal() {
  const name = h("input", { placeholder: "My Game" });
  const slug = h("input", { placeholder: "my-game" });
  let slugTouched = false;
  name.oninput = () => { if (!slugTouched) slug.value = name.value.toLowerCase().trim().replace(/[^a-z0-9._-]+/g, "-").replace(/^-+|-+$/g, ""); };
  slug.oninput = () => slugTouched = true;
  const desc = h("textarea", { placeholder: "What is this project about? Agents read this." });
  const imp = h("input", { placeholder: "optional: owner/name on GitHub, or https://…/repo.git" });
  const tok = h("input", { type: "password", autocomplete: "off", placeholder: "default: the server-wide GitHub token" });
  const mirror = h("label", { class: "toggle" }, h("input", { type: "checkbox", checked: true }), "Keep it in sync both ways: work done here is pushed back, commits made there (e.g. merged pull requests) are pulled in");
  const importOpts = h("div", { style: { display: "none" } },
    h("label", { class: "field" }, h("span", null, "Access token (needed for private repositories and for pushing back)"), tok),
    h("div", { class: "hint", style: { margin: "-6px 0 10px" } }, "Leave empty to use the server-wide GitHub token (Users & account). Fine-grained token: Contents read & write on the repository."),
    h("div", { class: "field" }, mirror),
    h("div", { class: "hint", style: { margin: "4px 0 12px" } }, "The default branch is taken from the imported repository. More repositories (e.g. a private one) can be added after creating the project, under Settings → Repositories & mirrors."));
  imp.addEventListener("input", () => {
    importOpts.style.display = imp.value.trim() ? "" : "none";
    const m = imp.value.trim().match(/([\w.-]+?)(?:\.git)?\/?$/);
    if (m && !name.dataset.touched) { name.value = m[1]; name.dispatchEvent(new Event("input")); }
  });
  name.addEventListener("keydown", () => { name.dataset.touched = "1"; });
  const review = h("select", null,
    h("option", { value: "agent" }, "Agent review (reviewer machines review, then auto-merge)"),
    h("option", { value: "human" }, "Human review (you approve & merge)"),
    h("option", { value: "none" }, "No review (merge when the worker is done)"));
  const nodes = checkGroup(S.overview.nodes.map(n => [n.id, n.name]), S.overview.nodes.map(n => n.id));
  modal("New project", h("div", null,
    h("label", { class: "field" }, h("span", null, "Name"), name),
    h("label", { class: "field" }, h("span", null, "Repository slug (clone URL)"), slug),
    h("label", { class: "field" }, h("span", null, "Description"), desc),
    h("label", { class: "field" }, h("span", null, "Import existing git repository"), imp),
    importOpts,
    h("label", { class: "field" }, h("span", null, "Review mode"), review),
    h("div", { class: "field" }, h("span", { class: "hint" }, "Machines working on this project"),
      S.overview.nodes.length ? nodes : h("div", { class: "hint" }, "No machines registered yet — install a runner from the Machines page."))),
    close => [h("button", { class: "btn", onclick: close }, "Cancel"),
      h("button", { class: "btn primary", onclick: async e => {
        e.target.disabled = true;
        try {
          const p = await api("POST", "/api/projects", { name: name.value, slug: slug.value, description: desc.value, import_url: imp.value, import_token: tok.value, review_mode: review.value, nodes: nodes.values() });
          if (imp.value.trim() && mirror.querySelector("input").checked)
            await api("POST", `/api/projects/${p.slug}/repos/${p.slug}/mirrors`, { url: imp.value.trim(), token: tok.value, mode: "sync" }).catch(e => toast("Mirror not added: " + e.message, true));
          close(); await loadOverview(); go(`/p/${p.slug}`);
        } finally { e.target.disabled = false; }
      } }, "Create project")]);
}

// ---------- dashboard ----------
function dashboardView() {
  const content = h("div", { class: "content" });
  const el = h("div", { style: { display: "contents" } },
    h("div", { class: "topbar", style: { paddingBottom: "14px" } }, h("h1", null, "Overview"), h("div", { class: "spacer" }),
      h("button", { class: "btn primary", onclick: newProjectModal }, "+ New project")),
    content);
  function render() {
    const { projects, nodes } = S.overview;
    const runs = nodes.flatMap(n => n.active);
    content.innerHTML = "";
    add(content, [
      h("div", { class: "grid", style: { gridTemplateColumns: "repeat(auto-fill,minmax(180px,1fr))", marginBottom: "20px" } },
        stat("Projects", projects.length), stat("Machines online", `${nodes.filter(n => n.online).length} / ${nodes.length}`),
        stat("Agents running", runs.length), stat("Total spend (API-equiv.)", money(S.overview.total_cost_usd))),
      h("div", { class: "section-title" }, "Projects"),
      projects.length ? h("div", { class: "grid" }, projects.map(projectCard)) :
        h("div", { class: "card empty" }, "No projects yet. ", h("a", { href: "#", onclick: e => { e.preventDefault(); newProjectModal(); } }, "Create the first one.")),
      h("div", { class: "section-title" }, "Running agents"),
      runs.length ? runTable(runs, true) : h("div", { class: "card empty" }, nodes.length ? "All machines idle." : ["No machines yet — see ", link("/machines", "Machines"), "."]),
    ]);
  }
  const stat = (k, v) => h("div", { class: "card pad" }, h("div", { class: "hint" }, k), h("div", { class: "stat" }, v));
  return { el, load: async () => { await loadOverview(); render(); }, onEvent: debounce(ev => { if (["nodes", "projects", "issues", "runs"].includes(ev.type)) setTimeout(render, 400); }, 200) };
}

function projectCard(p) {
  const c = p.counts || {};
  const total = Object.values(c).reduce((a, b) => a + b, 0) || 1;
  return h("div", { class: "card pad", style: { cursor: "pointer" }, onclick: () => go(`/p/${p.slug}`) },
    h("div", { class: "row" }, h("b", { style: { fontSize: "15px" } }, p.name), h("span", { class: "spacer" }),
      !p.dispatch && h("span", { class: "chip warn" }, "paused")),
    h("div", { class: "hint", style: { margin: "4px 0 12px", minHeight: "18px" } }, (p.description || "").slice(0, 120)),
    h("div", { class: "progress" }, STATUSES.map(([s]) => c[s] ? h("span", { style: { width: (100 * c[s] / total) + "%", background: STATUS_COLOR[s] } }) : null)),
    h("div", { class: "row wrap", style: { marginTop: "10px", gap: "6px" } },
      STATUSES.filter(([s]) => c[s]).map(([s, n]) => h("span", { class: "chip" }, `${n} ${c[s]}`))));
}

function runTable(runs, showProject) {
  const live = runs.some(r => r.status === "running");
  return h("table", { class: "list" },
    h("tr", null, h("th", null, ""), h("th", null, "Kind"), h("th", null, "Task"), live && h("th", null, "Now"), showProject && h("th", null, "Project"), h("th", null, "Machine"), h("th", null, "Started"), h("th", null, "Duration"), h("th", null, "Cost")),
    runs.map(r => h("tr", { class: "click", onclick: () => go(`/runs/${r.id}`) },
      h("td", null, runDot(r)),
      h("td", null, h("span", { class: "chip " + (r.kind === "review" ? "req" : r.kind === "chat" ? "accent" : "") }, r.kind)),
      h("td", null, r.issue_number ? `#${r.issue_number} ${r.issue_title}` : h("span", { class: "hint" }, "chat session")),
      live && h("td", { class: "now-cell" }, r.status === "running" ? h("span", { class: "agent-now", "data-act": r.id }, actText(ACT[r.id] || r.activity)) : null),
      showProject && h("td", null, r.project_slug),
      h("td", null, r.node_name),
      h("td", { class: "hint" }, ago(r.started_at)),
      h("td", { class: "hint" }, dur((r.ended_at || Date.now()) - r.started_at)),
      h("td", { class: "hint" }, money(r.cost_usd)))));
}
function runDot(r) {
  const cls = r.status === "running" ? "busy" : r.status === "succeeded" ? "on" : "";
  return h("span", { class: "row", style: { gap: "6px" } }, h("span", { class: "dot " + cls, style: r.status === "failed" || r.status === "lost" ? { background: "var(--bad)" } : null }), h("span", { class: "hint" }, r.status));
}

// ---------- project shell ----------
function projectView(slug, tab, params) {
  const tabs = [["board", "Board"], ["list", "List"], ["archive", "Archive"], ["chat", "Chat"], ["runs", "Runs"], ["artifacts", "Artifacts"], ["code", "Code"], ["settings", "Settings"]];
  const head = h("div", { class: "topbar" });
  const body = h("div", { class: "content flush" });
  const el = h("div", { style: { display: "contents" } }, head, body);
  let inner;
  const views = { board: boardView, list: (i, p) => listView(i, p, false), archive: (i, p) => listView(i, p, true), chat: chatView, runs: projectRunsView, artifacts: artifactsView, code: codeView, settings: settingsView };
  function renderHead(info) {
    head.innerHTML = "";
    add(head, [
      h("h1", null, info.project.name),
      h("span", { class: "sub" }, info.project.description?.split("\n")[0].slice(0, 100)),
      h("div", { class: "spacer" }),
      h("div", { class: "install", style: { padding: "4px 10px", fontSize: "12px" } }, h("code", null, `git clone ${info.clone_url}`), h("button", { class: "btn small ghost", onclick: () => copy(`git clone ${info.clone_url}`) }, "copy")),
      h("div", { class: "tabs" }, tabs.map(([k, n]) => { const a = link(`/p/${slug}${k === "board" ? "" : "/" + k}`, n); if (k === tab) a.classList.add("active"); return a; })),
    ]);
  }
  return {
    el,
    async load() {
      let info;
      try { info = await api("GET", `/api/projects/${slug}`); } catch { body.appendChild(h("div", { class: "empty" }, "Project not found.")); return; }
      this.info = info;
      renderHead(info);
      inner = (views[tab] || boardView)(info, params || new URLSearchParams());
      body.appendChild(inner.el);
      inner.load && inner.load();
    },
    onEvent(ev) {
      if (ev.project_id && this.info && ev.project_id !== this.info.project.id) return;
      inner && inner.onEvent && inner.onEvent(ev);
    },
    destroy() { inner && inner.destroy && inner.destroy(); },
  };
}

// ---------- board ----------
function parseQuick(text) {
  const out = { title: [], labels: [], requires: [], priority: undefined };
  const pr = { "!u": 0, "!urgent": 0, "!h": 1, "!high": 1, "!m": 2, "!medium": 2, "!l": 3, "!low": 3, "!0": 0, "!1": 1, "!2": 2, "!3": 3 };
  for (const w of text.split(/\s+/)) {
    if (!w) continue;
    if (w.toLowerCase() in pr) out.priority = pr[w.toLowerCase()];
    else if (/^#[\w.-]+$/.test(w) && !/^#\d+$/.test(w)) out.labels.push(w.slice(1));
    else if (/^\+[\w.:#-]+$/.test(w)) out.requires.push(w.slice(1));
    else out.title.push(w);
  }
  return { ...out, title: out.title.join(" ") };
}

// Live agent activity, kept fresh by "activity" SSE events.
const ACT = {};
function actText(a) { return a ? a.text : "starting…"; }
function agentTile(r) {
  const a = ACT[r.id] || r.activity;
  return h("div", { class: "agent-tile" + (r.stopping ? " stopping" : ""), "data-run": r.id, onclick: () => go(`/runs/${r.id}`), title: "Watch this agent" },
    h("div", { class: "row", style: { gap: "6px" } }, h("span", { class: "dot busy" }), h("b", { style: { whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis", minWidth: 0 } }, r.node_name), h("span", { class: "mono hint" }, "/" + r.id.slice(0, 6)),
      h("span", { class: "spacer" }),
      h("button", { class: "icon-btn", title: "Message this agent", onclick: e => { e.stopPropagation(); messageAgentModal(r.id, r.issue_number ? `#${r.issue_number} ${r.issue_title}` : "a chat session", `${r.node_name}/${r.id.slice(0, 6)}`); } }, "💬"),
      r.stopping ? h("span", { class: "chip warn", title: "Its issue was moved — this session is being stopped (work so far is saved on the branch)" }, "stopping…")
        : h("span", { class: "chip " + (r.kind === "review" ? "req" : r.kind === "chat" ? "accent" : "") }, r.kind)),
    h("div", { class: "agent-task" }, r.issue_number ? `#${r.issue_number} ${r.issue_title}` : "chat session"),
    h("div", { class: "agent-now", "data-act": r.id }, actText(a)),
    h("div", { class: "row", style: { gap: "6px" } }, h("div", { class: "hint grow", "data-actmeta": r.id }, actMeta(r, a)), modelChip(r.model)));
}
function actMeta(r, a) {
  return `${dur(Date.now() - r.started_at)}` + (a ? ` · ${a.tools} tool calls · ${a.edits} edits` : "");
}
// Update live activity text in place (no re-render).
function applyActivity(ev) {
  ACT[ev.run_id] = ev.data;
  document.querySelectorAll(`[data-act="${ev.run_id}"]`).forEach(el => {
    el.textContent = actText(ev.data);
    el.classList.remove("flash"); void el.offsetWidth; el.classList.add("flash");
  });
}
setInterval(() => document.querySelectorAll("[data-actmeta]").forEach(el => {
  const r = el._run; if (r) el.textContent = actMeta(r, ACT[r.id] || r.activity);
}), 5000);


function boardView(info) {
  const slug = info.project.slug;
  let data = { columns: {}, agents: [] };
  let issues = [];
  let filter = "";
  const PER = 150;
  const boardEl = h("div", { class: "board" });
  const strip = h("div", { class: "agent-strip" });
  const filterIn = h("input", { placeholder: "Search all issues…  (press / )", style: { maxWidth: "300px" },
    oninput: debounce(e => { filter = e.target.value.trim(); load(); }, 250) });
  const noMachines = h("div");
  const el = h("div", { style: { display: "flex", flexDirection: "column", height: "100%" } },
    strip,
    h("div", { class: "row", style: { padding: "12px 20px 0" } }, filterIn,
      h("span", { class: "hint" }, "Quick add: ", h("kbd", null, "n"), "  ·  ", h("code", null, "!high"), " priority  ", h("code", null, "#label"), "  ", h("code", null, "+gpu"), " requires capability"),
      h("span", { class: "spacer" }), noMachines),
    h("div", { style: { flex: 1, minHeight: 0 } }, boardEl));

  let dragId = null;
  function card(i) {
    const agent = i.run_id && data.agents.find(r => r.id === i.run_id);
    const stuck = i.no_machine;
    return h("div", {
      class: `issue-card p${i.priority}` + (agent ? " live" : ""), draggable: "true", "data-id": i.id,
      ondragstart: e => { dragId = i.id; e.dataTransfer.effectAllowed = "move"; e.target.classList.add("dragging"); },
      ondragend: e => { e.target.classList.remove("dragging"); },
      onclick: () => openIssue(slug, i.number),
    },
      h("div", { class: "row", style: { gap: "6px" } }, h("span", { class: "num" }, `#${i.number}`),
        i.attempts > 0 && i.status !== "done" && h("span", { class: "chip warn", title: "failed attempts" }, `↻${i.attempts}`),
        stuck && h("span", { class: "chip bad", title: `No machine assigned to this project has all of: ${i.requires.join(", ")} — nobody can pick this up` }, "⚠ no machine"),
        i.open_questions ? h("span", { class: "chip warn click", title: "The agent asked a question — click to answer", onclick: e => { e.stopPropagation(); go("/questions"); } }, "❓ question") : null,
        h("span", { class: "spacer" }),
        agent && h("button", { class: "icon-btn", title: `Message ${agent.node_name}/${agent.id.slice(0, 6)} (injected into its session)`, onclick: e => { e.stopPropagation(); messageAgentModal(agent.id, `#${i.number} ${i.title}`, i.assignee); } }, "💬"),
        i.run_id && h("span", { class: "dot busy", title: `${i.assignee} is on it` })),
      h("div", { class: "t" }, i.title),
      h("div", { class: "meta" },
        i.labels.map(l => h("span", { class: "chip" }, l)),
        i.requires.map(r => h("span", { class: "chip req", title: "requires capability" }, "⚙ " + r)),
        i.depends_on.length ? h("span", { class: "chip", title: "depends on" }, "⛓ " + i.depends_on.map(d => "#" + d).join(" ")) : null,
        i.assignee && (i.status === "in_progress" || i.status === "review") ? h("span", { style: { marginLeft: "auto" } }, "🤖 " + i.assignee) : null),
      agent && h("div", { class: "card-now", "data-act": agent.id, title: "Live — click to watch", onclick: e => { e.stopPropagation(); go(`/runs/${agent.id}`); } },
        actText(ACT[agent.id] || agent.activity)),
      !agent && i.waiting && !stuck && h("div", { class: "card-wait", title: "Why no agent is working on this yet" }, "⏳ " + i.waiting));
  }

  function column([status, name]) {
    const colData = data.columns[status] || { items: [], total: 0 };
    const list = colData.items;
    const body = h("div", { class: "col-body" }, list.map(card));
    const hidden = colData.total - list.length;
    let foot = null;
    if (status === "done") foot = link(`/p/${slug}/archive${filter ? "?q=" + encodeURIComponent(filter) : ""}`, hidden > 0 ? `Last ${list.length} shown · all ${colData.total} in the archive →` : "Archive →");
    else if (hidden > 0) foot = link(`/p/${slug}/list?status=${status}${filter ? "&q=" + encodeURIComponent(filter) : ""}`, `+ ${hidden} more — open in list →`);
    const col = h("div", { class: "col" },
      h("div", { class: "col-head" }, h("span", { class: "dot", style: { background: STATUS_COLOR[status] } }), name, h("span", { class: "n" }, colData.total)),
      (status === "todo" || status === "backlog") && quickAdd(status),
      body,
      foot && h("div", { class: "col-foot" }, foot));
    let line = null, dropIndex = 0;
    col.addEventListener("dragover", e => {
      if (dragId == null) return;
      e.preventDefault();
      col.classList.add("drop");
      const cards = [...body.querySelectorAll(".issue-card:not(.dragging)")];
      dropIndex = cards.findIndex(c => e.clientY < c.getBoundingClientRect().top + c.offsetHeight / 2);
      if (dropIndex < 0) dropIndex = cards.length;
      line?.remove();
      line = h("div", { class: "drop-line" });
      cards[dropIndex] ? body.insertBefore(line, cards[dropIndex]) : body.appendChild(line);
    });
    col.addEventListener("dragleave", e => { if (!col.contains(e.relatedTarget)) { col.classList.remove("drop"); line?.remove(); } });
    col.addEventListener("drop", async e => {
      e.preventDefault();
      col.classList.remove("drop"); line?.remove();
      const it = issues.find(x => x.id === dragId);
      dragId = null;
      if (!it) return;
      const others = list.filter(x => x.id !== it.id);
      const before = others[dropIndex - 1], after = others[dropIndex];
      let sort = before && after ? (before.sort + after.sort) / 2 : before ? before.sort + 1 : after ? after.sort - 1 : it.sort;
      const patch = { sort };
      if (it.status !== status) patch.status = status;
      await api("PATCH", `/api/projects/${slug}/issues/${it.number}`, patch);
      load();
    });
    return col;
  }

  function quickAdd(status) {
    const input = h("input", { placeholder: status === "todo" ? "+ Add issue, Enter to save" : "+ Add to backlog", "data-quick": status });
    input.addEventListener("keydown", async e => {
      if (e.key === "Escape") input.blur();
      if (e.key !== "Enter" || !input.value.trim()) return;
      const q = parseQuick(input.value);
      if (!q.title) return;
      input.disabled = true;
      try {
        await api("POST", `/api/projects/${slug}/issues`, { title: q.title, status, priority: q.priority, labels: q.labels, requires: q.requires });
        input.value = "";
        await load();
      } finally { input.disabled = false; $(`input[data-quick=${status}]`)?.focus(); }
    });
    return h("div", { class: "quick-add" }, input);
  }

  function renderStrip() {
    strip.innerHTML = "";
    const agents = data.agents;
    if (!agents.length) { strip.classList.add("empty-strip"); add(strip, [h("span", { class: "hint" }, "No agents running on this project right now.")]); return; }
    strip.classList.remove("empty-strip");
    add(strip, [h("div", { class: "strip-label" }, h("span", { class: "dot busy" }), `${agents.filter(a => !a.stopping).length} working`), agents.map(agentTile)]);
    strip.querySelectorAll("[data-actmeta]").forEach(el => el._run = agents.find(r => r.id === el.dataset.actmeta));
  }

  function render() {
    renderStrip();
    const scroll = boardEl.scrollLeft;
    const scrolls = [...boardEl.querySelectorAll(".col-body")].map(b => b.scrollTop);
    boardEl.innerHTML = "";
    add(boardEl, STATUSES.map(column));
    boardEl.scrollLeft = scroll;
    boardEl.querySelectorAll(".col-body").forEach((b, k) => b.scrollTop = scrolls[k] || 0);
    noMachines.innerHTML = "";
    if (!info.nodes.length) add(noMachines, [h("span", { class: "chip warn" }, "No machines assigned — "), " ", link(`/p/${slug}/settings`, "assign machines")]);
    if (info.project.dispatch) add(noMachines, [h("button", { class: "btn small", title: "Stop every running agent and move To do / In progress to the backlog", onclick: stopWork }, "⏸ Stop all work")]);
    else add(noMachines, [h("span", { class: "chip warn" }, "⏸ Paused"), " ", h("button", { class: "btn small primary", title: "Move the stopped issues back to To do and dispatch again", onclick: resumeWork }, "▶ Resume")]);
  }
  function stopWork() {
    const running = data.agents.length;
    modal(`Stop all work on ${info.project.name}?`, h("div", null,
      h("p", null, `${running ? `${running} running agent session${running > 1 ? "s" : ""} will be stopped — uncommitted work is saved on each issue's branch as a WIP commit. ` : ""}Every issue in To do and In progress moves to the backlog, and nothing new is dispatched.`),
      h("p", { class: "hint" }, "Issues in Review stay there (a running agent review is stopped). Resume moves exactly the stopped issues back to To do; the next sessions continue from their branches.")),
      close => [h("button", { class: "btn", onclick: close }, "Cancel"),
        h("button", { class: "btn danger solid", onclick: async () => {
          const r = await api("POST", `/api/projects/${slug}/stop`); close();
          info.project.dispatch = false; toast(`Stopped ${r.stopped_runs} session(s), ${r.moved} issue(s) to the backlog`); loadOverview(); load();
        } }, "Stop all work")]);
  }
  async function resumeWork() {
    const r = await api("POST", `/api/projects/${slug}/resume`);
    info.project.dispatch = true; toast(`Resumed — ${r.moved} issue(s) back in To do`); loadOverview(); load();
  }
  async function load() {
    data = await api("GET", `/api/projects/${slug}/board?per=${PER}&done=20${filter ? "&q=" + encodeURIComponent(filter) : ""}`);
    data.agents.forEach(r => { if (r.activity && !ACT[r.id]) ACT[r.id] = r.activity; });
    issues = Object.values(data.columns).flatMap(c => c.items);
    render();
  }
  const onKey = e => {
    if (e.target.matches("input,textarea,select") || e.metaKey || e.ctrlKey || S.drawer) return;
    if (e.key === "n") { e.preventDefault(); $("input[data-quick=todo]")?.focus(); }
    if (e.key === "/") { e.preventDefault(); filterIn.focus(); }
  };
  document.addEventListener("keydown", onKey);
  const reload = debounce(load, 300);
  return {
    el, load,
    onEvent: ev => {
      if (ev.type === "issues" || ev.type === "runs") reload();
      if (ev.type === "activity" && !data.agents.some(r => r.id === ev.run_id)) reload();
    },
    destroy: () => document.removeEventListener("keydown", onKey),
  };
}

// ---------- list / archive ----------
function listView(info, params, archive) {
  const slug = info.project.slug;
  const PAGE = 100;
  const st = {
    status: archive ? "done" : (params.get("status") || "open"),
    q: params.get("q") || "", label: params.get("label") || "", requires: params.get("requires") || "",
    order: params.get("order") || (archive ? "updated" : "board"), page: +params.get("page") || 0,
  };
  const results = h("div");
  const pager = h("div", { class: "row", style: { justifyContent: "flex-end", marginTop: "10px" } });
  const statusSeg = h("div", { class: "seg" });
  const inp = (key, ph, w) => h("input", { placeholder: ph, value: st[key], style: { maxWidth: w },
    oninput: debounce(e => { st[key] = e.target.value.trim(); st.page = 0; load(); }, 250) });
  const order = h("select", { style: { width: "auto" }, onchange: e => { st.order = e.target.value; st.page = 0; load(); } },
    [["board", "Board order"], ["priority", "Priority"], ["updated", "Recently updated"], ["created", "Newest"], ["number", "Number ↓"]]
      .map(([v, n]) => h("option", { value: v, selected: v === st.order }, n)));
  const el = h("div", { class: "content" },
    archive && h("p", { class: "hint", style: { marginTop: 0 } }, "All completed issues — merged work, newest first. Search covers titles, descriptions, labels and agents."),
    h("div", { class: "row wrap", style: { marginBottom: "12px" } },
      !archive && statusSeg,
      inp("q", archive ? "Search the archive…  (#123 for a number)" : "Search…  (#123 for a number)", "320px"),
      inp("label", "label", "140px"), inp("requires", "requires", "140px"), order),
    results, pager);

  function renderSeg() {
    statusSeg.innerHTML = "";
    const opts = [["open", "Open"], ["all", "All"], ...STATUSES.filter(([s]) => s !== "done"), ["done", "Done"]];
    add(statusSeg, opts.map(([v, n]) => h("button", { class: v === st.status ? "on" : "", onclick: () => { st.status = v; st.page = 0; renderSeg(); load(); } }, n)));
  }
  function syncURL() {
    const q = new URLSearchParams();
    for (const k of ["status", "q", "label", "requires", "order"]) if (st[k] && !(archive && k === "status")) q.set(k, st[k]);
    if (st.page) q.set("page", st.page);
    history.replaceState(null, "", `/p/${slug}/${archive ? "archive" : "list"}${q.toString() ? "?" + q : ""}`);
  }
  async function load() {
    syncURL();
    const q = new URLSearchParams({ limit: PAGE, offset: st.page * PAGE, order: st.order, status: st.status === "all" ? "" : st.status });
    for (const k of ["q", "label", "requires"]) if (st[k]) q.set(k, st[k]);
    const r = await api("GET", `/api/projects/${slug}/issues?${q}`);
    results.innerHTML = "";
    if (!r.items.length) { results.appendChild(h("div", { class: "card empty" }, archive ? "No completed issues match." : "No issues match.")); pager.innerHTML = ""; return; }
    results.appendChild(h("table", { class: "list" },
      h("tr", null, h("th", null, "#"), h("th", null, "Title"), !archive && h("th", null, "Status"), h("th", null, "Priority"), h("th", null, "Labels / requires"),
        h("th", null, archive ? "Done by" : "Assignee"), h("th", null, archive ? "Completed" : "Updated")),
      r.items.map(i => h("tr", { class: "click", onclick: () => openIssue(slug, i.number) },
        h("td", { class: "mono hint" }, i.number),
        h("td", null, h("span", { style: { fontWeight: 500 } }, i.title), i.run_id ? h("span", { class: "dot busy", style: { marginLeft: "8px" } }) : null),
        !archive && h("td", null, h("span", { class: "chip", style: { color: STATUS_COLOR[i.status] } }, STATUS_NAME[i.status])),
        h("td", null, h("span", { class: "prio p" + i.priority }, PRIOS[i.priority])),
        h("td", null, h("span", { class: "row wrap", style: { gap: "4px" } }, i.labels.map(l => h("span", { class: "chip" }, l)),
          i.requires.map(x => h("span", { class: "chip req" }, "⚙ " + x)),
          i.no_machine ? h("span", { class: "chip bad" }, "⚠ no machine") : null)),
        h("td", { class: "hint" }, i.assignee || "—"),
        h("td", { class: "hint", style: { whiteSpace: "nowrap" } }, ago(i.updated_at))))));
    pager.innerHTML = "";
    const from = st.page * PAGE + 1, to = st.page * PAGE + r.items.length;
    add(pager, [h("span", { class: "hint" }, `${from}–${to} of ${r.total}`),
      h("button", { class: "btn small", disabled: st.page === 0, onclick: () => { st.page--; load(); el.scrollTop = 0; } }, "← Prev"),
      h("button", { class: "btn small", disabled: to >= r.total, onclick: () => { st.page++; load(); el.scrollTop = 0; } }, "Next →")]);
  }
  renderSeg();
  return { el, load, onEvent: debounce(ev => { if (ev.type === "issues") load(); }, 400) };
}

// ---------- issue drawer ----------
function closeDrawer(silent) {
  if (!S.drawer) return;
  S.drawer.destroy();
  S.drawer = null;
  if (!silent) {
    const m = location.pathname.match(/^\/p\/([^/]+)\/i\/\d+/);
    if (m) history.pushState(null, "", S.drawerReturn || `/p/${m[1]}`);
  }
}

async function openIssue(slug, number, fromRoute) {
  closeDrawer(true);
  if (!fromRoute) { S.drawerReturn = location.pathname + location.search; history.pushState(null, "", `/p/${slug}/i/${number}`); }
  else S.drawerReturn = null;
  const root = $("#drawer-root");
  const body = h("div", { class: "drawer-body" });
  const headActions = h("div", { class: "row" });
  const drawer = h("div", { class: "drawer" },
    h("div", { class: "drawer-head" }, h("span", { class: "num mono" }, `#${number}`), h("span", { class: "spacer" }), headActions,
      h("button", { class: "btn ghost", title: "Close (Esc)", onclick: () => closeDrawer() }, "✕")),
    body);
  const backdrop = h("div", { class: "drawer-backdrop", onclick: () => closeDrawer() });
  root.innerHTML = "";
  root.append(backdrop, drawer);
  const onKey = e => { if (e.key === "Escape" && !$("#modal-root").children.length) closeDrawer(); };
  document.addEventListener("keydown", onKey);

  const base = `/api/projects/${slug}/issues/${number}`;
  let issue;
  const save = async patch => { issue = await api("PATCH", base, patch); };

  function render() {
    const i = issue;
    const focused = document.activeElement && drawer.contains(document.activeElement) && document.activeElement.matches("input,textarea") ? document.activeElement.dataset.f : null;
    const keep = {};
    if (focused) body.querySelectorAll("[data-f]").forEach(x => keep[x.dataset.f] = x.value);
    body.innerHTML = "";
    headActions.innerHTML = "";
    if (i.status === "review") add(headActions, [
      h("button", { class: "btn ok small", onclick: async () => { await api("POST", base + "/merge"); toast("Merged"); load(); } }, "✓ Approve & merge"),
      h("button", { class: "btn small", onclick: () => requestChanges() }, "Request changes")]);
    if (i.status === "blocked") add(headActions, [h("button", { class: "btn small", onclick: () => save({ status: "todo" }).then(render) }, "↻ Back to To do")]);
    const title = h("input", { class: "title-input", "data-f": "title", value: i.title, onchange: e => save({ title: e.target.value }) });
    const desc = h("textarea", { "data-f": "body", rows: 8, placeholder: "Description (markdown). Agents get this as their instructions.", value: i.body, onchange: e => save({ body: e.target.value }) });
    const listInput = (key, ph) => h("input", { "data-f": key, placeholder: ph, value: key === "depends_on" ? i.depends_on.join(", ") : i[key].join(", "),
      onchange: e => save({ [key]: key === "depends_on" ? e.target.value.split(/[,\s]+/).map(x => parseInt(x.replace("#", ""))).filter(x => x > 0) : e.target.value.split(",").map(x => x.trim()).filter(Boolean) }) });
    const statusSel = h("select", { style: { width: "auto" }, onchange: e => save({ status: e.target.value }).then(render) },
      STATUSES.map(([s, n]) => h("option", { value: s, selected: s === i.status }, n)));
    add(body, [
      title,
      h("div", { class: "props" },
        h("div", { class: "k" }, "Status"), h("div", { class: "row" }, statusSel, i.run_id && h("span", { class: "row hint" }, h("span", { class: "dot busy" }), `${i.assignee} is ${i.status === "review" ? "reviewing" : "working"} · `, link(`/runs/${i.run_id}`, "watch live"))),
        h("div", { class: "k" }, "Priority"), h("div", { class: "seg" }, PRIOS.map((p, n) => h("button", { class: n === i.priority ? "on" : "", onclick: () => save({ priority: n }).then(render) }, p))),
        h("div", { class: "k" }, "Labels"), listInput("labels", "e.g. backend, ui"),
        h("div", { class: "k", title: "Only machines with all these capabilities can claim the issue" }, "Requires"), listInput("requires", "capabilities, e.g. gpu, gpus:2, vram:48, docker"),
        h("div", { class: "k" }, "Depends on"), listInput("depends_on", "issue numbers, e.g. 3, 7"),
        h("div", { class: "k", title: "Claude model for this issue's sessions. Empty = project default, then the machine's setting." }, "Model"),
        (() => {
          const cur = i.run_id && (i.runs || []).find(r => r.id === i.run_id);
          const inp = modelInput(i.model, `default${projectBySlug(slug)?.model ? " (" + projectBySlug(slug).model + ")" : ""}`, v => save({ model: v }).then(render));
          inp.dataset.f = "model";
          return h("div", { class: "row", style: { gap: "8px" } }, inp,
            cur && cur.model ? h("span", { class: "hint", style: { whiteSpace: "nowrap" } }, "running ", modelChip(cur.model)) : null,
            cur ? h("button", { class: "btn small", style: { whiteSpace: "nowrap" }, onclick: () => switchModelModal({ ...cur, issue_model: i.model }, load) }, "Switch now") : null);
        })(),
        h("div", { class: "k" }, "Branch"), h("div", { class: "mono hint" }, i.branch || `issue-${i.number} (when started)`),
        h("div", { class: "k" }, "Created"), h("div", { class: "hint" }, `${ago(i.created_at)} by ${i.created_by}`)),
      i.open_questions ? h("div", { class: "card pad", style: { borderLeft: "3px solid var(--warn)", marginBottom: "12px" } }, `❓ The agent asked ${i.open_questions > 1 ? i.open_questions + " questions" : "a question"} about this issue. `, link("/questions", "Answer it →")) : null,
      i.run_id ? h("div", { class: "card pad agent-msg-card" }, h("div", { class: "hint", style: { marginBottom: "6px" } }, `💬 Message ${i.assignee} — injected straight into its running session`), agentMessageBox(i.run_id)) : null,
      desc,
      (() => {
        const box = h("div");
        api("GET", `/api/projects/${slug}/artifacts?issue=${i.number}`).then(r => {
          if (r.items.length) add(box, [h("div", { class: "section-title" }, `Artifacts (${r.items.length})`), artifactTable(slug, r.items, null)]);
        }).catch(() => {});
        return box;
      })(),
      i.runs?.length ? [h("div", { class: "section-title" }, "Agent runs"), runTable(i.runs.slice(0, 8), false)] : null,
      h("div", { class: "section-title" }, "Activity"),
      (i.comments || []).map(c => {
        const sys = c.author === "multiclaude" || (c.body.startsWith("moved ") && c.body.length < 40) || c.body.startsWith("🤖");
        return h("div", { class: "comment" + (sys && c.body.length < 200 ? " system" : "") },
          h("div", { class: "who" }, h("b", null, c.author === "human" ? "You" : c.author), h("span", null, ago(c.created_at))),
          h("div", { class: "md", html: md(c.body) }));
      }),
      commentBox(),
      h("div", { style: { marginTop: "30px" } }, h("button", { class: "btn danger small", onclick: async () => {
        if (!confirm(`Delete issue #${i.number}?`)) return;
        await api("DELETE", base); closeDrawer();
      } }, "Delete issue")),
    ]);
    if (focused) {
      body.querySelectorAll("[data-f]").forEach(x => { if (x.dataset.f in keep) x.value = keep[x.dataset.f]; });
      body.querySelector(`[data-f="${focused}"]`)?.focus();
    }
  }
  function commentBox() {
    const ta = h("textarea", { "data-f": "comment", rows: 3, placeholder: "Comment… (if an agent is working on this issue, it receives your comment live)" });
    return h("div", null, ta, h("div", { class: "row", style: { justifyContent: "flex-end", marginTop: "6px" } },
      h("button", { class: "btn primary small", onclick: async () => {
        if (!ta.value.trim()) return;
        await api("POST", base + "/comments", { body: ta.value }); ta.value = ""; load();
      } }, "Comment")));
  }
  function requestChanges() {
    const ta = h("textarea", { rows: 5, placeholder: "What should be changed?" });
    modal("Request changes", ta, close => [h("button", { class: "btn", onclick: close }, "Cancel"),
      h("button", { class: "btn primary", onclick: async () => { await api("POST", base + "/request-changes", { body: ta.value }); close(); load(); } }, "Send back to To do")]);
  }
  async function load() { try { issue = await api("GET", base); render(); } catch { closeDrawer(); } }
  S.drawer = {
    destroy: () => { root.innerHTML = ""; document.removeEventListener("keydown", onKey); },
    onEvent: debounce(ev => { if (ev.type === "issues" || ev.type === "runs") load(); }, 250),
  };
  load();
}

// ---------- chat ----------
function chatView(info) {
  const slug = info.project.slug;
  const log = h("div", { class: "chat-log" });
  const ta = h("textarea", { rows: 2, placeholder: "Message the agents…  (Enter to send, Shift+Enter for newline)" });
  const target = h("select");
  function fillTargets() {
    const cur = target.value;
    target.innerHTML = "";
    add(target, [h("option", { value: "" }, "→ All machines on this project"),
      S.overview.nodes.map(n => h("option", { value: n.id }, `→ ${n.name}${n.online ? "" : " (offline)"}`))]);
    target.value = cur;
  }
  fillTargets();
  const silent = h("input", { type: "checkbox" });
  const send = async () => {
    if (!ta.value.trim()) return;
    const body = ta.value; ta.value = "";
    await api("POST", `/api/projects/${slug}/messages`, { body, target: target.value, silent: silent.checked });
  };
  ta.addEventListener("keydown", e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } });
  const el = h("div", { class: "chat" }, log,
    h("div", { class: "composer" },
      h("div", { style: { display: "flex", flexDirection: "column", gap: "6px" } }, target,
        h("label", { class: "toggle hint", title: "Just post, don't wake agents" }, silent, "post only")),
      ta, h("button", { class: "btn primary", onclick: send }, "Send")));
  function msgEl(m) {
    if (m.author === "system") return h("div", { class: "msg system" }, m.body);
    const human = m.author === "human";
    let to = "";
    if (m.target && !m.target.startsWith("reply:")) to = " → " + nodeName(m.target);
    return h("div", { class: "msg " + (human ? "human" : "agent") },
      h("div", { class: "who" }, h("b", null, human ? "You" : "🤖 " + m.author), to, h("span", null, ago(m.created_at)),
        m.target.startsWith("reply:") && link(`/runs/${m.target.slice(6)}`, "session")),
      h("div", { class: "bubble md", html: md(m.body) }),
      /^❓ Question Q\d+/.test(m.body) && h("div", { style: { marginTop: "4px" } }, link("/questions", "→ Answer on the Questions page")));
  }
  function append(m) {
    const atBottom = log.scrollHeight - log.scrollTop - log.clientHeight < 80;
    log.appendChild(msgEl(m));
    if (atBottom) log.scrollTop = log.scrollHeight;
  }
  return {
    el,
    async load() {
      const msgs = await api("GET", `/api/projects/${slug}/messages`);
      log.innerHTML = "";
      if (!msgs.length) log.appendChild(h("div", { class: "empty" }, "Talk to your agents here. Messages go to all machines assigned to this project (or one machine), get injected live into running sessions, or start a chat session on idle machines. Try: “Plan the architecture for X and create issues for it.”"));
      msgs.forEach(append);
      log.scrollTop = log.scrollHeight;
      ta.focus();
    },
    onEvent(ev) {
      if (ev.type === "message") { log.querySelector(".empty")?.remove(); append(ev.data); }
      if (ev.type === "nodes") setTimeout(fillTargets, 500);
    },
  };
}

// ---------- runs ----------
function projectRunsView(info) {
  const el = h("div", { class: "content" });
  async function load() {
    const runs = await api("GET", `/api/projects/${info.project.slug}/runs`);
    el.innerHTML = "";
    el.appendChild(runs.length ? runTable(runs, false) : h("div", { class: "card empty" }, "No agent runs yet."));
  }
  return { el, load, onEvent: debounce(ev => { if (ev.type === "runs") load(); }, 300) };
}

// ---------- artifacts ----------
const fmtBytes = n => n >= 1 << 30 ? (n / (1 << 30)).toFixed(1) + " GB" : n >= 1 << 20 ? (n / (1 << 20)).toFixed(1) + " MB" : n >= 1024 ? Math.round(n / 1024) + " KB" : n + " B";
const artifactURL = (slug, name) => `/api/projects/${encodeURIComponent(slug)}/artifacts/` + name.split("/").map(encodeURIComponent).join("/");

function artifactTable(slug, items, onDelete) {
  return h("table", { class: "list" },
    h("tr", null, h("th", null, "Name"), h("th", null, "Size"), h("th", null, "Uploaded by"), h("th", null, "Issue"), h("th", null, "When"), h("th", null, "⇩"), h("th", null, "")),
    items.map(a => h("tr", null,
      h("td", null, h("a", { href: artifactURL(slug, a.name), download: "", class: "mono", title: `sha256 ${a.sha256}` }, a.name), a.description ? h("div", { class: "hint" }, a.description) : null),
      h("td", { class: "mono", style: { whiteSpace: "nowrap" } }, fmtBytes(a.size)),
      h("td", null, a.uploaded_by),
      h("td", null, a.issue_number ? link(`/p/${slug}/i/${a.issue_number}`, `#${a.issue_number}`) : ""),
      h("td", { class: "hint", style: { whiteSpace: "nowrap" } }, ago(a.created_at)),
      h("td", { class: "hint" }, a.downloads || ""),
      h("td", null, h("div", { class: "row", style: { gap: "4px", flexWrap: "nowrap" } },
        h("button", { class: "btn small ghost", title: "Copy the command to fetch it on a machine", onclick: () => copy(`multiclaude artifact get ${a.name}`) }, "cmd"),
        onDelete && h("button", { class: "btn small ghost danger", onclick: () => { if (confirm(`Delete ${a.name}? This can't be undone.`)) onDelete(a); } }, "✕"))))));
}

function artifactsView(info, params) {
  const slug = info.project.slug;
  const el = h("div", { class: "content" });
  let q = params.get("q") || "";
  const results = h("div");
  const usage = h("span", { class: "hint" });
  const search = h("input", { placeholder: "Search artifacts…  (name, description, uploader)", value: q, style: { maxWidth: "340px" },
    oninput: debounce(e => { q = e.target.value.trim(); history.replaceState(null, "", `/p/${slug}/artifacts${q ? "?q=" + encodeURIComponent(q) : ""}`); load(); }, 250) });
  const fileIn = h("input", { type: "file", multiple: true, style: { display: "none" }, onchange: () => upload([...fileIn.files]) });
  const prefixIn = h("input", { placeholder: "folder, e.g. builds/ (optional)", style: { maxWidth: "220px" } });
  const descIn = h("input", { placeholder: "description (optional)", style: { maxWidth: "260px" } });
  const prog = h("div", { class: "hint" });
  const drop = h("div", { class: "card pad drop-zone" },
    h("div", { class: "row wrap", style: { gap: "8px" } },
      h("button", { class: "btn primary", onclick: () => fileIn.click() }, "⇧ Upload files"), prefixIn, descIn,
      h("span", { class: "hint" }, "or drop files here"), fileIn),
    prog);
  drop.addEventListener("dragover", e => { e.preventDefault(); drop.classList.add("drop"); });
  drop.addEventListener("dragleave", () => drop.classList.remove("drop"));
  drop.addEventListener("drop", e => { e.preventDefault(); drop.classList.remove("drop"); upload([...e.dataTransfer.files]); });

  function upload(files) {
    if (!files.length) return;
    let prefix = prefixIn.value.trim().replace(/^\/+/, "");
    if (prefix && !prefix.endsWith("/")) prefix += "/";
    const next = () => {
      const f = files.shift();
      if (!f) { prog.textContent = ""; fileIn.value = ""; load(); return; }
      const name = prefix + f.name;
      const xhr = new XMLHttpRequest();
      xhr.open("PUT", artifactURL(slug, name) + (descIn.value.trim() ? "?description=" + encodeURIComponent(descIn.value.trim()) : ""));
      xhr.upload.onprogress = e => { if (e.lengthComputable) prog.textContent = `Uploading ${name}: ${fmtBytes(e.loaded)} / ${fmtBytes(e.total)} (${Math.round(100 * e.loaded / e.total)}%)`; };
      xhr.onload = () => { if (xhr.status >= 300) { let m = xhr.statusText; try { m = JSON.parse(xhr.responseText).error; } catch {} toast(`${name}: ${m}`, true); } else toast(`Uploaded ${name}`); next(); };
      xhr.onerror = () => { toast(`${name}: upload failed`, true); next(); };
      xhr.send(f);
    };
    next();
  }
  async function load() {
    const r = await api("GET", `/api/projects/${slug}/artifacts${q ? "?q=" + encodeURIComponent(q) : ""}`);
    usage.textContent = `${r.total} artifact${r.total === 1 ? "" : "s"} · ${fmtBytes(r.total_size)}${r.total > r.items.length ? ` · newest ${r.items.length} shown` : ""}`;
    results.innerHTML = "";
    results.appendChild(r.items.length ? artifactTable(slug, r.items, async a => { await api("DELETE", artifactURL(slug, a.name)); toast("Deleted"); load(); })
      : h("div", { class: "card empty" }, q ? "No artifacts match." : ["No artifacts yet. Agents upload builds and large files with ", h("code", null, "multiclaude artifact put"), "; you can upload here too."]));
  }
  add(el, [
    h("p", { class: "hint", style: { marginTop: 0 } }, "Builds, datasets, checkpoints and other large files outside git. Agents use ", h("code", null, "multiclaude artifact put/get/ls"),
      " in their sessions; on an enrolled machine you can too: ", h("code", null, `multiclaude-runner artifact get <name> --project ${slug}`), "."),
    drop,
    h("div", { class: "row", style: { margin: "14px 0 8px", gap: "12px" } }, search, usage),
    results,
  ]);
  return { el, load, onEvent: debounce(ev => { if (ev.type === "artifacts") load(); }, 300) };
}

const toolSummary = (name, input) => {
  input = input || {};
  const n = name.replace(/^mcp__multiclaude__/, "⧉ ");
  const v = input.command || input.file_path || input.pattern || input.description || input.url || input.query || input.title || input.message || input.summary || input.verdict || input.path;
  return [n, v ? String(v).split("\n")[0].slice(0, 160) : JSON.stringify(input).slice(0, 120)];
};

// Line diff for Edit tool calls (old_string → new_string).
function editDiff(oldS, newS) {
  const a = String(oldS ?? "").split("\n"), b = String(newS ?? "").split("\n");
  let p = 0; while (p < a.length && p < b.length && a[p] === b[p]) p++;
  let s = 0; while (s < a.length - p && s < b.length - p && a[a.length - 1 - s] === b[b.length - 1 - s]) s++;
  const rows = [];
  for (let k = Math.max(0, p - 2); k < p; k++) rows.push([" ", a[k]]);
  for (let k = p; k < a.length - s; k++) rows.push(["-", a[k]]);
  for (let k = p; k < b.length - s; k++) rows.push(["+", b[k]]);
  for (let k = a.length - s; k < Math.min(a.length, a.length - s + 2); k++) rows.push([" ", a[k]]);
  return h("pre", { class: "diffblock" }, rows.slice(0, 400).map(([t, l]) => h("span", { class: t === "-" ? "del" : t === "+" ? "add" : "" }, t + " " + l + "\n")));
}

function todoList(todos) {
  return h("div", { class: "todos" }, (todos || []).map(t => h("div", { class: "todo " + (t.status || "") },
    h("span", { class: "box" }, t.status === "completed" ? "✓" : t.status === "in_progress" ? "▸" : ""), t.status === "in_progress" ? (t.activeForm || t.content) : t.content)));
}

function runView(id) {
  const header = h("div", { class: "topbar", style: { paddingBottom: "14px" } });
  const logEl = h("div", { class: "runlog" });
  const side = h("aside", { class: "run-side" });
  const ta = h("textarea", { rows: 2, placeholder: "Message this agent… (injected into the live session; on finished runs this resumes the session)" });
  let mode = "all", follow = true;
  const modeSeg = h("div", { class: "seg" });
  const followCb = h("input", { type: "checkbox", checked: true, onchange: e => { follow = e.target.checked; if (follow) logEl.scrollTop = logEl.scrollHeight; } });
  const toolbar = h("div", { class: "row run-toolbar" }, modeSeg, h("span", { class: "spacer" }),
    h("button", { class: "btn small ghost", onclick: () => logEl.querySelectorAll("details").forEach(d => d.open = true) }, "Expand all"),
    h("button", { class: "btn small ghost", onclick: () => logEl.querySelectorAll("details").forEach(d => d.open = false) }, "Collapse all"),
    h("label", { class: "toggle" }, followCb, "Follow"));
  const el = h("div", { style: { display: "flex", flexDirection: "column", height: "100%" } }, header,
    h("div", { class: "run-wrap" },
      h("div", { class: "run-main" }, toolbar, logEl,
        h("div", { class: "composer" }, ta, h("button", { class: "btn primary", onclick: () => send() }, "Send"))),
      side));
  let run, lastId = 0, tools = {};
  const stats = { turns: 0, tools: 0, edits: 0, cmds: 0, msgs: 0, errors: 0 };
  const files = new Map(); // path -> {reads, edits}
  let todos = null;
  ta.addEventListener("keydown", e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } });
  logEl.addEventListener("scroll", () => {
    const atBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 60;
    if (follow !== atBottom) { follow = atBottom; followCb.checked = atBottom; }
  });
  function renderMode() {
    modeSeg.innerHTML = "";
    add(modeSeg, [["all", "Everything"], ["talk", "Messages only"], ["tools", "Tools only"]].map(([v, n]) =>
      h("button", { class: v === mode ? "on" : "", onclick: () => { mode = v; logEl.dataset.mode = v; renderMode(); } }, n)));
  }
  async function send() {
    if (!ta.value.trim()) return;
    const r = await api("POST", `/api/runs/${id}/message`, { body: ta.value });
    ta.value = "";
    if (r.run_id !== id) { toast("Continuing in a new session"); go(`/runs/${r.run_id}`); }
  }
  function renderHeader() {
    header.innerHTML = "";
    add(header, [
      h("h1", null, run.kind === "chat" ? "Chat session" : `${run.kind === "review" ? "Review" : "Work"} · #${run.issue_number} ${run.issue_title}`),
      runDot(run),
      h("span", { class: "sub" }, `agent ${run.node_name}/${run.id.slice(0, 6)} · ${run.project_slug}`),
      modelChip(run.model),
      h("div", { class: "spacer" }),
      run.status === "running" && run.issue_number && h("button", { class: "btn small", onclick: () => switchModelModal(run) }, "Switch model"),
      run.issue_number ? link(`/p/${run.project_slug}/i/${run.issue_number}`, "Open issue") : link(`/p/${run.project_slug}/chat`, "Project chat"),
      run.status === "running" && h("button", { class: "btn danger small", onclick: () => api("POST", `/api/runs/${id}/cancel`).then(() => toast("Cancelling…")) }, "Cancel run"),
    ]);
  }
  const renderSide = debounce(() => {
    side.innerHTML = "";
    const live = run && run.status === "running";
    const a = ACT[id] || run?.activity;
    const fileList = [...files.entries()].sort((x, y) => (y[1].edits - x[1].edits) || (y[1].reads - x[1].reads));
    add(side, [
      h("div", { class: "side-sec" },
        h("div", { class: "section-title", style: { marginTop: 0 } }, live ? "Now" : "Status"),
        live ? h("div", { class: "agent-now big", "data-act": id }, actText(a)) : h("div", null, runDot(run)),
        h("div", { class: "kv" },
          h("div", { class: "k" }, "Machine"), h("div", null, run.node_name),
          h("div", { class: "k" }, "Duration"), h("div", null, dur((run.ended_at || Date.now()) - run.started_at)),
          h("div", { class: "k" }, "Cost"), h("div", null, money(run.cost_usd)),
          h("div", { class: "k" }, "Turns"), h("div", null, stats.turns),
          h("div", { class: "k" }, "Tool calls"), h("div", null, stats.tools + (stats.errors ? ` (${stats.errors} failed)` : "")),
          h("div", { class: "k" }, "Edits"), h("div", null, stats.edits),
          h("div", { class: "k" }, "Commands"), h("div", null, stats.cmds),
          h("div", { class: "k" }, "Messages"), h("div", null, stats.msgs))),
      todos && h("div", { class: "side-sec" }, h("div", { class: "section-title" }, "Agent's plan"), todoList(todos)),
      fileList.length ? h("div", { class: "side-sec" }, h("div", { class: "section-title" }, `Files (${fileList.length})`),
        h("div", { class: "files" }, fileList.slice(0, 60).map(([f, c]) => h("div", { class: "file", title: f, onclick: () => jumpToFile(f) },
          h("span", { class: "fname" }, f.split("/").slice(-2).join("/")),
          h("span", { class: "hint" }, [c.edits && `✎${c.edits}`, c.reads && `📖${c.reads}`].filter(Boolean).join(" ")))))) : null,
    ]);
  }, 200);
  function jumpToFile(f) {
    const hits = [...logEl.querySelectorAll(".ev[data-file]")].filter(x => x.dataset.file === f);
    const t = hits[hits.length - 1];
    if (t) { follow = false; followCb.checked = false; t.open = true; t.scrollIntoView({ block: "center" }); t.classList.remove("flash"); void t.offsetWidth; t.classList.add("flash"); }
  }
  function touchFile(f, kind) {
    if (!f) return;
    const c = files.get(f) || { reads: 0, edits: 0 };
    c[kind]++; files.set(f, c);
  }
  function toolEl(c) {
    const name = c.name.replace(/^mcp__multiclaude__/, "");
    const inp = c.input || {};
    const det = (cls, summary, ...body) => h("details", { class: "ev tool " + cls }, h("summary", null, ...summary), ...body);
    stats.tools++;
    switch (name) {
      case "Edit": stats.edits++; touchFile(inp.file_path, "edits");
        return Object.assign(det("t-edit", [h("span", { class: "tname" }, "✎ Edit"), " ", inp.file_path], editDiff(inp.old_string, inp.new_string)), { open: true });
      case "MultiEdit": stats.edits++; touchFile(inp.file_path, "edits");
        return Object.assign(det("t-edit", [h("span", { class: "tname" }, "✎ MultiEdit"), " ", inp.file_path, h("span", { class: "hint" }, ` · ${(inp.edits || []).length} edits`)],
          (inp.edits || []).map(e => editDiff(e.old_string, e.new_string))), { open: true });
      case "Write": stats.edits++; touchFile(inp.file_path, "edits");
        return det("t-edit", [h("span", { class: "tname" }, "✎ Write"), " ", inp.file_path, h("span", { class: "hint" }, ` · ${String(inp.content || "").split("\n").length} lines`)],
          h("pre", { class: "diffblock" }, h("span", { class: "add" }, String(inp.content || "").slice(0, 20000))));
      case "Read": touchFile(inp.file_path, "reads");
        return det("t-read", [h("span", { class: "tname" }, "📖 Read"), " ", inp.file_path]);
      case "Bash": stats.cmds++;
        return det("t-bash", [h("span", { class: "tname" }, "$"), " ", h("span", { class: "cmd" }, String(inp.command || "").split("\n")[0].slice(0, 200)), inp.description ? h("span", { class: "hint" }, "  — " + inp.description) : null],
          String(inp.command || "").includes("\n") ? h("pre", null, inp.command) : null);
      case "TodoWrite":
        todos = inp.todos;
        return h("div", { class: "ev plan", "data-kind": "tool" }, h("div", { class: "hint" }, "☑ updated plan"), todoList(inp.todos));
      case "Task": case "Agent":
        return det("t-task", [h("span", { class: "tname" }, "🧩 Subagent"), " ", inp.description || ""], h("pre", null, inp.prompt || ""));
      case "post_chat": stats.msgs++;
        return h("div", { class: "ev agentmsg", "data-kind": "talk" }, h("div", { class: "hint" }, `💬 to ${inp.to || "everyone"}`), h("div", { class: "md", html: md(inp.message) }));
      case "complete_work":
        return h("div", { class: "ev finish ok", "data-kind": "talk" }, h("b", null, "✔ complete_work"), h("div", { class: "md", html: md(inp.summary) }));
      case "review_verdict":
        return h("div", { class: "ev finish " + (inp.verdict === "approve" ? "ok" : "warn"), "data-kind": "talk" }, h("b", null, `⚖ review verdict: ${inp.verdict}`), h("div", { class: "md", html: md(inp.comment) }));
      case "block_issue":
        return h("div", { class: "ev finish bad", "data-kind": "talk" }, h("b", null, "🚧 block_issue"), h("div", { class: "md", html: md(inp.reason) }));
      case "create_issue":
        return det("t-mc", [h("span", { class: "tname" }, "➕ create_issue"), " ", inp.title || "", inp.requires?.length ? h("span", { class: "chip req" }, "⚙ " + inp.requires.join(", ")) : null], h("pre", null, JSON.stringify(inp, null, 2)));
    }
    const [n, v] = toolSummary(c.name, inp);
    if (inp.file_path) touchFile(inp.file_path, "reads");
    return det("", [h("span", { class: "tname" }, n), " ", v], h("pre", null, JSON.stringify(inp, null, 2)));
  }
  function addEv(e) {
    const d = e.data || {};
    const put = x => { if (!x) return; if (!x.dataset.kind) x.dataset.kind = x.classList.contains("tool") ? "tool" : "talk"; logEl.appendChild(x); };
    if (d.truncated) put(h("div", { class: "ev sys" }, `(large ${e.type} event truncated)`));
    else if (e.type === "system" && d.subtype === "init") put(h("div", { class: "ev sys" }, `Claude session ${String(d.session_id || "").slice(0, 8)} · model ${d.model} · ${(d.tools || []).length} tools · ${d.cwd || ""}`));
    else if (e.type === "assistant") {
      for (const c of d.message?.content || []) {
        if (c.type === "text" && c.text.trim()) put(h("div", { class: "ev text md", html: md(c.text) }));
        else if (c.type === "tool_use") {
          const t = toolEl(c);
          if (c.input?.file_path) t.dataset.file = c.input.file_path;
          if (t.tagName === "DETAILS") tools[c.id] = t;
          put(t);
        }
      }
    } else if (e.type === "user") {
      const content = d.message?.content;
      if (Array.isArray(content)) for (const c of content) {
        if (c.type !== "tool_result") continue;
        let text = typeof c.content === "string" ? c.content : (c.content || []).map(x => x.text || "").join("\n");
        if (text.length > 6000) text = text.slice(0, 6000) + "\n…";
        const t = tools[c.tool_use_id];
        if (c.is_error) stats.errors++;
        if (t) {
          if (c.is_error) { t.classList.add("err"); t.open = true; }
          if (!t.classList.contains("t-edit") || c.is_error) t.appendChild(h("pre", { class: c.is_error ? "errout" : "" }, text || "(no output)"));
        }
      }
    } else if (e.type === "result") {
      stats.turns++;
      put(h("div", { class: "ev result" + (d.is_error ? " err" : "") },
        h("div", { class: "hint" }, `${d.subtype} · ${d.num_turns} turns · ${dur(d.duration_ms || 0)} · ${money(d.total_cost_usd)}`),
        h("div", { class: "md", html: md(d.result || "") })));
    } else if (e.type === "human") put(h("div", { class: "ev human" }, d.text));
    else if (e.type === "runner") put(h("div", { class: "ev sys" }, "runner: " + d.text));
    else if (e.type === "raw" || e.type === "stderr") put(h("div", { class: "ev sys mono" }, d.text));
    renderSide();
  }
  const scrollDown = () => { if (follow) logEl.scrollTop = logEl.scrollHeight; };
  async function loadEvents() {
    for (;;) {
      const evs = await api("GET", `/api/runs/${id}/events?after=${lastId}`);
      evs.forEach(e => { lastId = Math.max(lastId, e.id); addEv(e); });
      if (evs.length < 5000) break;
    }
  }
  const tick = setInterval(() => { if (run && run.status === "running") renderSide(); }, 5000);
  renderMode();
  return {
    el,
    async load() {
      run = await api("GET", `/api/runs/${id}`);
      renderHeader();
      if (run.prompt) logEl.appendChild(h("details", { class: "ev tool", "data-kind": "tool" }, h("summary", null, h("span", { class: "tname" }, "📋 assignment prompt")), h("pre", null, run.prompt)));
      await loadEvents();
      logEl.scrollTop = logEl.scrollHeight;
      renderSide();
    },
    onEvent: (ev) => {
      if (ev.type === "run_event" && ev.run_id === id && ev.data.id > lastId) { lastId = ev.data.id; addEv(ev.data); scrollDown(); }
      if (ev.type === "runs" && run) api("GET", `/api/runs/${id}`).then(r => { run = r; renderHeader(); renderSide(); });
    },
    destroy: () => clearInterval(tick),
  };
}

// ---------- code browser ----------
function codeView(info, params) {
  const p = { ...info.project }, slug = p.slug;
  const repo = params.get("repo") || "";
  const path = params.get("path") || "";
  const mode = params.get("commit") ? "commit" : params.get("file") ? "file" : params.get("view") || "files";
  const el = h("div", { class: "content" });
  let ref = params.get("ref") || "";
  const rq = repo ? `repo=${encodeURIComponent(repo)}&` : "";
  const q = o => `/p/${slug}/code?` + new URLSearchParams(Object.fromEntries(Object.entries({ repo, ref, ...o }).filter(([, v]) => v))).toString();

  async function load() {
    const repos = await api("GET", `/api/projects/${slug}/repos`);
    const cur = repos.find(r => r.name === repo) || repos[0];
    p.default_branch = cur.default_branch;
    if (!ref) ref = cur.default_branch;
    const branches = await api("GET", `/api/projects/${slug}/repo/branches?${rq}`);
    const refSel = h("select", { style: { width: "auto" }, onchange: e => go(q({ ref: e.target.value, path, view: mode === "file" ? "files" : mode })) },
      branches.map(b => h("option", { value: b.name, selected: b.name === ref }, b.name)));
    const repoSel = repos.length > 1 ? h("select", { style: { width: "auto", fontWeight: "600" }, title: "Repository", onchange: e => go(`/p/${slug}/code?` + (e.target.value === repos[0].name ? "" : "repo=" + encodeURIComponent(e.target.value))) },
      repos.map(r => h("option", { value: r.name, selected: r === cur }, "📦 " + r.name))) : null;
    const nav = h("div", { class: "row", style: { marginBottom: "14px" } }, repoSel, refSel,
      h("div", { class: "seg" }, [["files", "Files"], ["log", "Commits"], ["branches", `Branches (${branches.length})`]].map(([k, n]) =>
        h("button", { class: (mode === k || (k === "files" && mode === "file") || (k === "log" && mode === "commit")) ? "on" : "", onclick: () => go(q({ view: k })) }, n))));
    el.innerHTML = "";
    el.appendChild(nav);
    if (mode === "files" || mode === "file") {
      const crumbs = h("div", { class: "breadcrumb", style: { marginBottom: "10px" } }, link(q({}), repo || slug));
      let acc = "";
      for (const part of path.split("/").filter(Boolean)) { acc += (acc ? "/" : "") + part; add(crumbs, [" / ", link(q({ path: acc }), part)]); }
      el.appendChild(crumbs);
      if (mode === "file") {
        const b = await api("GET", `/api/projects/${slug}/repo/blob?${rq}ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(path)}`);
        const raw = `/api/projects/${slug}/repo/blob?${rq}raw=1&ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(path)}`;
        el.appendChild(h("div", { class: "row hint", style: { marginBottom: "8px" } }, `${b.size} bytes`, h("span", { class: "spacer" }),
          b.lfs ? h("a", { class: "btn small", href: b.lfs.href, download: path.split("/").pop() }, `Download LFS object (${(b.lfs.size / 1048576).toFixed(1)} MB)`) : h("a", { class: "btn small", href: raw }, "Download")));
        if (b.lfs) el.appendChild(h("div", { class: "card empty" }, `Git LFS object ${b.lfs.oid.slice(0, 12)}…`));
        else if (b.binary) el.appendChild(h("div", { class: "card empty" }, "Binary file"));
        else {
          const pre = h("pre");
          b.text.replace(/\n$/, "").split("\n").forEach(l => pre.appendChild(h("span", { class: "ln" }, l || " ")));
          el.appendChild(h("div", { class: "code-view" }, pre));
        }
      } else {
        const entries = await api("GET", `/api/projects/${slug}/repo/tree?${rq}ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(path)}`);
        entries.sort((a, b) => (a.type === "tree" ? 0 : 1) - (b.type === "tree" ? 0 : 1) || a.name.localeCompare(b.name));
        el.appendChild(h("table", { class: "list" }, entries.map(e => {
          const full = (path ? path + "/" : "") + e.name;
          return h("tr", { class: "click", onclick: () => go(e.type === "tree" ? q({ path: full }) : q({ path: full, file: "1" })) },
            h("td", { style: { width: "28px" } }, e.type === "tree" ? "📁" : "📄"), h("td", { class: "mono" }, e.name),
            h("td", { class: "hint", style: { textAlign: "right" } }, e.type === "blob" ? (e.size > 1024 ? (e.size / 1024).toFixed(1) + " KB" : e.size + " B") : ""));
        })));
        const readme = entries.find(e => /^readme\.md$/i.test(e.name));
        if (readme) {
          const b = await api("GET", `/api/projects/${slug}/repo/blob?${rq}ref=${encodeURIComponent(ref)}&path=${encodeURIComponent((path ? path + "/" : "") + readme.name)}`);
          if (b.text) el.appendChild(h("div", { class: "card pad md", style: { marginTop: "14px" }, html: md(b.text) }));
        }
      }
    } else if (mode === "log") {
      const log = await api("GET", `/api/projects/${slug}/repo/log?${rq}ref=${encodeURIComponent(ref)}`);
      el.appendChild(h("table", { class: "list" }, log.map(c => h("tr", { class: "click", onclick: () => go(q({ commit: c.sha })) },
        h("td", { class: "mono hint", style: { width: "90px" } }, c.sha.slice(0, 8)), h("td", null, c.subject, " ", c.refs && h("span", { class: "chip" }, c.refs)),
        h("td", { class: "hint" }, c.author), h("td", { class: "hint", style: { whiteSpace: "nowrap" } }, ago(c.date))))));
    } else if (mode === "commit") {
      const txt = await api("GET", `/api/projects/${slug}/repo/commit?${rq}sha=${params.get("commit")}`);
      const pre = h("pre");
      for (const l of txt.split("\n")) {
        const cls = l.startsWith("+++") || l.startsWith("---") || l.startsWith("diff ") ? "fhead" : l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : l.startsWith("@@") ? "hunk" : "";
        pre.appendChild(h("div", { class: cls }, l || " "));
      }
      el.appendChild(h("div", { class: "code-view diff" }, pre));
    } else if (mode === "branches") {
      el.appendChild(h("table", { class: "list" },
        h("tr", null, h("th", null, "Branch"), h("th", null, "vs " + p.default_branch), h("th", null, "Last commit"), h("th", null, "")),
        branches.map(b => {
          const m = b.name.match(/^issue-(\d+)$/);
          return h("tr", null, h("td", { class: "mono" }, link(q({ ref: b.name, view: "log" }), b.name)),
            h("td", { class: "hint" }, b.name === p.default_branch ? "default" : `${b.ahead} ahead · ${b.behind} behind`),
            h("td", null, b.subject, h("span", { class: "hint" }, " · " + ago(b.date))),
            h("td", null, m && link(`/p/${slug}/i/${m[1]}`, `#${m[1]}`)));
        })));
    }
  }
  return { el, load };
}

// ---------- project settings ----------
function settingsView(info) {
  const p = info.project;
  const el = h("div", { class: "content" });
  const name = h("input", { value: p.name });
  const desc = h("textarea", { rows: 3, value: p.description });
  const instr = h("textarea", { rows: 8, value: p.instructions, placeholder: "Conventions, tech stack, how to build & test, what to avoid… Every agent gets this in its system prompt." });
  const review = h("select", null, [["agent", "Agent review — reviewer machines review, approved work is auto-merged"], ["human", "Human review — you approve & merge on the board"], ["none", "No review — merge as soon as the worker completes"]]
    .map(([v, n]) => h("option", { value: v, selected: p.review_mode === v }, n)));
  const dispatch = h("input", { type: "checkbox", checked: p.dispatch });
  let projModel = p.model || "";
  const modelIn = modelInput(projModel, "machine default (e.g. opus, sonnet)", v => projModel = v);
  modelIn.oninput = e => projModel = e.target.value.trim();
  const nodes = checkGroup(S.overview.nodes.map(n => [n.id, `${n.online ? "●" : "○"} ${n.name}`]), info.nodes, async (id, on) => {
    await api(on ? "PUT" : "DELETE", `/api/nodes/${id}/projects/${p.slug}`);
    toast(on ? "Machine assigned" : "Machine unassigned");
    on ? info.nodes.push(id) : info.nodes.splice(info.nodes.indexOf(id), 1);
  });
  add(el, [h("div", { style: { maxWidth: "720px" } },
    h("div", { class: "card pad" },
      h("label", { class: "field" }, h("span", null, "Name"), name),
      h("label", { class: "field" }, h("span", null, "Description"), desc),
      h("label", { class: "field" }, h("span", null, "Instructions for agents"), instr),
      h("label", { class: "field" }, h("span", null, "Review mode"), review),
      h("label", { class: "field" }, h("span", null, "Default model"), modelIn, h("span", { class: "hint" }, "Used for this project's sessions unless an issue sets its own. Empty = each machine's model setting (or Claude Code's default).")),
      h("label", { class: "toggle" }, dispatch, "Dispatch issues to machines automatically (uncheck to pause the project)"),
      h("div", { class: "row", style: { justifyContent: "flex-end", marginTop: "12px" } },
        h("button", { class: "btn primary", onclick: async () => {
          await api("PATCH", `/api/projects/${p.slug}`, { name: name.value, description: desc.value, instructions: instr.value, review_mode: review.value, dispatch: dispatch.checked, model: projModel });
          toast("Saved"); loadOverview();
        } }, "Save"))),
    h("div", { class: "section-title" }, "Repositories & mirrors"),
    reposSection(p),
    h("div", { class: "section-title" }, "Machines assigned to this project"),
    h("div", { class: "card pad" }, S.overview.nodes.length ? nodes : h("div", { class: "hint" }, "No machines registered yet. ", link("/machines", "Install a runner."))),
    h("div", { class: "section-title" }, "Danger zone"),
    h("div", { class: "card pad row" }, h("div", { class: "grow hint" }, "Deleting moves the repository and LFS objects to the server's trash folder."),
      h("button", { class: "btn danger", onclick: () => {
        const inp = h("input", { placeholder: p.slug });
        modal(`Delete ${p.name}?`, h("div", null, h("p", null, "Type the project slug to confirm:"), inp), close => [h("button", { class: "btn", onclick: close }, "Cancel"),
          h("button", { class: "btn danger solid", onclick: async () => { await api("DELETE", `/api/projects/${p.slug}?confirm=${encodeURIComponent(inp.value)}`); close(); await loadOverview(); go("/"); } }, "Delete")]);
      } }, "Delete project")))]);
  return { el, onEvent: ev => { if (ev.type === "repos" && ev.project_id === p.id) el.querySelector(".repos-sec")?.reload?.(); } };
}

function reposSection(p) {
  const box = h("div", { class: "repos-sec" });
  box.reload = async () => {
    const repos = await api("GET", `/api/projects/${p.slug}/repos`);
    box.innerHTML = "";
    repos.forEach(r => box.appendChild(repoCard(p, r, box.reload)));
    box.appendChild(addRepoCard(p, box.reload));
  };
  box.reload();
  return box;
}

function mirrorState(m) {
  if (!m.enabled) return h("span", { class: "chip" }, "paused");
  if (m.last_status === "ok") return h("span", { class: "chip ok" }, "✓ synced " + ago(m.last_at));
  if (m.last_status === "error") return h("span", { class: "chip bad", title: m.last_error }, "✗ failed " + ago(m.last_at));
  return h("span", { class: "chip" }, "pending…");
}

function repoCard(p, r, reload) {
  const base = `/api/projects/${p.slug}/repos/${encodeURIComponent(r.name)}`;
  const clone = `${location.origin}${r.clone_path}`;
  const mUrl = h("input", { placeholder: "owner/name (GitHub) or https://….git" });
  const mTok = h("input", { type: "password", placeholder: "token (default: server-wide GitHub token)", autocomplete: "off" });
  const mMode = h("select", { style: { width: "auto" } }, h("option", { value: "sync" }, "two-way sync (default branch + tags)"), h("option", { value: "default" }, "push only (default branch + tags)"), h("option", { value: "all" }, "push all branches & tags (forced)"));
  const mCreate = h("select", { style: { width: "auto" } }, h("option", { value: "" }, "repo exists"), h("option", { value: "private" }, "create private"), h("option", { value: "public" }, "create public"));
  return h("div", { class: "card pad", style: { marginBottom: "12px" } },
    h("div", { class: "row" }, h("b", null, "📦 " + r.name), r.primary ? h("span", { class: "chip" }, "primary") : null,
      h("span", { class: "hint" }, "default branch " + r.default_branch), h("span", { class: "spacer" }),
      link(`/p/${p.slug}/code${r.primary ? "" : "?repo=" + encodeURIComponent(r.name)}`, "Browse"),
      r.primary ? null : h("button", { class: "btn small danger ghost", onclick: async () => {
        if (confirm(`Remove repository ${r.name}? It is moved to the server's trash folder.`)) { await api("DELETE", base); reload(); }
      } }, "Remove")),
    r.description ? h("div", { class: "hint", style: { margin: "4px 0" } }, r.description) : null,
    h("div", { class: "install", style: { margin: "8px 0", padding: "4px 10px", fontSize: "12px" } }, h("code", null, `git clone ${clone}`), h("button", { class: "btn small ghost", onclick: () => copy(`git clone ${clone}`) }, "copy")),
    h("div", { class: "k hint", style: { margin: "10px 0 4px", fontWeight: "600" } }, "Mirrors"),
    r.mirrors.length ? h("div", null, r.mirrors.map(m => h("div", { class: "mirror-row" },
      h("div", { class: "row", style: { gap: "8px", flexWrap: "wrap" } },
        h("a", { href: m.url.replace(/\.git$/, ""), target: "_blank", rel: "noopener", style: { fontWeight: "600" } }, m.url.replace(/^https:\/\//, "").replace(/\.git$/, "")),
        m.visibility === "public" ? h("span", { class: "chip warn" }, "public") : m.visibility === "private" ? h("span", { class: "chip" }, "private") : null,
        h("span", { class: "hint", title: { sync: "Commits made on the mirror (e.g. merged pull requests) are pulled in; ours are pushed", default: "Only pushes; fails if the mirror has commits we don't", all: "Overwrites every branch and tag on the mirror" }[m.mode] }, { sync: "⇅ two-way sync", default: "→ push only", all: "→ push all (forced)" }[m.mode] || m.mode),
        mirrorState(m), h("span", { class: "spacer" }),
        h("button", { class: "btn small ghost", onclick: async () => { toast("Syncing…"); await api("POST", `${base}/mirrors/${m.id}/sync`); reload(); } }, "Sync now"),
        h("button", { class: "btn small ghost", onclick: async () => { await api("PATCH", `${base}/mirrors/${m.id}`, { enabled: !m.enabled }); reload(); } }, m.enabled ? "Pause" : "Resume"),
        h("button", { class: "btn small danger ghost", onclick: async () => { if (confirm("Remove this mirror? (The GitHub repository itself is not touched.)")) { await api("DELETE", `${base}/mirrors/${m.id}`); reload(); } } }, "Remove")),
      m.last_status === "error" ? h("div", { class: "hint", style: { color: "var(--bad)", whiteSpace: "pre-wrap", marginTop: "4px" } }, m.last_error) : null))) :
      h("div", { class: "hint" }, "No mirrors. Mirrors are one-way copies (e.g. on GitHub) that the server updates after every push and merge."),
    h("div", { class: "mirror-form" }, mUrl, mTok, mMode, mCreate,
      h("button", { class: "btn", onclick: async () => {
        if (!mUrl.value.trim()) return toast("Enter owner/name or a URL", true);
        await api("POST", `${base}/mirrors`, { url: mUrl.value, token: mTok.value, mode: mMode.value, create: !!mCreate.value, private: mCreate.value === "private" });
        toast("Mirror added — first sync running"); reload();
      } }, "Add mirror")));
}

function addRepoCard(p, reload) {
  const name = h("input", { placeholder: "name, e.g. backend" });
  const desc = h("input", { placeholder: "what it contains (agents see this)" });
  const branch = h("input", { placeholder: "default branch", title: "Leave empty: main, or the imported repository's own default branch", style: { width: "140px" } });
  const imp = h("input", { placeholder: "import from URL (optional)" });
  const tok = h("input", { type: "password", placeholder: "token for a private import (optional)", autocomplete: "off" });
  const syncBack = h("label", { class: "toggle" }, h("input", { type: "checkbox", checked: true }), "Add the imported repository as a two-way synced mirror");
  return h("details", { class: "card pad" }, h("summary", { style: { cursor: "pointer", fontWeight: "600" } }, "+ Add repository"),
    h("div", { style: { display: "grid", gap: "8px", marginTop: "10px" } },
      h("div", { class: "row", style: { gap: "6px" } }, name, branch), desc, h("div", { class: "row", style: { gap: "6px" } }, imp, tok), syncBack,
      h("div", { class: "hint" }, "Agents get every repository of the project checked out side by side and work on branch issue-N in the ones they change; approved issues are merged in all repositories at once."),
      h("div", { class: "row", style: { justifyContent: "flex-end" } }, h("button", { class: "btn primary", onclick: async () => {
        const r = await api("POST", `/api/projects/${p.slug}/repos`, { name: name.value, description: desc.value, default_branch: branch.value.trim(), import_url: imp.value, import_token: tok.value });
        if (imp.value.trim() && syncBack.querySelector("input").checked)
          await api("POST", `/api/projects/${p.slug}/repos/${r.name}/mirrors`, { url: imp.value.trim(), token: tok.value, mode: "sync" }).catch(e => toast("Mirror not added: " + e.message, true));
        toast("Repository created"); reload();
      } }, "Create repository"))));
}

// ---------- machines ----------
// versionChip: does this machine's runner match the server, and if not, will it catch up by itself?
function versionChip(n) {
  const sv = S.overview.version || "", rv = n.runner_version || "";
  if (!rv || !sv) return null;
  if (sv === "dev" || rv === "dev")
    return h("span", { class: "chip", title: `Development build (server ${sv}, runner ${rv}) — versions are not compared and runners don't update themselves` }, `runner ${rv}`);
  if (rv === sv) return h("span", { class: "chip ok", title: `The runner runs the same version as the server (${sv})` }, `✓ ${rv}`);
  let label = `runner ${rv} · server ${sv}`, title;
  if (n.updating_to) {
    label = `updating ${rv} → ${n.updating_to}`;
    title = "The runner updates itself as soon as its running sessions are done. It takes no new work for up to 15 minutes while waiting, then works normally again and updates when it is next idle.";
  } else if (!n.online) {
    title = "Offline. The runner updates itself when it comes back (unless self-update is disabled there).";
  } else if (n.self_update === true) {
    title = "Outdated. The runner updates itself the next time it has no running sessions (it holds back new work for up to 15 minutes every 2 hours to get there).";
  } else if (n.self_update === false) {
    title = "Outdated, and this runner won't update itself: MC_NO_SELF_UPDATE is set, it is a dev build, or its last update attempt failed (see journalctl --user -u multiclaude-runner). It retries a failed update after 15 minutes; otherwise re-run the install command on that machine.";
  } else {
    title = "Outdated. This runner is too old to report whether it updates itself; re-run the install command on that machine if it stays like this.";
  }
  return h("span", { class: "chip " + (n.self_update === false ? "bad" : "warn"), title }, label);
}

function machinesView() {
  const content = h("div", { class: "content" });
  const el = h("div", { style: { display: "contents" } },
    h("div", { class: "topbar", style: { paddingBottom: "14px" } }, h("h1", null, "Machines"), h("span", { class: "sub" }, "Computers running Claude Code that work on your projects")), content);
  const editing = new Set();
  let setup = null, runnerSettings = { workspace_dir: "" };
  const loadRunnerSettings = () => api("GET", "/api/settings/runners").then(x => { runnerSettings = x; render(true); });
  const loadSetup = (rotate) => api(rotate ? "POST" : "GET", rotate ? "/api/runner-setup/rotate" : "/api/runner-setup").then(x => { setup = x; render(true); });
  function render(force) {
    if (!force && [...content.querySelectorAll("input")].some(i => i === document.activeElement)) return;
    const cmd = setup ? setup.install : "loading…";
    content.innerHTML = "";
    add(content, [
      h("div", { class: "card pad", style: { marginBottom: "18px" } },
        h("b", null, "Add a machine"),
        h("p", { class: "hint", style: { margin: "4px 0 10px" } }, "Run this on a Linux or macOS computer, as the user that is logged into Claude Code (subscription). It enrolls the machine with the join token below, then installs the runner as a user service. Then assign the machine to projects below."),
        h("div", { class: "install" }, h("code", null, cmd), h("button", { class: "btn small", onclick: () => copy(cmd) }, "Copy")),
        h("p", { class: "hint", style: { margin: "8px 0 0" } }, "Options: ", h("code", null, `… --token … --name gpu-box --roles worker --concurrency 4 --caps unity`), ". Roles: ", h("code", null, "worker"), " implements issues, ", h("code", null, "reviewer"), " reviews them."),
        h("p", { class: "hint", style: { margin: "8px 0 0" } }, "The command uses the address you opened this page with — open the UI via your public HTTPS address to enroll machines outside your LAN."),
        h("p", { class: "hint", style: { margin: "8px 0 0" } }, "Without the installer (e.g. Windows/WSL, containers): ", h("code", null, setup ? setup.manual : "…")),
        h("div", { class: "row", style: { marginTop: "10px", gap: "8px" } },
          h("span", { class: "hint" }, "The join token only enrolls new machines; each machine then gets its own token. Removing a machine revokes it."),
          h("button", { class: "btn small ghost", style: { marginLeft: "auto" }, onclick: () => { if (confirm("Create a new join token? The old install command stops working; enrolled machines keep working.")) loadSetup(true).then(() => toast("New join token created")); } }, "Regenerate join token"))),
      h("div", { class: "card pad", style: { marginBottom: "18px" } },
        h("b", null, "Work directory"),
        h("p", { class: "hint", style: { margin: "4px 0 10px" } }, "Where every machine keeps its project checkouts (one folder per project, one slot per parallel agent). ",
          h("code", null, "~"), " and ", h("code", null, "$VARS"), " are expanded on each machine. Leave empty for the runner's default (", h("code", null, "~/.multiclaude/workspaces"),
          "). A machine can override it on its card. Takes effect for new sessions; existing checkouts are not moved (the new location is cloned fresh)."),
        h("input", { value: runnerSettings.workspace_dir || "", placeholder: "e.g. /data/multiclaude or ~/work/multiclaude", style: { width: "100%", maxWidth: "520px" },
          onchange: e => api("PUT", "/api/settings/runners", { workspace_dir: e.target.value }).then(x => { runnerSettings = x; toast("Saved — applies to new sessions"); render(true); }) })),
      S.overview.nodes.length ? h("div", { class: "grid", style: { gridTemplateColumns: "repeat(auto-fill,minmax(400px,1fr))" } }, S.overview.nodes.map(machineCard)) :
        h("div", { class: "card empty" }, "No machines yet."),
    ]);
  }
  function machineCard(n) {
    const hw = n.hardware || {};
    const save = patch => api("PATCH", `/api/nodes/${n.id}`, patch).then(() => toast("Saved"));
    const roles = checkGroup([["worker", "worker"], ["reviewer", "reviewer"]], n.roles, (v, on, all) => save({ roles: all }));
    const projects = checkGroup(S.overview.projects.map(p => [p.id, p.name]), n.projects, async (pid, on) => {
      const p = S.overview.projects.find(x => x.id === pid);
      await api(on ? "PUT" : "DELETE", `/api/nodes/${n.id}/projects/${p.slug}`);
      toast(on ? `Assigned to ${p.name}` : `Removed from ${p.name}`);
    });
    const conc = h("input", { type: "number", min: 1, max: 64, value: n.max_conc, style: { width: "70px" }, onchange: e => save({ max_conc: +e.target.value }) });
    const extra = h("input", { value: n.extra_caps.join(", "), placeholder: "extra tags, e.g. unity, prod-db", onchange: e => save({ extra_caps: e.target.value.split(",").map(x => x.trim()).filter(Boolean) }) });
    const model = h("input", { value: n.model, placeholder: "default (e.g. opus, sonnet)", onchange: e => save({ model: e.target.value }) });
    const wsDir = h("input", { value: n.workspace_dir || "", placeholder: runnerSettings.workspace_dir ? `default: ${runnerSettings.workspace_dir}` : "default (~/.multiclaude/workspaces)",
      title: "Where this machine keeps project checkouts — overrides the server-wide work directory. Applies to new sessions.", onchange: e => save({ workspace_dir: e.target.value }) });
    const authOk = /subscription|keychain|env/.test(n.claude_auth || "");
    return h("div", { class: "card pad machine" },
      h("div", { class: "hdr" }, h("span", { class: "dot " + (n.online ? (n.active.length ? "busy" : "on") : "") }), h("h3", null, n.name),
        h("span", { class: "hint" }, n.online ? (n.active.length ? `${n.active.length}/${n.max_conc} busy` : "idle") : `offline · seen ${ago(n.last_seen)}`),
        versionChip(n),
        h("span", { class: "spacer" }),
        n.paused && h("span", { class: "chip warn" }, "paused"),
        h("button", { class: "btn small", onclick: () => save({ paused: !n.paused }) }, n.paused ? "Resume" : "Pause")),
      h("div", { class: "kv" },
        h("div", { class: "k" }, "Claude"), h("div", null, n.claude_version || "?", " ", h("span", { class: "chip " + (authOk ? "ok" : "bad") }, n.claude_auth || "auth unknown")),
        h("div", { class: "k" }, "Account"), h("div", null, n.claude_account || h("span", { class: "hint" }, n.online ? "unknown (update the runner)" : "unknown")),
        h("div", { class: "k" }, "Load"), loadBlock(n),
        h("div", { class: "k" }, "CPU"), h("div", null, `${hw.cpu_model || "?"} · ${hw.cpu_cores} cores`, (hw.cpu_features || []).length ? h("span", { class: "hint" }, " · " + hw.cpu_features.join(", ")) : null),
        h("div", { class: "k" }, "Memory"), h("div", null, `${hw.memory_gb} GB · ${hw.disk_free_gb} GB disk free`),
        h("div", { class: "k" }, (hw.gpus || []).length > 1 ? `GPUs (${hw.gpus.length})` : "GPU"), h("div", null, (hw.gpus || []).length ? gpuList(n, hw.gpus, save) : h("span", { class: "hint" }, "none")),
        h("div", { class: "k" }, "System"), h("div", null, `${hw.os}/${hw.arch} · runner ${n.runner_version}`,
          [hw.cuda_version && ` · CUDA ≤ ${hw.cuda_version}`, hw.cuda_toolkit && ` · nvcc ${hw.cuda_toolkit}`, hw.rocm_version && ` · ROCm ${hw.rocm_version}`,
           (hw.gpus || [])[0]?.driver && ` · driver ${hw.gpus[0].driver}`].filter(Boolean).join(""))),
      h("div", { class: "row wrap", style: { gap: "4px", marginBottom: "4px" } },
        [...n.caps, ...n.extra_caps].map(c => {
          const off = (n.disabled_caps || []).some(d => d.toLowerCase() === c.toLowerCase());
          const extraTag = n.extra_caps.includes(c) && !n.caps.includes(c);
          return h("span", { class: "chip click cap " + (off ? "off" : extraTag ? "accent" : "req"),
            title: off ? `"${c}" is switched off — click to enable it for scheduling again` : `Click to switch "${c}" off (no new work needing it is sent here; running agents continue)`,
            onclick: () => {
              // "gpu" switches the whole GPU family (cuda, nvidia, rocm, mps) so GPU work really stops.
              const fam = c.toLowerCase() === "gpu" ? [...n.caps, ...n.extra_caps].filter(x => GPU_CAPS.includes(x.toLowerCase())) : [c];
              const lc = fam.map(x => x.toLowerCase());
              save({ disabled_caps: off ? (n.disabled_caps || []).filter(d => !lc.includes(d.toLowerCase())) : [...new Set([...(n.disabled_caps || []), ...fam])] });
            } },
            off ? "⊘ " + c : c);
        })),
      h("div", { class: "hint", style: { marginBottom: "12px" } }, (n.disabled_caps || []).length
        ? `Switched off: ${n.disabled_caps.join(", ")} — issues requiring them won't be sent to this machine. Click a tag to toggle.`
        : "Click a capability to switch it off temporarily (e.g. click gpu to reserve the GPU for something else)."),
      h("div", { class: "props", style: { gridTemplateColumns: "100px 1fr", margin: "0" } },
        h("div", { class: "k" }, "Projects"), S.overview.projects.length ? projects : h("span", { class: "hint" }, "no projects yet"),
        h("div", { class: "k" }, "Roles"), roles,
        h("div", { class: "k" }, "Parallel agents"), conc,
        h("div", { class: "k" }, "Extra tags"), extra,
        h("div", { class: "k" }, "Model"), model,
        h("div", { class: "k" }, "Work directory"), wsDir),
      n.active.length ? h("div", { style: { marginTop: "12px" } }, n.active.map(r => h("div", { class: "row", style: { fontSize: "13px", padding: "2px 0" } },
        h("span", { class: "dot busy" }), link(`/runs/${r.id}`, r.issue_number ? `${r.kind} #${r.issue_number} ${r.issue_title}` : `chat session`), h("span", { class: "hint" }, r.project_slug + " · " + ago(r.started_at))))) : null,
      h("div", { class: "row", style: { justifyContent: "flex-end", marginTop: "10px" } },
        h("button", { class: "btn small danger ghost", onclick: async () => { if (confirm(`Remove ${n.name}? This revokes its token: the runner on that machine stops working until it is enrolled again.`)) { await api("DELETE", `/api/nodes/${n.id}`); loadOverview().then(render); } } }, "Remove")));
  }
  return { el, load: async () => { await loadOverview(); render(); loadSetup(); loadRunnerSettings(); }, onEvent: debounce(ev => { if (ev.type === "nodes" || ev.type === "projects") loadOverview().then(() => render()); }, 400) };
}

// ---------- users & account ----------
function accountView() {
  const content = h("div", { class: "content" });
  const el = h("div", { style: { display: "contents" } },
    h("div", { class: "topbar", style: { paddingBottom: "14px" } }, h("h1", null, "Users & account"), h("span", { class: "sub" }, "Everyone with an account has full access")), content);
  const pwInput = ph => h("input", { type: "password", placeholder: ph, autocomplete: "new-password" });
  async function render() {
    const users = await api("GET", "/api/users");
    const cur = h("input", { type: "password", placeholder: "Current password", autocomplete: "current-password" });
    const np = pwInput("New password (min. 8 characters)"), np2 = pwInput("Repeat new password");
    const nu = h("input", { placeholder: "Username" }), nupw = pwInput("Password (min. 8 characters)");
    const host = location.origin;
    content.innerHTML = "";
    add(content, [
      h("div", { class: "card pad", style: { marginBottom: "18px", maxWidth: "640px" } },
        h("b", null, "Change my password"), h("span", { class: "hint" }, ` (${S.user.username})`),
        h("div", { style: { display: "grid", gap: "8px", margin: "10px 0" } }, cur, np, np2),
        h("button", { class: "btn primary", onclick: async () => {
          if (np.value !== np2.value) return toast("The new passwords don't match", true);
          await api("PATCH", `/api/users/${S.user.id}`, { current: cur.value, password: np.value });
          toast("Password changed — other sessions were logged out"); render();
        } }, "Change password")),
      h("div", { class: "card pad", style: { marginBottom: "18px", maxWidth: "640px" } },
        h("b", null, "Users"),
        h("table", { class: "list", style: { margin: "8px 0 14px" } }, h("tbody", null, users.map(u => h("tr", null,
          h("td", null, h("b", null, u.username), u.id === S.user.id ? h("span", { class: "hint" }, " (you)") : null),
          h("td", { class: "hint" }, "since " + new Date(u.created_at).toLocaleDateString()),
          h("td", { style: { textAlign: "right" } },
            u.id === S.user.id ? null : h("button", { class: "btn small ghost", onclick: async () => {
              const pw = prompt(`New password for ${u.username} (min. 8 characters):`);
              if (pw) { await api("PATCH", `/api/users/${u.id}`, { password: pw }); toast("Password set"); }
            } }, "Set password"),
            u.id === S.user.id ? null : h("button", { class: "btn small danger ghost", onclick: async () => {
              if (confirm(`Delete user ${u.username}?`)) { await api("DELETE", `/api/users/${u.id}`); render(); }
            } }, "Delete")))))),
        h("div", { class: "row", style: { gap: "8px" } }, nu, nupw,
          h("button", { class: "btn", onclick: async () => { await api("POST", "/api/users", { username: nu.value, password: nupw.value }); toast("User added"); render(); } }, "Add user"))),
      githubCard(),
      h("div", { class: "card pad", style: { maxWidth: "640px" } },
        h("b", null, "Git access"),
        h("p", { class: "hint", style: { margin: "6px 0" } }, "Clone a project's repository with your username and password (git asks for them; a credential helper remembers them):"),
        h("div", { class: "install" }, h("code", null, `git clone ${host}/git/<project>.git`)),
        h("p", { class: "hint", style: { margin: "6px 0 0" } }, "Machines authenticate with their own tokens; you don't need to set anything up for them.")),
    ]);
  }
  return { el, load: render };
}

function githubCard() {
  const card = h("div", { class: "card pad", style: { marginBottom: "18px", maxWidth: "640px" } });
  const render = st => {
    const tok = h("input", { type: "password", placeholder: st.has_token ? "•••••• (stored) — paste a new one to replace" : "GitHub token", autocomplete: "off" });
    card.innerHTML = "";
    add(card, [h("b", null, "GitHub"),
      h("p", { class: "hint", style: { margin: "6px 0" } }, "Server-wide token for mirrors and private imports (a mirror can also have its own). Fine-grained token: the repositories (or all of the organization) with Contents: read & write, plus Administration: read & write if MultiClaude should create mirror repositories."),
      st.has_token ? h("div", { style: { margin: "6px 0" } }, st.error ? h("span", { class: "chip bad" }, "✗ " + st.error) : h("span", { class: "chip ok" }, "✓ authenticated as " + st.login)) : null,
      h("div", { class: "row", style: { gap: "6px" } }, tok,
        h("button", { class: "btn", onclick: async () => { render(await api("PUT", "/api/settings/github", { token: tok.value })); toast("Saved"); } }, "Save"),
        st.has_token ? h("button", { class: "btn ghost danger", onclick: async () => { render(await api("PUT", "/api/settings/github", { token: "" })); } }, "Remove") : null)]);
  };
  api("GET", "/api/settings/github").then(render);
  return card;
}

// One line per GPU: which run holds it; click to reserve it for something else ("gpu#i" switched off).
// ---------- machine load (live, every ~10 s) ----------
function applyLoad(nodeId, load) {
  const n = S.overview.nodes.find(x => x.id === nodeId);
  if (n) n.load = load;
  document.querySelectorAll(`[data-load="${CSS.escape(nodeId)}"]`).forEach(el => { el.replaceWith(loadBlock(n || { id: nodeId, load, hardware: {} })); });
}
function meter(label, pct, text, title) {
  pct = Math.max(0, Math.min(100, pct || 0));
  const lvl = pct >= 90 ? "bad" : pct >= 70 ? "warn" : "ok";
  return h("div", { class: "meter", title: title || "" }, h("span", { class: "ml" }, label),
    h("span", { class: "bar" }, h("span", { class: "fill " + lvl, style: { width: pct + "%" } })), h("span", { class: "mv" }, text));
}
const gb = v => v >= 1000 ? (v / 1000).toFixed(1) + " TB" : Math.round(v) + " GB";
function loadBlock(n) {
  const l = n.load;
  if (!l || !n.online) return h("div", { "data-load": n.id, class: "hint" }, n.online ? "waiting for the first report… (needs runner ≥ this version)" : "—");
  const gpus = (n.hardware && n.hardware.gpus) || [];
  const seen = {};
  const num = gpus.map((g, i) => { const idx = g.has_index ? g.index : (seen[g.vendor] || 0); seen[g.vendor] = (seen[g.vendor] || 0) + 1; return [g.vendor + ":" + idx, i]; });
  const byKey = Object.fromEntries(num);
  const pct = (a, b) => b ? 100 * a / b : 0;
  return h("div", { "data-load": n.id, class: "meters" },
    meter("CPU", l.cpu_pct, `${Math.round(l.cpu_pct)}% · load ${l.load1}`, "CPU utilisation over all cores · 1-minute load average"),
    meter("RAM", pct(l.mem_used_gb, l.mem_total_gb), `${l.mem_used_gb} / ${l.mem_total_gb} GB`),
    l.disk_total_gb ? meter("Disk", pct(l.disk_used_gb, l.disk_total_gb), `${gb(l.disk_used_gb)} / ${gb(l.disk_total_gb)}`, "All local disks together") : null,
    (l.gpus || []).flatMap(g => {
      const i = byKey[g.vendor + ":" + g.index] ?? g.index;
      const lab = (l.gpus.length > 1 ? `GPU ${i}` : "GPU");
      return [meter(lab, g.util_pct, `${Math.round(g.util_pct)}%`, (gpus[i] || {}).name || ""),
        meter(l.gpus.length > 1 ? `VRAM ${i}` : "VRAM", pct(g.mem_used_mb, g.mem_total_mb), `${(g.mem_used_mb / 1024).toFixed(1)} / ${Math.round(g.mem_total_mb / 1024)} GB`)];
    }),
    h("div", { class: "hint", style: { fontSize: "11px" } }, "updated ", h("span", { "data-ago": l.ts }, ago(l.ts))));
}

// ---------- questions from agents ----------
function questionsView() {
  const content = h("div", { class: "content" });
  const el = h("div", { style: { display: "contents" } },
    h("div", { class: "topbar", style: { paddingBottom: "14px" } }, h("h1", null, "Questions"), h("span", { class: "sub" }, "Agents ask here when they need a human decision")), content);
  const drafts = {};
  async function render() {
    if ([...content.querySelectorAll("textarea")].some(t => t === document.activeElement)) return;
    const [open, recent] = await Promise.all([api("GET", "/api/questions"), api("GET", "/api/questions?status=recent")]);
    content.innerHTML = "";
    add(content, [
      open.length ? open.map(qCard) : h("div", { class: "card empty" }, "No open questions. Agents ask with ask_human when they need a decision; they show up here, in the sidebar and in the project chat."),
      recent.length ? [h("div", { class: "section-title" }, "Recently answered"), recent.map(doneCard)] : null,
    ]);
  }
  function where(q) {
    return [link(`/p/${q.project_slug}`, q.project_name), " · 🤖 ", q.agent,
      q.issue_number ? [" · ", link(`/p/${q.project_slug}/i/${q.issue_number}`, `#${q.issue_number} ${q.issue_title}`)] : " · chat session",
      " · ", ago(q.created_at)];
  }
  function qCard(q) {
    const ta = h("textarea", { rows: 3, placeholder: "Your answer… (Ctrl+Enter to send)", style: { width: "100%" }, oninput: e => drafts[q.id] = e.target.value });
    ta.value = drafts[q.id] || "";
    const send = async (answer, dismiss) => {
      const r = await api("POST", `/api/questions/${q.id}/answer`, dismiss ? { dismiss: true, answer } : { answer });
      delete drafts[q.id];
      toast(`Answer ${r.delivery}`);
      render();
    };
    ta.addEventListener("keydown", e => { if (e.key === "Enter" && (e.ctrlKey || e.metaKey) && ta.value.trim()) send(ta.value.trim()); });
    const state = q.waiting ? h("span", { class: "chip warn" }, "⏳ agent is waiting") :
      q.run_status === "running" ? h("span", { class: "chip" }, "agent continues — answer is delivered live") :
      h("span", { class: "chip", title: "The session has ended" }, q.issue_number ? "session ended — answer goes to the issue" : "session ended — answer starts a chat session");
    return h("div", { class: "card pad question" },
      h("div", { class: "row wrap", style: { gap: "6px", marginBottom: "6px" } }, h("b", null, `Q${q.id}`), h("span", { class: "hint" }, where(q)), h("span", { class: "spacer" }), state,
        q.run_status === "running" && link(`/runs/${q.run_id}`, "watch")),
      h("div", { class: "md q-text", html: md(q.question) }),
      q.context && h("details", null, h("summary", { class: "hint" }, "Context from the agent"), h("div", { class: "md hint", html: md(q.context) })),
      q.options.length ? h("div", { class: "row wrap", style: { gap: "6px", margin: "8px 0" } }, q.options.map(o => h("button", { class: "btn small", onclick: () => send(o) }, o))) : null,
      ta,
      h("div", { class: "row", style: { gap: "8px", marginTop: "6px", justifyContent: "flex-end" } },
        h("button", { class: "btn small ghost", title: "Tell the agent to decide itself", onclick: () => send(ta.value.trim(), true) }, "Dismiss — decide yourself"),
        h("button", { class: "btn small primary", onclick: () => ta.value.trim() ? send(ta.value.trim()) : toast("Write an answer first", true) }, "Send answer")));
  }
  function doneCard(q) {
    return h("div", { class: "card pad", style: { opacity: .8 } },
      h("div", { class: "row wrap", style: { gap: "6px" } }, h("b", null, `Q${q.id}`), h("span", { class: "hint" }, where(q)), h("span", { class: "spacer" }),
        h("span", { class: "chip " + (q.status === "answered" ? "ok" : "") }, q.status === "answered" ? `answered by ${q.answered_by}` : `dismissed by ${q.answered_by}`)),
      h("div", { class: "md", html: md(q.question) }),
      q.answer && h("div", { class: "md answer", html: md(q.answer) }));
  }
  return { el, load: render, onEvent: debounce(ev => { if (ev.type === "questions") render(); }, 300) };
}

function gpuList(n, gpus, save) {
  const holder = {};
  for (const r of n.active) for (const g of r.gpus || []) holder[g] = r;
  return h("div", { class: "gpus" }, gpus.map((g, i) => {
    const tag = `gpu#${i}`, off = (n.disabled_caps || []).some(d => d.toLowerCase() === tag), r = holder[i];
    return h("div", { class: "row", style: { gap: "6px", fontSize: "13px" } },
      h("span", { class: "mono hint" }, `#${i}`), h("span", null, g.name + (g.memory_mb ? ` · ${Math.round(g.memory_mb / 1024)} GB` : "")),
      g.arch && g.vendor !== "apple" ? h("span", { class: "chip req", title: "Architecture — usable in requires, e.g. " + g.arch.split(" ")[0].toLowerCase() }, g.arch) : null,
      g.compute_cap ? h("span", { class: "chip", title: g.vendor === "nvidia" ? `Compute capability ${g.compute_cap} (requires sm_${g.compute_cap.replace(".", "")} or sm>=…)` : g.vendor === "amd" ? "AMD target" : "Metal" },
        g.vendor === "nvidia" ? `sm_${g.compute_cap.replace(".", "")}` : g.compute_cap) : null,
      r ? h("span", { class: "chip warn", title: `run ${r.id}` }, r.issue_number ? `busy: ${r.project_slug} #${r.issue_number}` : `busy: ${r.kind}`) : off ? null : h("span", { class: "chip ok" }, "free"),
      gpus.length > 1 ? h("span", { class: "chip click cap " + (off ? "off" : ""), title: off ? "Reserved: no agent gets this GPU. Click to release it." : "Click to reserve this GPU (agents won't get it; running ones continue)",
        onclick: () => save({ disabled_caps: off ? n.disabled_caps.filter(d => d.toLowerCase() !== tag) : [...(n.disabled_caps || []), tag] }) }, off ? "⊘ reserved" : "reserve") : null);
  }));
}

// ---------- login ----------
function showLogin(msg) {
  if (document.getElementById("login")) return;
  const user = h("input", { placeholder: "Username", autocomplete: "username", value: "" });
  const pw = h("input", { type: "password", placeholder: "Password", autocomplete: "current-password" });
  const err = h("div", { class: "login-err" }, msg || "");
  const submit = async e => {
    e.preventDefault();
    err.textContent = "";
    const r = await fetch("/api/login", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ username: user.value, password: pw.value }) });
    if (r.ok) { location.reload(); return; }
    try { err.textContent = (await r.json()).error; } catch { err.textContent = "Login failed"; }
    pw.value = ""; pw.focus();
  };
  document.body.innerHTML = "";
  document.body.appendChild(h("div", { id: "login" },
    h("form", { class: "card login-card", onsubmit: submit },
      h("div", { class: "brand", style: { padding: "0 0 14px" } }, h("div", { class: "logo", html: `<svg width="16" height="16" viewBox="0 0 32 32"><circle cx="10" cy="11" r="4" fill="white"/><circle cx="22" cy="11" r="4" fill="white"/><circle cx="16" cy="22" r="4" fill="white"/></svg>` }), "MultiClaude"),
      h("label", { class: "field" }, h("span", null, "Username"), user),
      h("label", { class: "field" }, h("span", null, "Password"), pw),
      err,
      h("button", { class: "btn primary", type: "submit", style: { width: "100%", justifyContent: "center", marginTop: "6px" } }, "Log in"))));
  setTimeout(() => user.focus(), 0);
}

// ---------- boot ----------
(async function boot() {
  const auth = await fetch("/api/auth").then(r => r.json()).catch(() => ({}));
  if (!auth.user) return showLogin();
  S.user = auth.user;
  await loadOverview().catch(() => {});
  connectEvents();
  route();
  setInterval(() => { if (S.view && S.view.el.isConnected && location.pathname === "/machines") loadOverview(); }, 30000);
})();
