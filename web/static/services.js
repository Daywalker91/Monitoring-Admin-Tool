// Dienste in den Host-Details: Liste mit Filter, Überwachung (Alarm "service")
// und – falls freigeschaltet – Start/Stopp/Neustart als Jobs. Nutzt el, cell,
// fmtAgo, fetchJSON, toLogin, hostName (app.js) und startJob (jobs.js) erst
// beim Aufruf. Werte nur per textContent.
"use strict";

const SVC_RELOAD_MS = 15000;
const SVC_STATE = {
  running: ["läuft", "online"], stopped: ["gestoppt", ""], failed: ["fehlgeschlagen", "crit"],
  starting: ["startet", "warn"], stopping: ["stoppt", "warn"],
};
const SVC_FILTERS = [["problems", "Wichtig"], ["running", "Laufend"], ["all", "Alle"]];
// Startarten, bei denen ein gestoppter Dienst auffällt.
const SVC_AUTOSTART = ["enabled", "auto", "auto (verzögert)"];
const SVC_ACTIONS = [
  ["service.start", "Start", s => s.state !== "running", "starten"],
  ["service.stop", "Stopp", s => s.state === "running", "stoppen"],
  ["service.restart", "Neustart", s => s.state === "running", "neu starten"],
];

const serviceViews = new Map();

// Hinweis in der Host-Liste auf fehlgeschlagene systemd-Units.
function hostServicesPill(h) {
  const n = h.services?.failed || 0;
  if (!n) return null;
  return el("span", n + (n === 1 ? " Unit" : " Units") + " fehlgeschlagen", "pill warn host-alerts");
}

function hostServiceSection(h) {
  let v = serviceViews.get(h.id);
  if (!v) {
    v = createServiceView(h);
    serviceViews.set(h.id, v);
  }
  v.host = h;
  if (!v.loadedAt || Date.now() - v.loadedAt > SVC_RELOAD_MS) loadServices(v);
  else renderServices(v);
  return v.el;
}

function createServiceView(h) {
  const v = { id: h.id, host: h, data: null, loadedAt: 0, filter: "problems", el: el("section", null, "host-services") };
  v.el.append(el("h3", "Dienste"));
  v.summary = el("p", "", "muted small");
  const bar = el("div", null, "svc-bar");
  v.search = el("input");
  v.search.type = "search";
  v.search.placeholder = "Dienst suchen";
  v.search.setAttribute("aria-label", "Dienst suchen");
  v.search.addEventListener("input", () => renderServices(v));
  const seg = el("div", null, "segmented");
  seg.setAttribute("role", "group");
  seg.setAttribute("aria-label", "Dienste filtern");
  v.filterButtons = SVC_FILTERS.map(([key, label]) => {
    const b = el("button", label);
    b.type = "button";
    b.addEventListener("click", () => { v.filter = key; renderServices(v); });
    b.dataset.key = key;
    seg.append(b);
    return b;
  });
  bar.append(v.search, seg);
  v.status = el("p", "", "small");
  v.status.setAttribute("role", "status");
  const table = el("table", null, "mini svc-list");
  const head = el("tr");
  for (const [t, c, title] of [["👁", "num", "Überwachen: Alarm, wenn der Dienst nicht läuft"], ["Dienst", ""],
    ["Status", ""], ["Start", "hide-sm"], ["", "num"]]) {
    const th = el("th", t, c);
    if (title) th.title = title;
    head.append(th);
  }
  const thead = el("thead");
  thead.append(head);
  v.tbody = el("tbody");
  table.append(thead, v.tbody);
  v.table = table;
  v.empty = el("p", "", "muted small");
  v.el.append(v.summary, bar, v.status, table, v.empty);
  v.el.addEventListener("click", ev => ev.stopPropagation());
  return v;
}

async function loadServices(v) {
  v.loadedAt = Date.now();
  try {
    v.data = await fetchJSON("api/hosts/" + encodeURIComponent(v.id) + "/services");
  } catch (e) {
    v.status.className = "small error-text";
    v.status.textContent = "Dienste: " + e.message;
    return;
  }
  renderServices(v);
}

function svcVisible(v, s, watched) {
  const q = v.search.value.trim().toLowerCase();
  if (q) return s.name.toLowerCase().includes(q) || (s.description || "").toLowerCase().includes(q);
  if (v.filter === "all") return true;
  if (v.filter === "running") return s.state === "running";
  return watched || s.state === "failed" || (s.state !== "running" && SVC_AUTOSTART.includes(s.start_type));
}

function renderServices(v) {
  for (const b of v.filterButtons) b.setAttribute("aria-pressed", String(b.dataset.key === v.filter));
  const d = v.data;
  if (!d) return;
  if (!d.updated) {
    v.summary.textContent = v.host.online ? "Noch keine Dienstliste – der Agent meldet sie ab Version 1.2."
      : "Keine Dienstliste.";
    v.table.hidden = true;
    v.empty.hidden = true;
    return;
  }
  const watches = new Set(d.watches);
  const failed = d.list.filter(s => s.state === "failed").length;
  const parts = [d.list.length + " Dienste"];
  if (failed) parts.push(failed + " fehlgeschlagen");
  if (watches.size) parts.push(watches.size + " überwacht");
  parts.push("Stand " + fmtAgo(d.updated));
  v.summary.textContent = parts.join(" · ");

  // Überwachte Dienste, die es nicht (mehr) gibt, trotzdem zeigen, damit man sie abwählen kann.
  const list = [...d.list];
  for (const name of watches) {
    if (!list.some(s => s.name === name)) list.push({ name, state: "missing", detail: "nicht vorhanden" });
  }
  const canControl = v.host.online && (v.host.capabilities || []).includes("services");
  const rows = list.filter(s => svcVisible(v, s, watches.has(s.name))).map(s => serviceRow(v, s, watches.has(s.name), canControl));
  v.tbody.replaceChildren(...rows);
  v.table.hidden = rows.length === 0;
  v.empty.hidden = rows.length > 0;
  v.empty.textContent = v.filter === "problems" && !v.search.value
    ? "Keine Auffälligkeiten. Unter „Alle“ lassen sich Dienste zur Überwachung auswählen."
    : "Keine passenden Dienste.";
}

function serviceRow(v, s, watched, canControl) {
  const tr = el("tr");
  const tdWatch = el("td", null, "num");
  const cb = el("input");
  cb.type = "checkbox";
  cb.checked = watched;
  cb.setAttribute("aria-label", s.name + " überwachen");
  cb.addEventListener("change", () => setWatch(v, s.name, cb));
  tdWatch.append(cb);

  const name = el("td", null, "name");
  name.append(el("span", s.name, "mono"));
  if (s.description) {
    const desc = el("span", s.description, "muted small svc-desc");
    desc.title = s.description;
    name.append(desc);
  }

  const [text, cls] = SVC_STATE[s.state] || ["nicht vorhanden", "crit"];
  const st = el("td", null, "svc-state");
  st.append(el("span", null, "dot " + cls), text);
  st.title = s.detail || "";

  const actions = el("td", null, "num svc-actions");
  if (canControl && s.state !== "missing") {
    for (const [action, label, show, verb] of SVC_ACTIONS) {
      if (!show(s)) continue;
      const b = el("button", label, "secondary small-btn");
      b.type = "button";
      b.addEventListener("click", () => serviceJob(v, s.name, action, verb, b));
      actions.append(b);
    }
  }
  tr.append(tdWatch, name, st, el("td", s.start_type || "–", "hide-sm"), actions);
  return tr;
}

async function setWatch(v, name, cb) {
  cb.disabled = true;
  try {
    const resp = await fetch("api/hosts/" + encodeURIComponent(v.id) + "/services/watch", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, watch: cb.checked }),
    });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    v.status.textContent = "";
    await loadServices(v);
  } catch (e) {
    cb.checked = !cb.checked;
    v.status.className = "small error-text";
    v.status.textContent = e.message;
  } finally {
    cb.disabled = false;
  }
}

async function serviceJob(v, name, action, verb, btn) {
  if (!confirm("Dienst „" + name + "“ auf „" + hostName(v.host) + "“ " + verb + "?")) return;
  btn.disabled = true;
  v.status.className = "small muted";
  v.status.textContent = "„" + name + "“ " + verb + " … (Ergebnis unter „Aktionen“)";
  try {
    await startJob(v.id, action, { name });
    // Der Agent meldet die Dienstliste direkt nach dem Job neu.
    setTimeout(() => loadServices(v), 4000);
  } catch (e) {
    v.status.className = "small error-text";
    v.status.textContent = e.message;
    btn.disabled = false;
  }
}
