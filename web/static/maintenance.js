// Wartungsfenster unter „Einstellungen“ und Hinweis in der Host-Liste. Nutzt
// el, fetchJSON, toLogin, hostName, lastHosts (app.js) und fmtDateTime
// (alerts.js) erst beim Aufruf. Werte nur per textContent.
"use strict";

const WEEKDAYS = ["Mo", "Di", "Mi", "Do", "Fr", "Sa", "So"]; // Bit 0 = Montag
const SCOPE_LABELS = { all: "alle", group: "Gruppe", host: "Host" };

let maintList = [];
let maintEditing = null; // ID des bearbeiteten Fensters, null = neu

// Hinweis neben dem Hostnamen, solange ein Wartungsfenster gilt.
function hostMaintenancePill(h) {
  if (!h.maintenance) return null;
  const p = el("span", "🛠 Wartung", "pill info host-alerts");
  p.title = "Wartungsfenster „" + h.maintenance + "“: Alarme werden nicht gemeldet";
  return p;
}

function fmtMinutes(min) {
  if (min < 60) return min + " min";
  const h = Math.floor(min / 60), m = min % 60;
  return h + " h" + (m ? " " + m + " min" : "");
}

function maintTime(m) {
  const dur = fmtMinutes(m.duration_min);
  if (m.kind === "once") return fmtDateTime(m.start) + " · " + dur;
  const days = WEEKDAYS.filter((_, i) => m.weekdays & (1 << i)).join(", ");
  const hh = String(Math.floor(m.start_min / 60)).padStart(2, "0"), mm = String(m.start_min % 60).padStart(2, "0");
  return days + " " + hh + ":" + mm + " · " + dur;
}

function maintScope(m) {
  if (m.scope === "all") return "alle";
  if (m.scope === "host") {
    const h = lastHosts.find(x => x.id === m.target);
    return "Host " + (h ? hostName(h) : m.target);
  }
  return "Gruppe " + m.target;
}

async function loadMaintenance() {
  initMaintenanceForm();
  try {
    maintList = await fetchJSON("api/maintenance");
  } catch (e) {
    setMaintStatus(e.message, true);
    return;
  }
  renderMaintenance();
}

function renderMaintenance() {
  const tbody = document.querySelector("#maint-list tbody");
  document.getElementById("maint-table").hidden = maintList.length === 0;
  tbody.replaceChildren(...maintList.map(m => {
    const tr = el("tr");
    let state = "aus", cls = "muted";
    if (m.active) [state, cls] = ["aktiv bis " + fmtDateTime(m.ends_at), "ok-text"];
    else if (m.enabled && m.next_start) state = "nächster Beginn " + fmtDateTime(m.next_start);
    else if (m.enabled) state = "abgelaufen";
    const actions = el("td", null, "actions");
    const edit = el("button", "Bearbeiten", "secondary small-btn");
    edit.type = "button";
    edit.addEventListener("click", () => editMaintenance(m));
    const del = el("button", "Löschen", "danger small-btn");
    del.type = "button";
    del.addEventListener("click", () => deleteMaintenance(m));
    actions.append(edit, del);
    tr.append(el("td", m.name, "name"), el("td", maintScope(m)), el("td", maintTime(m)), el("td", state, cls), actions);
    return tr;
  }));
  fillMaintTargets();
}

// Ziele passend zum Bereich: Gruppen bzw. Hosts aus der Host-Liste.
function fillMaintTargets(keep) {
  const f = document.getElementById("maint-form");
  const scope = f.scope.value;
  const sel = f.target;
  const current = keep ?? sel.value;
  let opts = [];
  if (scope === "group") opts = [...new Set(lastHosts.map(h => h.group).filter(Boolean))].sort().map(g => [g, g]);
  if (scope === "host") opts = lastHosts.map(h => [h.id, hostName(h) + (hostName(h) !== h.id ? " (" + h.id + ")" : "")]);
  sel.replaceChildren(...opts.map(([v, t]) => {
    const o = el("option", t);
    o.value = v;
    return o;
  }));
  if (opts.some(([v]) => v === current)) sel.value = current;
  f.querySelector(".maint-target").hidden = scope === "all";
}

function syncMaintForm() {
  const f = document.getElementById("maint-form");
  const weekly = f.kind.value === "weekly";
  for (const e of f.querySelectorAll(".maint-weekly")) e.hidden = !weekly;
  for (const e of f.querySelectorAll(".maint-once")) e.hidden = weekly;
  f.duration.max = weekly ? 1440 : 10080;
  fillMaintTargets();
}

function toLocalInput(unix) {
  const d = new Date(unix * 1000);
  d.setMinutes(d.getMinutes() - d.getTimezoneOffset());
  return d.toISOString().slice(0, 16);
}

function editMaintenance(m) {
  const f = document.getElementById("maint-form");
  maintEditing = m.id;
  document.getElementById("maint-form-title").textContent = "Wartungsfenster bearbeiten";
  document.getElementById("maint-cancel").hidden = false;
  f.name.value = m.name;
  f.scope.value = m.scope;
  f.kind.value = m.kind;
  f.duration.value = m.duration_min;
  f.enabled.checked = m.enabled;
  if (m.kind === "once") f.start.value = toLocalInput(m.start);
  else {
    f.time.value = String(Math.floor(m.start_min / 60)).padStart(2, "0") + ":" + String(m.start_min % 60).padStart(2, "0");
    for (const cb of f.querySelectorAll("input[name=weekday]")) cb.checked = !!(m.weekdays & (1 << Number(cb.value)));
  }
  syncMaintForm();
  fillMaintTargets(m.target);
  f.scrollIntoView({ block: "nearest" });
}

function resetMaintForm() {
  const f = document.getElementById("maint-form");
  f.reset();
  maintEditing = null;
  document.getElementById("maint-form-title").textContent = "Neues Wartungsfenster";
  document.getElementById("maint-cancel").hidden = true;
  syncMaintForm();
}

function setMaintStatus(text, error) {
  const s = document.getElementById("maint-status");
  s.className = "small" + (error ? " error-text" : "");
  s.textContent = text;
}

async function saveMaintenance(ev) {
  ev.preventDefault();
  const f = ev.target;
  const m = { name: f.name.value, scope: f.scope.value, target: f.scope.value === "all" ? "" : f.target.value,
    kind: f.kind.value, duration_min: Number(f.duration.value), enabled: f.enabled.checked };
  if (m.kind === "once") {
    if (!f.start.value) return setMaintStatus("Beginn angeben", true);
    m.start = Math.floor(new Date(f.start.value).getTime() / 1000);
  } else {
    const [hh, mm] = (f.time.value || "00:00").split(":").map(Number);
    m.start_min = hh * 60 + mm;
    m.weekdays = [...f.querySelectorAll("input[name=weekday]:checked")].reduce((a, cb) => a | (1 << Number(cb.value)), 0);
  }
  setMaintStatus("speichere …");
  try {
    const resp = await fetch("api/maintenance" + (maintEditing ? "/" + maintEditing : ""), {
      method: maintEditing ? "PUT" : "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(m),
    });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    resetMaintForm();
    setMaintStatus("gespeichert");
    await loadMaintenance();
    await refresh();
  } catch (e) {
    setMaintStatus(e.message, true);
  }
}

async function deleteMaintenance(m) {
  if (!confirm("Wartungsfenster „" + m.name + "“ löschen?")) return;
  try {
    const resp = await fetch("api/maintenance/" + m.id, { method: "DELETE" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok && resp.status !== 404) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    if (maintEditing === m.id) resetMaintForm();
    await loadMaintenance();
  } catch (e) {
    setMaintStatus(e.message, true);
  }
}

// Einmalig beim ersten Laden (app.js ist dann geladen, el existiert).
let maintFormReady = false;
function initMaintenanceForm() {
  if (maintFormReady) return;
  maintFormReady = true;
  const f = document.getElementById("maint-form");
  const set = f.querySelector(".weekday-set");
  WEEKDAYS.forEach((d, i) => {
    const l = el("label");
    const cb = el("input");
    cb.type = "checkbox";
    cb.name = "weekday";
    cb.value = String(i);
    cb.defaultChecked = i === 6; // Vorschlag: sonntags (bleibt nach reset)
    l.append(cb, d);
    set.append(l);
  });
  f.scope.addEventListener("change", () => fillMaintTargets());
  f.kind.addEventListener("change", syncMaintForm);
  f.addEventListener("submit", saveMaintenance);
  document.getElementById("maint-cancel").addEventListener("click", resetMaintForm);
  syncMaintForm();
}
