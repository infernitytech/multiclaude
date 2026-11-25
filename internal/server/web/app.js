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
  const busy = S.overview.nodes.reduce((a, n) => a + n.active.length, 0);
  add(sb, [
    link("/", h("div", { class: "brand" }, h("div", { class: "logo", html: `<svg width="16" height="16" viewBox="0 0 32 32"><circle cx="10" cy="11" r="4" fill="white"/><circle cx="22" cy="11" r="4" fill="white"/><circle cx="16" cy="22" r="4" fill="white"/></svg>` }), "MultiClaude")),
    h("div", { class: "nav" },
      link("/", "Overview"),
      link("/machines", "Machines", h("span", { class: "count" }, `${online} on · ${busy} busy`))),
    h("div", { class: "nav-section" }, "Projects"),
    h("div", { class: "nav" },
      S.overview.projects.map(p => {
        const c = p.counts || {};
        const active = (c.in_progress || 0) + (c.review || 0);
        return link(`/p/${p.slug}`, p.name, h("span", { class: "count" }, active ? `${active} active` : (c.todo || "")));
      }),
      h("a", { href: "#", onclick: e => { e.preventDefault(); newProjectModal(); } }, "+ New project")),
    h("div", { class: "sidebar-foot" }, `v${S.overview.version || ""} · spent ${money(S.overview.total_cost_usd)}`),
  ]);
  sb.querySelectorAll(".nav a").forEach(a => {
    const href = a.getAttribute("href");
    if (href === "/" ? path === "/" : path === href || path.startsWith(href + "/")) a.classList.add("active");
  });
}

// ---------- events (SSE) ----------
function connectEvents() {
  const es = new EventSource("/api/events");
  const refreshOverview = debounce(loadOverview, 300);
  es.onmessage = e => {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }
    if (["nodes", "projects", "issues", "runs"].includes(ev.type)) refreshOverview();
    S.view && S.view.onEvent && S.view.onEvent(ev);
    S.drawer && S.drawer.onEvent && S.drawer.onEvent(ev);
  };
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
  const imp = h("input", { placeholder: "optional: https://github.com/you/repo.git" });
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
    h("label", { class: "field" }, h("span", null, "Review mode"), review),
    h("div", { class: "field" }, h("span", { class: "hint" }, "Machines working on this project"),
      S.overview.nodes.length ? nodes : h("div", { class: "hint" }, "No machines registered yet — install a runner from the Machines page."))),
    close => [h("button", { class: "btn", onclick: close }, "Cancel"),
      h("button", { class: "btn primary", onclick: async e => {
        e.target.disabled = true;
        try {
          const p = await api("POST", "/api/projects", { name: name.value, slug: slug.value, description: desc.value, import_url: imp.value, review_mode: review.value, nodes: nodes.values() });
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
  return h("table", { class: "list" },
    h("tr", null, h("th", null, ""), h("th", null, "Kind"), h("th", null, "Task"), showProject && h("th", null, "Project"), h("th", null, "Machine"), h("th", null, "Started"), h("th", null, "Duration"), h("th", null, "Cost")),
    runs.map(r => h("tr", { class: "click", onclick: () => go(`/runs/${r.id}`) },
      h("td", null, runDot(r)),
      h("td", null, h("span", { class: "chip " + (r.kind === "review" ? "req" : r.kind === "chat" ? "accent" : "") }, r.kind)),
      h("td", null, r.issue_number ? `#${r.issue_number} ${r.issue_title}` : h("span", { class: "hint" }, "chat session")),
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
  const tabs = [["board", "Board"], ["chat", "Chat"], ["runs", "Runs"], ["code", "Code"], ["settings", "Settings"]];
  const head = h("div", { class: "topbar" });
  const body = h("div", { class: "content flush" });
  const el = h("div", { style: { display: "contents" } }, head, body);
  let inner;
  const views = { board: boardView, chat: chatView, runs: projectRunsView, code: codeView, settings: settingsView };
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
    else if (/^\+[\w.-]+$/.test(w)) out.requires.push(w.slice(1));
    else out.title.push(w);
  }
  return { ...out, title: out.title.join(" ") };
}

function boardView(info) {
  const slug = info.project.slug;
  let issues = [];
  let filter = "";
  const boardEl = h("div", { class: "board" });
  const filterIn = h("input", { placeholder: "Filter issues…  (press / )", style: { maxWidth: "280px" }, oninput: e => { filter = e.target.value.toLowerCase(); render(); } });
  const noMachines = h("div");
  const el = h("div", { style: { display: "flex", flexDirection: "column", height: "100%" } },
    h("div", { class: "row", style: { padding: "12px 20px 0" } }, filterIn,
      h("span", { class: "hint" }, "Quick add: ", h("kbd", null, "n"), "  ·  ", h("code", null, "!high"), " priority  ", h("code", null, "#label"), "  ", h("code", null, "+gpu"), " requires capability"),
      h("span", { class: "spacer" }), noMachines),
    h("div", { style: { flex: 1, minHeight: 0 } }, boardEl));

  let dragId = null;
  function card(i) {
    const run = i.run_id && i.assignee;
    return h("div", {
      class: `issue-card p${i.priority}`, draggable: "true", "data-id": i.id,
      ondragstart: e => { dragId = i.id; e.dataTransfer.effectAllowed = "move"; e.target.classList.add("dragging"); },
      ondragend: e => { e.target.classList.remove("dragging"); },
      onclick: () => openIssue(slug, i.number),
    },
      h("div", { class: "row", style: { gap: "6px" } }, h("span", { class: "num" }, `#${i.number}`),
        i.attempts > 0 && i.status !== "done" && h("span", { class: "chip warn", title: "failed attempts" }, `↻${i.attempts}`),
        h("span", { class: "spacer" }), run && h("span", { class: "dot busy", title: `${i.assignee} is on it` })),
      h("div", { class: "t" }, i.title),
      h("div", { class: "meta" },
        i.labels.map(l => h("span", { class: "chip" }, l)),
        i.requires.map(r => h("span", { class: "chip req", title: "requires capability" }, "⚙ " + r)),
        i.depends_on.length ? h("span", { class: "chip", title: "depends on" }, "⛓ " + i.depends_on.map(d => "#" + d).join(" ")) : null,
        i.assignee && (i.status === "in_progress" || i.status === "review") ? h("span", { style: { marginLeft: "auto" } }, "🤖 " + i.assignee) : null));
  }

  function column([status, name]) {
    let list = issues.filter(i => i.status === status && (!filter || (`#${i.number} ${i.title} ${i.labels.join(" ")} ${i.requires.join(" ")} ${i.assignee}`).toLowerCase().includes(filter)));
    list.sort((a, b) => status === "done" ? b.updated_at - a.updated_at : (a.sort - b.sort) || (a.number - b.number));
    const body = h("div", { class: "col-body" }, list.map(card));
    const col = h("div", { class: "col" },
      h("div", { class: "col-head" }, h("span", { class: "dot", style: { background: STATUS_COLOR[status] } }), name, h("span", { class: "n" }, list.length)),
      (status === "todo" || status === "backlog") && quickAdd(status),
      body);
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
      Object.assign(it, patch); render();
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

  function render() {
    const scroll = boardEl.scrollLeft;
    boardEl.innerHTML = "";
    add(boardEl, STATUSES.map(column));
    boardEl.scrollLeft = scroll;
    const assigned = info.nodes.length;
    noMachines.innerHTML = "";
    if (!assigned) add(noMachines, [h("span", { class: "chip warn" }, "No machines assigned — "), " ", link(`/p/${slug}/settings`, "assign machines")]);
    else if (!info.project.dispatch) add(noMachines, [h("span", { class: "chip warn" }, "Dispatch paused")]);
  }
  async function load() { issues = await api("GET", `/api/projects/${slug}/issues`); render(); }
  const onKey = e => {
    if (e.target.matches("input,textarea,select") || e.metaKey || e.ctrlKey || S.drawer) return;
    if (e.key === "n") { e.preventDefault(); $("input[data-quick=todo]")?.focus(); }
    if (e.key === "/") { e.preventDefault(); filterIn.focus(); }
  };
  document.addEventListener("keydown", onKey);
  return { el, load, onEvent: debounce(ev => { if (ev.type === "issues") load(); }, 150), destroy: () => document.removeEventListener("keydown", onKey) };
}

// ---------- issue drawer ----------
function closeDrawer(silent) {
  if (!S.drawer) return;
  S.drawer.destroy();
  S.drawer = null;
  if (!silent) {
    const m = location.pathname.match(/^\/p\/([^/]+)\/i\/\d+/);
    if (m) history.pushState(null, "", `/p/${m[1]}`);
  }
}

async function openIssue(slug, number, fromRoute) {
  closeDrawer(true);
  if (!fromRoute) history.pushState(null, "", `/p/${slug}/i/${number}`);
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
        h("div", { class: "k", title: "Only machines with all these capabilities can claim the issue" }, "Requires"), listInput("requires", "capabilities, e.g. gpu, cuda, docker"),
        h("div", { class: "k" }, "Depends on"), listInput("depends_on", "issue numbers, e.g. 3, 7"),
        h("div", { class: "k" }, "Branch"), h("div", { class: "mono hint" }, i.branch || `issue-${i.number} (when started)`),
        h("div", { class: "k" }, "Created"), h("div", { class: "hint" }, `${ago(i.created_at)} by ${i.created_by}`)),
      desc,
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
      h("div", { class: "bubble md", html: md(m.body) }));
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

const toolSummary = (name, input) => {
  input = input || {};
  const n = name.replace(/^mcp__multiclaude__/, "⧉ ");
  const v = input.command || input.file_path || input.pattern || input.description || input.url || input.query || input.title || input.message || input.summary || input.verdict || input.path;
  return [n, v ? String(v).split("\n")[0].slice(0, 160) : JSON.stringify(input).slice(0, 120)];
};

function runView(id) {
  const header = h("div", { class: "topbar", style: { paddingBottom: "14px" } });
  const logEl = h("div", { class: "runlog" });
  const ta = h("textarea", { rows: 2, placeholder: "Message this agent… (injected into the live session; on finished runs this resumes the session)" });
  const el = h("div", { style: { display: "flex", flexDirection: "column", height: "100%" } }, header, logEl,
    h("div", { class: "composer" }, ta, h("button", { class: "btn primary", onclick: () => send() }, "Send")));
  let run, lastId = 0, tools = {};
  ta.addEventListener("keydown", e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); send(); } });
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
      h("span", { class: "sub" }, `on ${run.node_name} · ${run.project_slug} · started ${ago(run.started_at)} · ${dur((run.ended_at || Date.now()) - run.started_at)} · ${money(run.cost_usd)}`),
      h("div", { class: "spacer" }),
      run.issue_number ? link(`/p/${run.project_slug}/i/${run.issue_number}`, "Open issue") : link(`/p/${run.project_slug}/chat`, "Project chat"),
      run.status === "running" && h("button", { class: "btn danger small", onclick: () => api("POST", `/api/runs/${id}/cancel`).then(() => toast("Cancelling…")) }, "Cancel run"),
    ]);
  }
  function addEv(e) {
    const d = e.data || {};
    const atBottom = logEl.scrollHeight - logEl.scrollTop - logEl.clientHeight < 120;
    const put = x => x && logEl.appendChild(x);
    if (d.truncated) put(h("div", { class: "ev sys" }, `(large ${e.type} event truncated)`));
    else if (e.type === "system" && d.subtype === "init") put(h("div", { class: "ev sys" }, `Claude session ${String(d.session_id || "").slice(0, 8)} · model ${d.model} · ${(d.tools || []).length} tools · ${d.cwd || ""}`));
    else if (e.type === "assistant") {
      for (const c of d.message?.content || []) {
        if (c.type === "text" && c.text.trim()) put(h("div", { class: "ev text md", html: md(c.text) }));
        else if (c.type === "tool_use") {
          const [n, v] = toolSummary(c.name, c.input);
          const det = h("details", { class: "ev tool" }, h("summary", null, "▸ ", h("span", { class: "tname" }, n), " ", v),
            h("pre", null, JSON.stringify(c.input, null, 2)));
          tools[c.id] = det;
          put(det);
        }
      }
    } else if (e.type === "user") {
      const content = d.message?.content;
      if (Array.isArray(content)) for (const c of content) {
        if (c.type !== "tool_result") continue;
        let text = typeof c.content === "string" ? c.content : (c.content || []).map(x => x.text || "").join("\n");
        if (text.length > 6000) text = text.slice(0, 6000) + "\n…";
        const pre = h("pre", { style: c.is_error ? { borderColor: "var(--bad)" } : null }, text || "(no output)");
        if (tools[c.tool_use_id]) tools[c.tool_use_id].appendChild(pre);
      }
    } else if (e.type === "result") {
      put(h("div", { class: "ev result" + (d.is_error ? " err" : "") },
        h("div", { class: "hint" }, `${d.subtype} · ${d.num_turns} turns · ${dur(d.duration_ms || 0)} · ${money(d.total_cost_usd)}`),
        h("div", { class: "md", html: md(d.result || "") })));
    } else if (e.type === "human") put(h("div", { class: "ev human" }, d.text));
    else if (e.type === "runner") put(h("div", { class: "ev sys" }, "runner: " + d.text));
    else if (e.type === "raw" || e.type === "stderr") put(h("div", { class: "ev sys mono" }, d.text));
    if (atBottom) logEl.scrollTop = logEl.scrollHeight;
  }
  async function loadEvents() {
    for (;;) {
      const evs = await api("GET", `/api/runs/${id}/events?after=${lastId}`);
      evs.forEach(e => { lastId = Math.max(lastId, e.id); addEv(e); });
      if (evs.length < 5000) break;
    }
  }
  return {
    el,
    async load() {
      run = await api("GET", `/api/runs/${id}`);
      renderHeader();
      if (run.prompt) logEl.appendChild(h("details", { class: "ev tool" }, h("summary", null, "▸ ", h("span", { class: "tname" }, "assignment prompt")), h("pre", null, run.prompt)));
      await loadEvents();
      logEl.scrollTop = logEl.scrollHeight;
    },
    onEvent: (ev) => {
      if (ev.type === "run_event" && ev.run_id === id && ev.data.id > lastId) { lastId = ev.data.id; addEv(ev.data); }
      if (ev.type === "runs" && run) api("GET", `/api/runs/${id}`).then(r => { run = r; renderHeader(); });
    },
  };
}

// ---------- code browser ----------
function codeView(info, params) {
  const p = info.project, slug = p.slug;
  const ref = params.get("ref") || p.default_branch;
  const path = params.get("path") || "";
  const mode = params.get("commit") ? "commit" : params.get("file") ? "file" : params.get("view") || "files";
  const el = h("div", { class: "content" });
  const q = o => `/p/${slug}/code?` + new URLSearchParams(Object.fromEntries(Object.entries({ ref, ...o }).filter(([, v]) => v))).toString();

  async function load() {
    const branches = await api("GET", `/api/projects/${slug}/repo/branches`);
    const refSel = h("select", { style: { width: "auto" }, onchange: e => go(q({ ref: e.target.value, path, view: mode === "file" ? "files" : mode })) },
      branches.map(b => h("option", { value: b.name, selected: b.name === ref }, b.name)));
    const nav = h("div", { class: "row", style: { marginBottom: "14px" } }, refSel,
      h("div", { class: "seg" }, [["files", "Files"], ["log", "Commits"], ["branches", `Branches (${branches.length})`]].map(([k, n]) =>
        h("button", { class: (mode === k || (k === "files" && mode === "file") || (k === "log" && mode === "commit")) ? "on" : "", onclick: () => go(q({ view: k })) }, n))));
    el.innerHTML = "";
    el.appendChild(nav);
    if (mode === "files" || mode === "file") {
      const crumbs = h("div", { class: "breadcrumb", style: { marginBottom: "10px" } }, link(q({}), slug));
      let acc = "";
      for (const part of path.split("/").filter(Boolean)) { acc += (acc ? "/" : "") + part; add(crumbs, [" / ", link(q({ path: acc }), part)]); }
      el.appendChild(crumbs);
      if (mode === "file") {
        const b = await api("GET", `/api/projects/${slug}/repo/blob?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(path)}`);
        const raw = `/api/projects/${slug}/repo/blob?raw=1&ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(path)}`;
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
        const entries = await api("GET", `/api/projects/${slug}/repo/tree?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent(path)}`);
        entries.sort((a, b) => (a.type === "tree" ? 0 : 1) - (b.type === "tree" ? 0 : 1) || a.name.localeCompare(b.name));
        el.appendChild(h("table", { class: "list" }, entries.map(e => {
          const full = (path ? path + "/" : "") + e.name;
          return h("tr", { class: "click", onclick: () => go(e.type === "tree" ? q({ path: full }) : q({ path: full, file: "1" })) },
            h("td", { style: { width: "28px" } }, e.type === "tree" ? "📁" : "📄"), h("td", { class: "mono" }, e.name),
            h("td", { class: "hint", style: { textAlign: "right" } }, e.type === "blob" ? (e.size > 1024 ? (e.size / 1024).toFixed(1) + " KB" : e.size + " B") : ""));
        })));
        const readme = entries.find(e => /^readme\.md$/i.test(e.name));
        if (readme) {
          const b = await api("GET", `/api/projects/${slug}/repo/blob?ref=${encodeURIComponent(ref)}&path=${encodeURIComponent((path ? path + "/" : "") + readme.name)}`);
          if (b.text) el.appendChild(h("div", { class: "card pad md", style: { marginTop: "14px" }, html: md(b.text) }));
        }
      }
    } else if (mode === "log") {
      const log = await api("GET", `/api/projects/${slug}/repo/log?ref=${encodeURIComponent(ref)}`);
      el.appendChild(h("table", { class: "list" }, log.map(c => h("tr", { class: "click", onclick: () => go(q({ commit: c.sha })) },
        h("td", { class: "mono hint", style: { width: "90px" } }, c.sha.slice(0, 8)), h("td", null, c.subject, " ", c.refs && h("span", { class: "chip" }, c.refs)),
        h("td", { class: "hint" }, c.author), h("td", { class: "hint", style: { whiteSpace: "nowrap" } }, ago(c.date))))));
    } else if (mode === "commit") {
      const txt = await api("GET", `/api/projects/${slug}/repo/commit?sha=${params.get("commit")}`);
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
      h("label", { class: "toggle" }, dispatch, "Dispatch issues to machines automatically (uncheck to pause the project)"),
      h("div", { class: "row", style: { justifyContent: "flex-end", marginTop: "12px" } },
        h("button", { class: "btn primary", onclick: async () => {
          await api("PATCH", `/api/projects/${p.slug}`, { name: name.value, description: desc.value, instructions: instr.value, review_mode: review.value, dispatch: dispatch.checked });
          toast("Saved"); loadOverview();
        } }, "Save"))),
    h("div", { class: "section-title" }, "Machines assigned to this project"),
    h("div", { class: "card pad" }, S.overview.nodes.length ? nodes : h("div", { class: "hint" }, "No machines registered yet. ", link("/machines", "Install a runner."))),
    h("div", { class: "section-title" }, "Danger zone"),
    h("div", { class: "card pad row" }, h("div", { class: "grow hint" }, "Deleting moves the repository and LFS objects to the server's trash folder."),
      h("button", { class: "btn danger", onclick: () => {
        const inp = h("input", { placeholder: p.slug });
        modal(`Delete ${p.name}?`, h("div", null, h("p", null, "Type the project slug to confirm:"), inp), close => [h("button", { class: "btn", onclick: close }, "Cancel"),
          h("button", { class: "btn danger solid", onclick: async () => { await api("DELETE", `/api/projects/${p.slug}?confirm=${encodeURIComponent(inp.value)}`); close(); await loadOverview(); go("/"); } }, "Delete")]);
      } }, "Delete project")))]);
  return { el };
}

// ---------- machines ----------
function machinesView() {
  const content = h("div", { class: "content" });
  const el = h("div", { style: { display: "contents" } },
    h("div", { class: "topbar", style: { paddingBottom: "14px" } }, h("h1", null, "Machines"), h("span", { class: "sub" }, "Computers running Claude Code that work on your projects")), content);
  const editing = new Set();
  function render() {
    if ([...content.querySelectorAll("input")].some(i => i === document.activeElement)) return;
    const cmd = `curl -fsSL ${location.origin}/install.sh | sh`;
    content.innerHTML = "";
    add(content, [
      h("div", { class: "card pad", style: { marginBottom: "18px" } },
        h("b", null, "Add a machine"),
        h("p", { class: "hint", style: { margin: "4px 0 10px" } }, "Run this on a Linux or macOS computer, as the user that is logged into Claude Code (subscription). It installs the runner as a user service. Then assign the machine to projects below."),
        h("div", { class: "install" }, h("code", null, cmd), h("button", { class: "btn small", onclick: () => copy(cmd) }, "Copy")),
        h("p", { class: "hint", style: { margin: "8px 0 0" } }, "Options: ", h("code", null, `… | sh -s -- --name gpu-box --roles worker --concurrency 4 --caps unity`), ". Roles: ", h("code", null, "worker"), " implements issues, ", h("code", null, "reviewer"), " reviews them.")),
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
    const authOk = /subscription|keychain|env/.test(n.claude_auth || "");
    return h("div", { class: "card pad machine" },
      h("div", { class: "hdr" }, h("span", { class: "dot " + (n.online ? (n.active.length ? "busy" : "on") : "") }), h("h3", null, n.name),
        h("span", { class: "hint" }, n.online ? (n.active.length ? `${n.active.length}/${n.max_conc} busy` : "idle") : `offline · seen ${ago(n.last_seen)}`),
        h("span", { class: "spacer" }),
        n.paused && h("span", { class: "chip warn" }, "paused"),
        h("button", { class: "btn small", onclick: () => save({ paused: !n.paused }) }, n.paused ? "Resume" : "Pause")),
      h("div", { class: "kv" },
        h("div", { class: "k" }, "Claude"), h("div", null, n.claude_version || "?", " ", h("span", { class: "chip " + (authOk ? "ok" : "bad") }, n.claude_auth || "auth unknown")),
        h("div", { class: "k" }, "CPU"), h("div", null, `${hw.cpu_model || "?"} · ${hw.cpu_cores} cores`),
        h("div", { class: "k" }, "Memory"), h("div", null, `${hw.memory_gb} GB · ${hw.disk_free_gb} GB disk free`),
        h("div", { class: "k" }, "GPU"), h("div", null, (hw.gpus || []).length ? hw.gpus.map(g => `${g.name}${g.memory_mb ? ` (${Math.round(g.memory_mb / 1024)} GB)` : ""}`).join(", ") : h("span", { class: "hint" }, "none")),
        h("div", { class: "k" }, "System"), h("div", null, `${hw.os}/${hw.arch} · runner ${n.runner_version}`)),
      h("div", { class: "row wrap", style: { gap: "4px", marginBottom: "12px" } }, n.caps.map(c => h("span", { class: "chip req" }, c)), n.extra_caps.map(c => h("span", { class: "chip accent" }, c))),
      h("div", { class: "props", style: { gridTemplateColumns: "100px 1fr", margin: "0" } },
        h("div", { class: "k" }, "Projects"), S.overview.projects.length ? projects : h("span", { class: "hint" }, "no projects yet"),
        h("div", { class: "k" }, "Roles"), roles,
        h("div", { class: "k" }, "Parallel agents"), conc,
        h("div", { class: "k" }, "Extra tags"), extra,
        h("div", { class: "k" }, "Model"), model),
      n.active.length ? h("div", { style: { marginTop: "12px" } }, n.active.map(r => h("div", { class: "row", style: { fontSize: "13px", padding: "2px 0" } },
        h("span", { class: "dot busy" }), link(`/runs/${r.id}`, r.issue_number ? `${r.kind} #${r.issue_number} ${r.issue_title}` : `chat session`), h("span", { class: "hint" }, r.project_slug + " · " + ago(r.started_at))))) : null,
      h("div", { class: "row", style: { justifyContent: "flex-end", marginTop: "10px" } },
        h("button", { class: "btn small danger ghost", onclick: async () => { if (confirm(`Remove ${n.name}? (it re-appears if its runner is still running)`)) { await api("DELETE", `/api/nodes/${n.id}`); loadOverview().then(render); } } }, "Remove")));
  }
  return { el, load: async () => { await loadOverview(); render(); }, onEvent: debounce(ev => { if (ev.type === "nodes" || ev.type === "projects") loadOverview().then(render); }, 400) };
}

// ---------- boot ----------
(async function boot() {
  await loadOverview().catch(() => {});
  connectEvents();
  route();
  setInterval(() => { if (S.view && S.view.el.isConnected && location.pathname === "/machines") loadOverview(); }, 30000);
})();
