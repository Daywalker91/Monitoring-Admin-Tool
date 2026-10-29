// Host-Liste: lädt /api/hosts regelmäßig und rendert die Tabelle (Desktop) bzw.
// Karten (Handy, per CSS aus denselben Zeilen).
// Sicherheit: Werte werden ausschließlich per textContent gesetzt, nie per innerHTML.
"use strict";

const REFRESH_MS = 5000;
const VIEWS = ["hosts", "checks", "cluster", "alerts", "rules"];
// Ansicht und aufgeklappte Hosts stehen im URL-Hash (#view=alerts&open=a,b),
// damit sie einen Reload überstehen.
const hashParams = new URLSearchParams(location.hash.slice(1));
const openHosts = new Set(hashParams.get("open")?.split(",").filter(Boolean) ?? []);
let currentView = VIEWS.includes(hashParams.get("view")) ? hashParams.get("view") : "hosts";
let lastHosts = [];
// Zugeklappte Gruppen der Host-Liste; reine Bequemlichkeit, daher nur im Browser.
const GROUPS_KEY = "mat.collapsedGroups";
const collapsedGroups = new Set(loadCollapsed());

function loadCollapsed() {
  try { return JSON.parse(localStorage.getItem(GROUPS_KEY)) || []; } catch { return []; }
}

function saveCollapsed() {
  try { localStorage.setItem(GROUPS_KEY, JSON.stringify([...collapsedGroups])); } catch { /* egal */ }
}

// Anzeigename eines Hosts, ersatzweise der gemeldete Hostname.
function hostName(h) { return h.display_name || h.hostname; }

function saveOpenHosts() {
  const parts = [];
  if (currentView !== "hosts") parts.push("view=" + currentView);
  if (openHosts.size) parts.push("open=" + [...openHosts].map(encodeURIComponent).join(","));
  history.replaceState(null, "", parts.length ? "#" + parts.join("&") : location.pathname + location.search);
}

function showView(view) {
  currentView = view;
  for (const v of VIEWS) document.getElementById("view-" + v).hidden = v !== view;
  for (const b of document.querySelectorAll("#tabs button")) {
    b.setAttribute("aria-pressed", String(b.dataset.view === view));
  }
  saveOpenHosts();
  if (view === "rules") loadRules(); // nur beim Öffnen laden, damit Eingaben erhalten bleiben
  if (view === "cluster") loadClusterIgnore();
  refresh();
}

function el(tag, text, cls) {
  const e = document.createElement(tag);
  if (text !== undefined && text !== null) e.textContent = text;
  if (cls) e.className = cls;
  return e;
}

// Tabellenzelle mit Spaltenklasse und Label (das Label erscheint nur in der Kartenansicht).
function cell(col, label, text, extraCls) {
  const td = el("td", text, ["c-" + col, extraCls].filter(Boolean).join(" "));
  if (label) td.dataset.label = label;
  return td;
}

function fmtBytes(n) {
  if (!n) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return n.toFixed(n < 10 && i > 0 ? 1 : 0) + " " + units[i];
}

function fmtRate(n) { return fmtBytes(n) + "/s"; }
function fmtPct(n) { return (n ?? 0).toFixed(1) + " %"; }

function fmtAgo(unix) {
  const s = Math.max(0, Math.round(Date.now() / 1000 - unix));
  if (s < 60) return "vor " + s + " s";
  if (s < 3600) return "vor " + Math.round(s / 60) + " min";
  if (s < 86400) return "vor " + Math.round(s / 3600) + " h";
  return "vor " + Math.round(s / 86400) + " d";
}

function fmtUptime(bootUnix) {
  if (!bootUnix) return "–";
  const s = Date.now() / 1000 - bootUnix;
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600);
  return d + " d " + h + " h";
}

// Windows kennt keinen Load-Wert; gopsutil liefert dort nur Nullen.
function hasLoad(h) { return h.os !== "windows"; }

// Prozentzelle mit Warnstufe ab 80 % bzw. 90 %.
function pctCell(col, label, v) {
  const td = cell(col, label, v === undefined ? "–" : fmtPct(v), "num");
  if (v >= 90) td.classList.add("lvl-crit");
  else if (v >= 80) td.classList.add("lvl-warn");
  return td;
}

function fmtTemp(v) { return v == null ? "–" : v.toFixed(1) + " °C"; }

// CPU-Temperatur; ohne Sensor (Windows, VM) "–".
function tempCell(v) {
  const td = cell("temp", "Temp", fmtTemp(v), "num");
  if (v >= 80) td.classList.add("lvl-crit");
  else if (v >= 70) td.classList.add("lvl-warn");
  return td;
}

// Markierung für Hosts, die k3s-Nodes sind; „nicht bereit“ kommt aus der k3s-API.
function k8sPill(n) {
  const p = el("span", n.ready ? "k3s" : "k3s nicht bereit", "pill host-alerts" + (n.ready ? "" : " crit"));
  p.title = "k3s-Node " + n.name + (n.roles?.length ? " (" + n.roles.join(", ") + ")" : "") +
    (!n.ready && n.reason ? ": " + n.reason : "");
  return p;
}

// Öffnet einen Host in der Host-Liste (z. B. aus dem Tab „Cluster“).
function openHost(id) {
  openHosts.add(id);
  showView("hosts");
  requestAnimationFrame(() => document.querySelector(`#hosts tr.host[data-id="${CSS.escape(id)}"]`)?.scrollIntoView({ block: "start" }));
}

function hostRow(h) {
  const m = h.metrics || {};
  const tr = el("tr", null, "host");
  tr.dataset.id = h.id;
  if (openHosts.has(h.id)) tr.classList.add("open");

  const status = cell("status", null, null);
  status.append(el("span", null, "dot" + (h.online ? " online" : "")), h.online ? "online" : "offline");
  tr.append(status);

  const host = cell("host", null, null);
  host.append(el("span", hostName(h), "hostname"));
  // Bei eigenem Namen den echten Hostnamen zeigen, sonst die Agent-ID, wenn sie
  // vom Hostnamen abweicht (sonst sehen mehrere Einträge gleich aus).
  if (h.display_name) host.append(el("span", h.hostname, "agent-id"));
  else if (h.id !== h.hostname.toLowerCase()) host.append(el("span", h.id, "agent-id"));
  const nAlerts = alertCountFor(h.id);
  if (nAlerts) host.append(el("span", alertBadgeText(nAlerts), "pill crit host-alerts"));
  if (h.muted_until) host.append(el("span", "🔕 stumm", "pill host-alerts"));
  const svcPill = hostServicesPill(h);
  if (svcPill) host.append(svcPill);
  host.append(...hostPackagePills(h));
  if (h.k8s_node) host.append(k8sPill(h.k8s_node));
  const upd = hostUpdatePill(h);
  if (upd) host.append(upd);
  tr.append(host);

  tr.append(cell("system", null, [h.platform || h.os, h.platform_version, h.arch].filter(Boolean).join(" · ")));

  if (!h.metrics) {
    for (const [col, label] of [["cpu", "CPU"], ["load", "Load"], ["mem", "RAM"], ["swap", "Swap"], ["net", "Netz ↓ / ↑"], ["disk", "Disk max"], ["temp", "Temp"]]) {
      tr.append(cell(col, label, "–", "num"));
    }
  } else {
    tr.append(pctCell("cpu", "CPU", m.cpu?.total_pct));
    tr.append(cell("load", "Load", m.cpu && hasLoad(h) ? m.cpu.load1.toFixed(2) : "–", "num"));
    tr.append(pctCell("mem", "RAM", m.mem?.used_pct));
    tr.append(m.swap?.total ? pctCell("swap", "Swap", m.swap.used_pct) : cell("swap", "Swap", "–", "num"));
    let rx = 0, tx = 0;
    for (const n of m.net || []) { rx += n.rx_bps; tx += n.tx_bps; }
    tr.append(cell("net", "Netz ↓ / ↑", fmtRate(rx) + " / " + fmtRate(tx), "num"));
    const diskMax = Math.max(0, ...(m.disks || []).map(d => d.used_pct));
    tr.append(pctCell("disk", "Disk max", diskMax));
    tr.append(tempCell(m.cpu_temp));
  }
  tr.append(cell("seen", "Zuletzt", fmtAgo(h.last_seen)));

  tr.addEventListener("click", () => {
    if (openHosts.has(h.id)) openHosts.delete(h.id); else openHosts.add(h.id);
    saveOpenHosts();
    render(lastHosts);
  });
  return tr;
}

// --- Verlauf --------------------------------------------------------------
// Je Host eine dauerhafte Ansicht: Das Element überlebt das Neuzeichnen der
// Liste alle 5 s (kein Flackern, Hover bleibt erhalten) und lädt nur alle 30 s nach.

const HISTORY_POINTS = 300;
const HISTORY_REFRESH_MS = 30000;
const RANGES = [["1 h", 1], ["24 h", 24], ["7 d", 168]];
const historyViews = new Map();

// Achse: "0", "100 KB"; Tooltip: "100 KB/s".
function fmtRateShort(n, axis) {
  if (axis) return n ? fmtBytes(n) : "0";
  return fmtRate(n);
}

function createHistoryView(h) {
  const v = { id: h.id, hours: 1, data: null, fetchedAt: 0, loading: false, interval: h.interval_sec };
  const sec = el("section", null, "history");

  const head = el("div", null, "history-head");
  head.append(el("h3", "Verlauf"));
  const ranges = el("div", null, "segmented");
  ranges.setAttribute("role", "group");
  ranges.setAttribute("aria-label", "Zeitraum");
  for (const [label, hours] of RANGES) {
    const b = el("button", label);
    b.type = "button";
    b.setAttribute("aria-pressed", String(hours === v.hours));
    b.addEventListener("click", ev => {
      ev.stopPropagation();
      if (v.hours === hours) return;
      v.hours = hours;
      ranges.querySelectorAll("button").forEach(x => x.setAttribute("aria-pressed", String(x === b)));
      v.fetchedAt = 0;
      loadHistory(v);
    });
    ranges.append(b);
  }
  head.append(ranges);
  sec.append(head);

  const pct = (n, axis) => axis ? n.toFixed(0) + "%" : (n ?? 0).toFixed(1) + " %";
  v.charts = [
    Charts.lineChart({ title: "CPU", scale: "pct", area: true, format: pct,
      series: [{ key: "cpu_pct", name: "CPU", cls: "s1" }] }),
    Charts.lineChart({ title: "RAM", scale: "pct", area: true, format: pct,
      series: [{ key: "mem_pct", name: "RAM", cls: "s1" }] }),
    Charts.lineChart({ title: "Netz", scale: "bytes", format: fmtRateShort,
      series: [{ key: "net_rx_bps", name: "↓ empfangen", cls: "s1" }, { key: "net_tx_bps", name: "↑ gesendet", cls: "s2" }] }),
  ];
  v.hasTemp = h.metrics?.cpu_temp != null;
  if (v.hasTemp) {
    v.charts.push(Charts.lineChart({ title: "CPU-Temperatur", format: (n, axis) => axis ? n.toFixed(0) + "°" : fmtTemp(n),
      series: [{ key: "cpu_temp", name: "Temperatur", cls: "s1" }] }));
  }
  const grid = el("div", null, "chart-grid");
  for (const c of v.charts) grid.append(c.el);
  sec.append(grid);

  // Tabellenansicht: alle Werte ohne Hover erreichbar. Wird erst beim Aufklappen gefüllt.
  const details = el("details", null, "history-table");
  details.append(el("summary", "Werte als Tabelle"));
  const table = el("table", null, "mini");
  const thead = el("thead");
  const hr = el("tr");
  const cols = [["Zeit"], ["CPU", "num"], ["RAM", "num"], ["↓", "num"], ["↑", "num"]];
  if (v.hasTemp) cols.push(["Temp", "num"]);
  for (const [t, c] of cols) hr.append(el("th", t, c));
  thead.append(hr);
  table.append(thead, el("tbody"));
  details.append(table);
  details.addEventListener("toggle", () => fillHistoryTable(v));
  sec.append(details);
  v.details = details;
  v.table = table;

  v.el = sec;
  return v;
}

function fillHistoryTable(v) {
  if (!v.details.open || !v.data) return;
  const opts = { hour: "2-digit", minute: "2-digit" };
  if (v.hours > 24) Object.assign(opts, { day: "numeric", month: "numeric" });
  const rows = [...v.data].reverse().map(p => [
    [new Date(p.ts * 1000).toLocaleString("de-DE", opts)],
    [fmtPct(p.cpu_pct), "num"], [fmtPct(p.mem_pct), "num"],
    [fmtRate(p.net_rx_bps), "num"], [fmtRate(p.net_tx_bps), "num"],
    ...(v.hasTemp ? [[fmtTemp(p.cpu_temp), "num"]] : []),
  ]);
  const tbody = v.table.querySelector("tbody");
  tbody.replaceChildren();
  fillTable(tbody, rows);
}

async function loadHistory(v) {
  if (v.loading || Date.now() - v.fetchedAt < HISTORY_REFRESH_MS) return;
  v.loading = true;
  v.el.classList.add("loading"); // altes Bild bleibt gedimmt stehen, bis neue Daten da sind
  const hours = v.hours;
  try {
    const resp = await fetch(`api/hosts/${encodeURIComponent(v.id)}/history?hours=${hours}&points=${HISTORY_POINTS}`,
      { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    const data = await resp.json();
    if (hours !== v.hours) return; // Zeitraum wurde inzwischen gewechselt
    v.data = data;
    v.fetchedAt = Date.now();
    drawHistory(v);
  } catch (e) {
    console.warn("Verlauf laden fehlgeschlagen:", e);
  } finally {
    v.loading = false;
    v.el.classList.remove("loading");
    if (hours !== v.hours) loadHistory(v);
  }
}

function drawHistory(v) {
  const end = Date.now() / 1000;
  const start = end - v.hours * 3600;
  const bucket = (v.hours * 3600) / HISTORY_POINTS;
  // Lücke, wenn mehr als ~3 Messintervalle bzw. 2,5 Fenster fehlen.
  const gap = Math.max(bucket * 2.5, (v.interval || 10) * 3);
  for (const c of v.charts) c.update(v.data, start, end, gap);
  fillHistoryTable(v);
}

function historySection(h) {
  let v = historyViews.get(h.id);
  if (!v) {
    v = createHistoryView(h);
    historyViews.set(h.id, v);
  }
  v.interval = h.interval_sec;
  loadHistory(v);
  return v.el;
}

function fillTable(tbody, rows) {
  for (const cells of rows) {
    const tr = el("tr");
    for (const [text, cls] of cells) tr.append(el("td", text, cls));
    tbody.append(tr);
  }
}

function procRows(list) {
  return (list || []).map(p => [
    [String(p.pid), "hide-sm"], [p.name, "name"], [p.cpu_pct.toFixed(1) + " %", "num"], [fmtBytes(p.rss), "num"],
  ]);
}

function detailRow(h) {
  const tpl = document.getElementById("detail-tpl").content.cloneNode(true);
  const tr = tpl.querySelector("tr");
  const m = h.metrics || {};
  const p = h.processes || {};

  tr.querySelector(".history-slot").replaceWith(historySection(h));
  tr.querySelector(".host-alerts-slot").replaceWith(hostAlertSection(h));
  tr.querySelector(".host-meta-slot").replaceWith(hostMetaSection(h));
  tr.querySelector(".host-update-slot").replaceWith(hostUpdateSection(h));
  tr.querySelector(".host-jobs-slot").replaceWith(hostJobSection(h));
  tr.querySelector(".host-services-slot").replaceWith(hostServiceSection(h));
  tr.querySelector(".host-packages-slot").replaceWith(hostPackageSection(h));

  const cores = tr.querySelector(".cores");
  (m.cpu?.per_core || []).forEach((v, i) => {
    const c = el("div", "#" + i + " " + v.toFixed(0) + " %", "core");
    const bar = el("div", null, "bar");
    const fill = el("span");
    fill.style.width = Math.min(100, v) + "%";
    bar.append(fill);
    c.append(bar);
    cores.append(c);
  });

  fillTable(tr.querySelector(".disks tbody"), (m.disks || []).map(d => [
    [d.mount, "name"], [d.fstype, "hide-sm"], [fmtPct(d.used_pct), "num"], [fmtBytes(d.total), "num"],
  ]));
  fillTable(tr.querySelector(".ifaces tbody"), (m.net || []).map(n => [
    [n.name, "name"], [fmtRate(n.rx_bps), "num"], [fmtRate(n.tx_bps), "num"],
    [n.rx_pps.toFixed(0) + " / " + n.tx_pps.toFixed(0), "num hide-sm"],
  ]));
  fillTable(tr.querySelector(".procs-cpu tbody"), procRows(p.top_cpu));
  fillTable(tr.querySelector(".procs-mem tbody"), procRows(p.top_mem));

  const info = tr.querySelector(".info");
  const add = (k, v) => { info.append(el("dt", k), el("dd", v || "–")); };
  add("Agent-ID", h.id);
  add("Version", h.agent_version);
  add("Kernel", h.kernel_version);
  add("Kerne", String(h.cpu_cores));
  add("RAM", m.mem ? fmtBytes(m.mem.used) + " / " + fmtBytes(m.mem.total) : "");
  add("Uptime", fmtUptime(h.boot_time));
  if (m.temps?.length) add("Sensoren", m.temps.map(t => t.name + " " + fmtTemp(t.celsius)).join(", "));
  if (hasLoad(h)) {
    add("Load 1/5/15", m.cpu ? [m.cpu.load1, m.cpu.load5, m.cpu.load15].map(x => x.toFixed(2)).join(" / ") : "");
  }
  if (h.k8s_node) {
    const n = h.k8s_node;
    add("k3s-Node", n.name + (n.roles?.length ? " · " + n.roles.join(", ") : "") + " · " + (n.ready ? "bereit" : "nicht bereit"));
  }
  add("Fähigkeiten", (h.capabilities || []).join(", "));
  add("Adresse", h.remote_addr);
  add("Intervall", h.interval_sec + " s");

  // Nur Offline-Hosts lassen sich entfernen; ein verbundener Agent würde sich sofort neu anlegen.
  const del = tr.querySelector(".delete");
  del.hidden = h.online;
  del.addEventListener("click", ev => {
    ev.stopPropagation();
    deleteHost(h);
  });
  return tr;
}

async function deleteHost(h) {
  if (!confirm("Host „" + hostName(h) + "“ (" + h.id + ") samt Historie aus der Liste entfernen?")) return;
  try {
    const resp = await fetch("api/hosts/" + encodeURIComponent(h.id), { method: "DELETE" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok && resp.status !== 404) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    openHosts.delete(h.id);
    historyViews.delete(h.id);
    hostAlertViews.delete(h.id);
    metaViews.delete(h.id);
    jobViews.delete(h.id);
    serviceViews.delete(h.id);
    pkgViews.delete(h.id);
    saveOpenHosts();
    await refresh();
  } catch (e) {
    alert("Löschen fehlgeschlagen: " + e.message);
  }
}

// --- Name und Gruppe --------------------------------------------------------
// Wie beim Verlauf überlebt das Formular das Neuzeichnen, damit Eingaben bleiben.

const metaViews = new Map();

function hostMetaSection(h) {
  let v = metaViews.get(h.id);
  if (!v) {
    v = createMetaView(h);
    metaViews.set(h.id, v);
  }
  // Nur übernehmen, solange nichts ungespeichert eingetippt ist.
  if (!v.dirty) {
    v.name.value = h.display_name || "";
    v.group.value = h.group || "";
  }
  v.name.placeholder = h.hostname;
  return v.el;
}

function createMetaView(h) {
  const v = { id: h.id, dirty: false };
  const form = el("form", null, "host-meta");
  form.autocomplete = "off";
  form.append(el("h3", "Name und Gruppe"));
  const grid = el("div", null, "form-grid");
  const field = (label, input) => {
    const l = el("label", label);
    l.append(input);
    grid.append(l);
    return input;
  };
  v.name = field("Anzeigename", el("input"));
  v.group = field("Gruppe", el("input"));
  v.group.setAttribute("list", "host-groups");
  v.group.placeholder = "ohne Gruppe";
  for (const i of [v.name, v.group]) {
    i.maxLength = 64;
    i.addEventListener("input", () => { v.dirty = true; });
  }
  form.append(grid);
  const actions = el("div", null, "form-actions");
  const save = el("button", "Speichern", "secondary");
  save.type = "submit";
  v.status = el("span", "", "small");
  v.status.setAttribute("role", "status");
  actions.append(save, v.status);
  form.append(actions, el("p", "Leer lassen = Hostname bzw. ohne Gruppe. Der Name erscheint auch in Alarmen und Benachrichtigungen.", "muted small"));
  form.addEventListener("click", ev => ev.stopPropagation());
  form.addEventListener("submit", ev => { ev.preventDefault(); saveMeta(v, save); });
  v.el = el("section");
  v.el.append(form);
  return v;
}

async function saveMeta(v, btn) {
  btn.disabled = true;
  v.status.className = "small";
  v.status.textContent = "speichere …";
  try {
    const resp = await fetch("api/hosts/" + encodeURIComponent(v.id) + "/meta", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ display_name: v.name.value, group: v.group.value }),
    });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    v.dirty = false;
    v.status.textContent = "gespeichert";
    await refresh();
  } catch (e) {
    v.status.className = "small error-text";
    v.status.textContent = e.message;
  } finally {
    btn.disabled = false;
  }
}

// Gruppen alphabetisch, Hosts ohne Gruppe am Ende.
function groupHosts(hosts) {
  const groups = new Map();
  for (const h of hosts) {
    const g = h.group || "";
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(h);
  }
  return [...groups.entries()].sort(([a], [b]) =>
    (a === "") - (b === "") || a.localeCompare(b, "de", { sensitivity: "base" }));
}

function groupRow(name, hosts) {
  const tr = el("tr", null, "group-row");
  const td = el("td");
  td.colSpan = 11;
  const collapsed = collapsedGroups.has(name);
  const btn = el("button", (collapsed ? "▸ " : "▾ ") + (name || "Ohne Gruppe"), "group-toggle");
  btn.type = "button";
  btn.setAttribute("aria-expanded", String(!collapsed));
  const online = hosts.filter(h => h.online).length;
  const nAlerts = hosts.reduce((n, h) => n + alertCountFor(h.id), 0);
  td.append(btn, el("span", hosts.length + (hosts.length === 1 ? " Host, " : " Hosts, ") + online + " online", "muted small"));
  if (nAlerts) td.append(el("span", alertBadgeText(nAlerts), "pill crit"));
  btn.addEventListener("click", () => {
    if (collapsed) collapsedGroups.delete(name); else collapsedGroups.add(name);
    saveCollapsed();
    render(lastHosts);
  });
  tr.append(td);
  return tr;
}

function render(hosts) {
  lastHosts = hosts;
  const tbody = document.querySelector("#hosts tbody");
  const rows = [];
  const groups = groupHosts(hosts);
  // Überschriften nur, wenn überhaupt Gruppen vergeben sind.
  const withHeads = groups.some(([g]) => g !== "");
  for (const [name, list] of groups) {
    if (withHeads) {
      rows.push(groupRow(name, list));
      if (collapsedGroups.has(name)) continue;
    }
    for (const h of list) {
      rows.push(hostRow(h));
      if (openHosts.has(h.id)) rows.push(detailRow(h));
    }
  }
  tbody.replaceChildren(...rows);

  const dl = document.getElementById("host-groups");
  dl.replaceChildren(...groups.filter(([g]) => g !== "").map(([g]) => {
    const o = el("option");
    o.value = g;
    return o;
  }));

  const online = hosts.filter(h => h.online).length;
  document.getElementById("summary").textContent = hosts.length + " Hosts, " + online + " online";
  document.getElementById("empty").hidden = hosts.length > 0;
}

// Session abgelaufen oder abgemeldet: zur Login-Seite.
function toLogin() { location.replace("login.html"); }

async function fetchJSON(url) {
  const resp = await fetch(url, { cache: "no-store" });
  if (resp.status === 401) { toLogin(); throw new Error("nicht angemeldet"); }
  if (!resp.ok) throw new Error("HTTP " + resp.status);
  return resp.json();
}

async function refresh() {
  const errBox = document.getElementById("error");
  try {
    // In der Alarmansicht auch beendete Alarme laden, sonst nur aktive.
    const withPast = currentView === "alerts";
    const [hosts, alerts] = await Promise.all([fetchJSON("api/hosts"), fetchJSON("api/alerts" + (withPast ? "?all=1" : "")),
      loadUpdateStatus()]);
    activeAlerts = alerts.filter(a => a.resolved_at === null);
    renderAlertBadge();
    if (withPast) renderAlerts(alerts);
    render(hosts);
    renderUpdateBanner(hosts);
    if (currentView === "rules") {
      renderUpdatePanel(hosts);
      renderInstallPanel();
    }
    if (currentView === "checks") await loadChecks();
    if (currentView === "cluster") await loadCluster();
    errBox.hidden = true;
    document.getElementById("updated").textContent = "aktualisiert " + new Date().toLocaleTimeString("de-DE");
  } catch (e) {
    errBox.textContent = "Server nicht erreichbar: " + e.message;
    errBox.hidden = false;
  }
}

async function initUser() {
  try {
    const resp = await fetch("api/me", { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    if (resp.ok) document.getElementById("username").textContent = (await resp.json()).username;
  } catch { /* Anzeige des Namens ist nicht kritisch */ }
}

document.getElementById("logout").addEventListener("click", async () => {
  try { await fetch("api/logout", { method: "POST" }); } finally { toLogin(); }
});

for (const b of document.querySelectorAll("#tabs button")) {
  b.addEventListener("click", () => showView(b.dataset.view));
}
document.getElementById("alert-badge").addEventListener("click", () => showView("alerts"));

initUser();
showView(currentView);
setInterval(refresh, REFRESH_MS);
