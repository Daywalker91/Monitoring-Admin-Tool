// k3s-Ansicht: Nodes, ArgoCD-Apps, Workloads und Pods mit Logs. Nutzt el(),
// cell(), fmtBytes(), fmtAgo(), toLogin() erst beim Aufruf. Werte und Logs
// werden ausschließlich per textContent gesetzt.
"use strict";

let clusterAvailable = true; // false, wenn der Server keinen Cluster kennt (404)
let clusterData = null;
let clusterRendered = ""; // Schlüssel des zuletzt gezeichneten Stands
let clusterIgnore = [];
const clusterFilter = { ns: "", problems: true, search: "" };
const openPods = new Set();
const logViews = new Map(); // "ns/pod" → Log-Ansicht (überlebt das Neuzeichnen)

// Alter kompakt: "45s", "12m", "5h", "23d".
function fmtAge(unix) {
  const s = Math.max(0, Date.now() / 1000 - unix);
  if (s < 60) return Math.floor(s) + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}

// Gleiche Logik wie im Server: Muster "namespace/name" mit * und ?.
function globMatch(pattern, text) {
  const re = "^" + pattern.replace(/[.+^${}()|\\]/g, "\\$&").replace(/\*/g, "[^/]*").replace(/\?/g, "[^/]") + "$";
  try { return new RegExp(re).test(text); } catch { return false; }
}
function isIgnored(ns, name) { return clusterIgnore.some(p => globMatch(p, ns + "/" + name)); }

const POD_PROBLEMS = ["CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull", "CreateContainerConfigError", "InvalidImageName", "OOMKilled"];
function podProblem(p) {
  if (p.phase === "Succeeded") return false;
  return p.phase !== "Running" || POD_PROBLEMS.includes(p.reason) || p.ready < p.total;
}
function workloadProblem(w) { return w.desired > 0 && w.ready < w.desired; }

async function loadCluster() {
  if (!clusterAvailable) return;
  try {
    const resp = await fetch("api/cluster", { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    if (resp.status === 404) {
      clusterAvailable = false;
      document.querySelector('#tabs [data-view="cluster"]').hidden = true;
      return;
    }
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    clusterData = await resp.json();
    renderCluster();
  } catch (e) {
    console.warn("Cluster laden fehlgeschlagen:", e);
  }
}

async function loadClusterIgnore() {
  try {
    const resp = await fetch("api/cluster/ignore", { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) return;
    clusterIgnore = (await resp.json()).patterns;
    document.getElementById("cluster-ignore").value = clusterIgnore.join("\n");
    clusterRendered = "";
    renderCluster();
  } catch (e) {
    console.warn("Ausnahmen laden fehlgeschlagen:", e);
  }
}

function renderCluster(force) {
  const d = clusterData;
  if (!d) return;
  const key = d.fetched_at + "|" + JSON.stringify(clusterFilter) + "|" + clusterIgnore.join(",") + "|" + [...openPods].join(",");
  if (!force && key === clusterRendered) return; // nichts Neues: Markierungen und Scrollstand bleiben
  clusterRendered = key;

  const msgs = [d.error, ...(d.errors || [])].filter(Boolean);
  const banner = document.getElementById("cluster-error");
  banner.hidden = msgs.length === 0;
  banner.textContent = msgs.join(" · ");
  document.getElementById("cluster-fetched").textContent = d.fetched_at ? "Stand " + fmtAgo(d.fetched_at) : "noch kein Stand";

  renderClusterSummary(d);
  renderNodes(d.nodes || []);
  renderApps(d);
  renderNamespaceFilter(d);
  renderWorkloads(d.workloads || []);
  renderPods(d.pods || []);
}

function renderClusterSummary(d) {
  const nodes = d.nodes || [], wl = d.workloads || [], pods = d.pods || [], apps = d.apps || [];
  const pill = (text, bad) => el("span", text, "pill" + (bad ? " crit" : ""));
  const readyNodes = nodes.filter(n => n.ready).length;
  const readyWl = wl.filter(w => !workloadProblem(w)).length;
  const badPods = pods.filter(p => podProblem(p) && !isIgnored(p.namespace, p.name)).length;
  const okApps = apps.filter(a => a.health === "Healthy" && a.sync === "Synced").length;
  const box = document.getElementById("cluster-summary");
  box.replaceChildren(
    pill(readyNodes + "/" + nodes.length + " Nodes bereit", readyNodes < nodes.length),
    pill(readyWl + "/" + wl.length + " Workloads bereit", wl.some(w => workloadProblem(w) && !isIgnored(w.namespace, w.name))),
    pill(pods.length + " Pods" + (badPods ? ", " + badPods + " mit Problemen" : ""), badPods > 0),
  );
  if (d.has_argocd) box.append(pill(okApps + "/" + apps.length + " ArgoCD-Apps in Ordnung", okApps < apps.length));
}

function readyCell(ready, total, label) {
  const td = cell("ready", label, ready + "/" + total, "num");
  if (ready < total) td.classList.add("lvl-crit");
  return td;
}

function pctOrDash(v) {
  if (v == null) return cell("pct", null, "–", "num");
  const td = cell("pct", null, v.toFixed(1) + " %", "num");
  if (v >= 90) td.classList.add("lvl-crit"); else if (v >= 80) td.classList.add("lvl-warn");
  return td;
}

// Läuft auf dem Node ein Agent, kommen CPU, RAM und Temperatur von ihm
// (genauer, mit Temperatur), sonst vom metrics-server.
function renderNodes(nodes) {
  document.querySelector("#k8s-nodes tbody").replaceChildren(...nodes.map(n => {
    const host = lastHosts.find(h => h.k8s_node?.name === n.name);
    const m = host?.online ? host.metrics : null;
    const tr = el("tr");
    const st = cell("status", null, null);
    st.append(el("span", null, "dot" + (n.ready ? " online" : " crit")), n.ready ? "bereit" : "nicht bereit");
    if (!n.ready && n.reason) st.title = n.reason;
    const name = cell("name", null, null, "name");
    name.append(n.name);
    if (host) {
      const link = el("button", host.online ? "Agent" : "Agent offline", "pill host-alerts link-pill" + (host.online ? "" : " crit"));
      link.type = "button";
      link.title = "Host „" + hostName(host) + "“ öffnen";
      link.addEventListener("click", () => openHost(host.id));
      name.append(link);
    }
    const cpu = pctOrDash(m ? m.cpu?.total_pct : n.cpu_pct);
    const mem = pctOrDash(m ? m.mem?.used_pct : n.mem_pct);
    const src = m ? "vom Agenten" : "vom metrics-server";
    cpu.title = mem.title = src;
    const temp = m ? tempCell(m.cpu_temp) : cell("temp", null, "–", "num");
    temp.classList.add("hide-sm");
    tr.append(st, name, cell("roles", null, n.roles.join(", ") || "–", "hide-sm"),
      cell("version", null, n.version, "hide-sm"), cell("ip", null, n.ip || "–", "hide-sm"),
      cpu, mem, temp, cell("pods", null, String(n.pods), "num"),
      cell("age", null, fmtAge(n.created), "num hide-sm"));
    return tr;
  }));
}

function renderApps(d) {
  document.getElementById("k8s-apps-section").hidden = !d.has_argocd;
  if (!d.has_argocd) return;
  const apps = [...d.apps].sort((a, b) => appRank(a) - appRank(b) || a.name.localeCompare(b.name));
  document.querySelector("#k8s-apps tbody").replaceChildren(...apps.map(a => {
    const tr = el("tr");
    const sync = cell("sync", null, a.sync || "–");
    if (a.sync === "OutOfSync") sync.classList.add("lvl-warn");
    const health = cell("health", null, a.health || "–");
    if (a.health === "Degraded" || a.health === "Missing") health.classList.add("lvl-crit");
    else if (a.health !== "Healthy") health.classList.add("lvl-warn");
    const name = cell("name", null, null, "name");
    name.append(a.name);
    if (isIgnored("argocd", a.name)) name.append(el("span", "ausgenommen", "pill host-alerts"));
    tr.append(name, cell("project", null, a.project, "hide-sm"), cell("dest", null, a.dest_namespace || "–", "hide-sm"),
      sync, health, cell("rev", null, a.revision || "–", "mono hide-sm"));
    return tr;
  }));
}

function appRank(a) {
  if (a.health === "Degraded" || a.health === "Missing") return 0;
  if (a.sync === "OutOfSync" || a.health !== "Healthy") return 1;
  return 2;
}

function renderNamespaceFilter(d) {
  const sel = document.getElementById("cluster-ns");
  const names = [...new Set([...(d.workloads || []), ...(d.pods || [])].map(x => x.namespace))].sort();
  const want = ["", ...names].join(",");
  if (sel.dataset.names !== want) {
    sel.dataset.names = want;
    sel.replaceChildren(el("option", "alle Namespaces"), ...names.map(n => el("option", n)));
    sel.options[0].value = "";
    for (const o of [...sel.options].slice(1)) o.value = o.textContent;
  }
  sel.value = clusterFilter.ns;
}

function matchesFilter(x) {
  if (clusterFilter.ns && x.namespace !== clusterFilter.ns) return false;
  const q = clusterFilter.search.trim().toLowerCase();
  return !q || (x.namespace + "/" + x.name).toLowerCase().includes(q);
}

// Im Modus „nur Probleme“ bleiben ausgenommene Ziele ausgeblendet; die Zahl
// steht unter der Tabelle.
function problemFilter(list, isProblem, noteId) {
  const matching = list.filter(x => matchesFilter(x) && (!clusterFilter.problems || isProblem(x)));
  let rows = matching;
  if (clusterFilter.problems) rows = matching.filter(x => !isIgnored(x.namespace, x.name));
  const hidden = matching.length - rows.length;
  const note = document.getElementById(noteId);
  note.hidden = hidden === 0;
  note.textContent = hidden + " ausgenommene ausgeblendet (Ausnahmen unten)";
  return rows;
}

function renderWorkloads(list) {
  const rows = problemFilter(list, workloadProblem, "k8s-workloads-hidden");
  document.getElementById("k8s-workloads-empty").hidden = rows.length > 0;
  document.querySelector("#k8s-workloads tbody").replaceChildren(...rows.map(w => {
    const tr = el("tr");
    const name = cell("name", null, null, "name");
    name.append(w.name);
    if (isIgnored(w.namespace, w.name)) name.append(el("span", "ausgenommen", "pill host-alerts"));
    tr.append(cell("ns", null, w.namespace), cell("kind", null, w.kind, "hide-sm"), name,
      readyCell(w.ready, w.desired, null), cell("age", null, fmtAge(w.created), "num hide-sm"));
    return tr;
  }));
}

function renderPods(list) {
  const rows = [];
  const pods = problemFilter(list, podProblem, "k8s-pods-hidden");
  document.getElementById("k8s-pods-empty").hidden = pods.length > 0;
  for (const p of pods) {
    const id = p.namespace + "/" + p.name;
    const tr = el("tr", null, "pod-row" + (openPods.has(id) ? " open" : ""));
    tr.title = "Logs anzeigen";
    const st = cell("status", null, p.reason || p.phase);
    if (podProblem(p)) st.classList.add(POD_PROBLEMS.includes(p.reason) || p.phase === "Failed" ? "lvl-crit" : "lvl-warn");
    const name = cell("name", null, null, "name");
    name.append(p.name);
    if (podProblem(p) && isIgnored(p.namespace, p.name)) name.append(el("span", "ausgenommen", "pill host-alerts"));
    const restarts = cell("restarts", null, String(p.restarts), "num");
    if (p.restarts >= 5) restarts.classList.add("lvl-warn");
    tr.append(cell("ns", null, p.namespace), name, st, readyCell(p.ready, p.total, null), restarts,
      cell("node", null, p.node || "–", "hide-sm"), cell("age", null, fmtAge(p.created), "num hide-sm"));
    tr.addEventListener("click", () => {
      if (openPods.has(id)) openPods.delete(id); else openPods.add(id);
      renderCluster(true);
    });
    rows.push(tr);
    if (openPods.has(id)) rows.push(logRow(p));
  }
  document.querySelector("#k8s-pods tbody").replaceChildren(...rows);
}

// --- Pod-Logs ------------------------------------------------------------------

function logRow(p) {
  const id = p.namespace + "/" + p.name;
  let v = logViews.get(id);
  if (!v) {
    v = createLogView(p);
    logViews.set(id, v);
    loadLogs(v);
  }
  const tr = el("tr", null, "detail");
  const td = el("td");
  td.colSpan = 7;
  td.append(v.el);
  tr.append(td);
  return tr;
}

function createLogView(p) {
  const v = { ns: p.namespace, pod: p.name };
  const box = el("div", null, "log-view");
  const bar = el("div", null, "log-bar");

  v.container = el("select");
  v.container.setAttribute("aria-label", "Container");
  for (const c of p.containers) v.container.append(el("option", c));
  v.lines = el("select");
  v.lines.setAttribute("aria-label", "Zeilen");
  for (const n of [100, 500, 2000, 5000]) {
    const o = el("option", n + " Zeilen");
    o.value = n;
    v.lines.append(o);
  }
  v.lines.value = "500";
  const prevLabel = el("label", null, "check-label");
  v.previous = el("input");
  v.previous.type = "checkbox";
  prevLabel.append(v.previous, " vorheriger Container (nach Absturz)");
  const reload = el("button", "Neu laden", "secondary");
  reload.type = "button";
  v.status = el("span", null, "small muted");

  for (const ctl of [v.container, v.lines, v.previous]) ctl.addEventListener("change", () => loadLogs(v));
  reload.addEventListener("click", () => loadLogs(v));
  for (const ctl of [v.container, v.lines, prevLabel, reload]) ctl.addEventListener("click", ev => ev.stopPropagation());
  if (p.containers.length > 1) bar.append(v.container);
  bar.append(v.lines, prevLabel, reload, v.status);

  v.pre = el("pre", null, "log-text");
  box.append(bar, v.pre);
  box.addEventListener("click", ev => ev.stopPropagation());
  v.el = box;
  return v;
}

async function loadLogs(v) {
  const q = new URLSearchParams({ ns: v.ns, pod: v.pod, lines: v.lines.value });
  if (v.container.value) q.set("container", v.container.value);
  if (v.previous.checked) q.set("previous", "1");
  v.status.textContent = "lädt …";
  try {
    const resp = await fetch("api/cluster/logs?" + q, { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    const text = await resp.text();
    if (!resp.ok) throw new Error(text.trim() || "HTTP " + resp.status);
    v.pre.textContent = text || "(keine Ausgabe)";
    v.pre.scrollTop = v.pre.scrollHeight;
    v.status.textContent = "geladen " + new Date().toLocaleTimeString("de-DE");
  } catch (e) {
    v.pre.textContent = "";
    v.status.textContent = "Fehler: " + e.message;
  }
}

// --- Ausnahmen -------------------------------------------------------------------

async function saveClusterIgnore(ev) {
  ev.preventDefault();
  const out = document.getElementById("cluster-ignore-status");
  const patterns = document.getElementById("cluster-ignore").value.split("\n").map(s => s.trim()).filter(Boolean);
  out.textContent = "Speichern …";
  try {
    const resp = await fetch("api/cluster/ignore", {
      method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ patterns }),
    });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    clusterIgnore = (await resp.json()).patterns;
    out.textContent = "✓ Gespeichert";
    renderCluster(true);
  } catch (e) {
    out.textContent = "Fehler: " + e.message;
  }
}

document.getElementById("cluster-ns").addEventListener("change", e => { clusterFilter.ns = e.target.value; renderCluster(); });
document.getElementById("cluster-problems").addEventListener("change", e => { clusterFilter.problems = e.target.checked; renderCluster(); });
document.getElementById("cluster-search").addEventListener("input", e => { clusterFilter.search = e.target.value; renderCluster(); });
document.getElementById("cluster-ignore-form").addEventListener("submit", saveClusterIgnore);
