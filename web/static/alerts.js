// Alarme: Markierung in der Kopfzeile, Alarmansicht und Regel-Einstellungen.
// Wird vor app.js geladen und nutzt dessen Hilfsfunktionen (el, cell, fmtPct, toLogin)
// erst beim Aufruf. Werte werden ausschließlich per textContent gesetzt.
"use strict";

const ALERT_LABELS = { offline: "Offline", cpu: "CPU", mem: "RAM", swap: "Swap", disk: "Disk", temp: "CPU-Temperatur",
  check_down: "Check", check_cert: "Zertifikat" };
// Regeltypen ohne Schwelle (nur Mindestdauer).
const NO_THRESHOLD = ["offline", "check_down"];
const RULE_HELP = {
  offline: "Agent nicht verbunden",
  cpu: "CPU-Auslastung gesamt",
  mem: "Arbeitsspeicher belegt",
  swap: "Swap belegt (nur Hosts mit Swap)",
  disk: "Belegung je Mountpoint",
  temp: "CPU-Temperatur in °C (nur Linux-Hosts mit Sensor)",
  check_down: "Externer Check schlägt fehl",
  check_cert: "Zertifikat eines HTTPS-Checks läuft in weniger als … Tagen ab",
};

// Schwelle je Regeltyp: Einheit und erlaubter Bereich (wie im Server geprüft).
function ruleUnit(kind) {
  if (kind === "temp") return { unit: " °C", min: 30, max: 120, aria: "in Grad Celsius" };
  if (kind === "check_cert") return { unit: " Tage", min: 1, max: 365, aria: "in Tagen" };
  return { unit: " %", min: 10, max: 100, aria: "in Prozent" };
}

// Messwert eines Alarms in der Einheit seines Regeltyps.
function fmtAlertNum(kind, v) {
  if (kind === "temp") return fmtTemp(v);
  if (kind === "check_cert") return Math.floor(v) + " Tage";
  return fmtPct(v);
}
const CHANNEL_LABELS = { ntfy: "ntfy", email: "E-Mail" };

let activeAlerts = []; // aktive Alarme, für Markierung und Host-Liste

function alertSubject(a) { return (ALERT_LABELS[a.kind] || a.kind) + (a.target ? " " + a.target : ""); }

function fmtDuration(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return sec + " s";
  if (sec < 3600) return Math.floor(sec / 60) + " min";
  if (sec < 86400) return Math.floor(sec / 3600) + " h " + Math.floor((sec % 3600) / 60) + " min";
  return Math.floor(sec / 86400) + " d " + Math.floor((sec % 86400) / 3600) + " h";
}

function fmtDateTime(unix) {
  const d = new Date(unix * 1000);
  // Jahr nur, wenn es nicht das laufende ist (z. B. Ablauf des Zertifikats)
  const year = d.getFullYear() !== new Date().getFullYear() ? { year: "numeric" } : {};
  return d.toLocaleString("de-DE", { day: "2-digit", month: "2-digit", ...year, hour: "2-digit", minute: "2-digit" });
}

// Wert eines Alarms: Prozent, bei "offline" die Zeit ohne Verbindung (in Minuten gespeichert).
function alertValue(a, v) {
  if (a.kind === "offline") return fmtDuration(v * 60);
  if (a.kind === "check_down") return a.detail || "fehlgeschlagen";
  return fmtAlertNum(a.kind, v);
}

function alertCountFor(hostId) { return activeAlerts.filter(a => a.host_id === hostId).length; }

function alertBadgeText(n) { return "⚠ " + n + (n === 1 ? " Alarm" : " Alarme"); }

// Markierung oben und Zähler im Reiter.
function renderAlertBadge() {
  const n = activeAlerts.length;
  const badge = document.getElementById("alert-badge");
  badge.hidden = n === 0;
  badge.textContent = alertBadgeText(n) + " aktiv";
  document.querySelector('#tabs [data-view="alerts"] .count').textContent = n ? " (" + n + ")" : "";
}

// --- Alarmansicht -----------------------------------------------------------

function alertRow(a) {
  const tr = el("tr", null, "alert-row");
  const active = a.resolved_at === null;
  const state = cell("state", null, null);
  state.append(el("span", active ? "⚠ aktiv" : "✓ beendet", active ? "pill crit" : "pill"));
  tr.append(state);

  const host = cell("host", "Host", null);
  host.append(el("span", a.hostname, "hostname"));
  tr.append(host);
  tr.append(cell("rule", "Regel", alertSubject(a)));

  let value = alertValue(a, a.value);
  if (!NO_THRESHOLD.includes(a.kind) && a.kind !== "check_cert" && alertValue(a, a.peak) !== value) {
    value += " (Spitze " + alertValue(a, a.peak) + ")";
  }
  tr.append(cell("value", "Wert", value, "num"));
  tr.append(cell("threshold", "Schwelle", NO_THRESHOLD.includes(a.kind) ? "–" : fmtAlertNum(a.kind, a.threshold), "num hide-sm"));
  tr.append(cell("start", "Beginn", fmtDateTime(a.started_at)));
  const end = active ? Date.now() / 1000 : a.resolved_at;
  tr.append(cell("dur", "Dauer", fmtDuration(end - a.started_at), "num"));

  tr.title = "Host anzeigen";
  tr.addEventListener("click", () => {
    if (a.host_id.startsWith("check:")) return showView("checks");
    openHosts.add(a.host_id);
    showView("hosts");
  });
  return tr;
}

function renderAlerts(list) {
  const active = list.filter(a => a.resolved_at === null);
  const past = list.filter(a => a.resolved_at !== null);
  document.querySelector("#alerts-active tbody").replaceChildren(...active.map(alertRow));
  document.querySelector("#alerts-past tbody").replaceChildren(...past.map(alertRow));
  document.getElementById("alerts-active-empty").hidden = active.length > 0;
  document.getElementById("alerts-past-empty").hidden = past.length > 0;
  document.getElementById("alerts-active").parentElement.hidden = active.length === 0;
  document.getElementById("alerts-past").parentElement.hidden = past.length === 0;
}

// --- Regeln -----------------------------------------------------------------

let rulesState = null; // letzte Antwort von api/alert-rules

async function loadRules() {
  const status = document.getElementById("rules-status");
  try {
    const resp = await fetch("api/alert-rules", { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    rulesState = await resp.json();
    renderRules();
    status.textContent = "";
  } catch (e) {
    status.textContent = "Regeln laden fehlgeschlagen: " + e.message;
    status.className = "error";
  }
}

function numberInput(value, min, max, step, label) {
  const input = el("input");
  Object.assign(input, { type: "number", value, min, max, step, required: true });
  input.setAttribute("aria-label", label);
  input.inputMode = "decimal";
  return input;
}

function renderRules() {
  const rows = rulesState.rules.map(r => {
    const tr = el("tr");
    tr.dataset.kind = r.kind;
    const label = ALERT_LABELS[r.kind] || r.kind;

    const on = el("input");
    on.type = "checkbox";
    on.checked = r.enabled;
    on.className = "rule-enabled";
    on.setAttribute("aria-label", label + " aktiv");
    const tdOn = cell("on", null, null);
    tdOn.append(on);
    tr.append(tdOn);

    const name = cell("name", null, null);
    name.append(el("strong", label), el("span", RULE_HELP[r.kind] || "", "muted small rule-help"));
    tr.append(name);

    const thr = cell("thr", "Schwelle", null, "num");
    if (NO_THRESHOLD.includes(r.kind)) {
      thr.append(el("span", "–", "muted"));
    } else {
      const u = ruleUnit(r.kind);
      thr.append(numberInput(r.threshold, u.min, u.max, 1, label + " Schwelle " + u.aria), el("span", u.unit, "unit"));
      thr.querySelector("input").className = "rule-threshold";
    }
    tr.append(thr);

    const dur = cell("dur", "Mindestdauer", null, "num");
    // Sekunden als Minuten mit höchstens zwei Nachkommastellen; "any" erlaubt z. B. 0,33.
    dur.append(numberInput(Math.round(r.for_sec / 60 * 100) / 100, r.kind === "offline" ? 0.5 : 0, 1440, "any",
      label + " Mindestdauer in Minuten"),
      el("span", " min", "unit"));
    dur.querySelector("input").className = "rule-duration";
    tr.append(dur);
    return tr;
  });
  document.querySelector("#rules tbody").replaceChildren(...rows);
  document.getElementById("reminder-hours").value = rulesState.reminder_hours;
  document.getElementById("rules-hint").textContent =
    "Ein Alarm löst aus, wenn der Wert die Mindestdauer lang ununterbrochen über der Schwelle liegt, " +
    "und endet, wenn er " + rulesState.hysteresis + " Prozentpunkte (bei der Temperatur " + rulesState.hysteresis +
    " °C) unter die Schwelle fällt.";

  const chans = rulesState.channels.map(c => CHANNEL_LABELS[c] || c);
  document.getElementById("channels").textContent = chans.length
    ? "Benachrichtigungen gehen an: " + chans.join(", ") + ". Alarme erscheinen außerdem hier in der GUI."
    : "Keine externen Kanäle eingerichtet – Alarme erscheinen nur hier in der GUI. " +
      "ntfy und E-Mail werden über Umgebungsvariablen des Servers eingerichtet (siehe README).";
  document.getElementById("test-alert").hidden = chans.length === 0;
}

async function saveRules(ev) {
  ev.preventDefault();
  const status = document.getElementById("rules-status");
  const rules = [...document.querySelectorAll("#rules tbody tr")].map(tr => {
    const thr = tr.querySelector(".rule-threshold");
    return {
      kind: tr.dataset.kind,
      enabled: tr.querySelector(".rule-enabled").checked,
      threshold: thr ? Number(thr.value) : 0,
      for_sec: Math.round(Number(tr.querySelector(".rule-duration").value) * 60),
    };
  });
  status.className = "muted";
  status.textContent = "Speichere …";
  try {
    const resp = await fetch("api/alert-rules", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ rules, reminder_hours: Number(document.getElementById("reminder-hours").value) }),
    });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    rulesState = await resp.json();
    renderRules();
    status.className = "ok-text";
    status.textContent = "✓ Gespeichert";
  } catch (e) {
    status.className = "error";
    status.textContent = "Speichern fehlgeschlagen: " + e.message;
  }
}

async function sendTestAlert() {
  const btn = document.getElementById("test-alert");
  const out = document.getElementById("test-result");
  btn.disabled = true;
  out.replaceChildren(el("li", "Sende …", "muted"));
  try {
    const resp = await fetch("api/alerts/test", { method: "POST" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    const results = await resp.json();
    out.replaceChildren(...results.map(r => el("li",
      (CHANNEL_LABELS[r.channel] || r.channel) + ": " + (r.ok ? "✓ gesendet" : "✗ " + r.error),
      r.ok ? "ok-text" : "error")));
  } catch (e) {
    out.replaceChildren(el("li", "Fehlgeschlagen: " + e.message, "error"));
  } finally {
    btn.disabled = false;
  }
}

document.getElementById("rules-form").addEventListener("submit", saveRules);
document.getElementById("test-alert").addEventListener("click", sendTestAlert);

// --- Alarme je Host (in den Host-Details) -------------------------------------
// Wie der Verlauf eine dauerhafte Ansicht je Host: Das Element überlebt das
// Neuzeichnen der Liste, damit Eingaben nicht verloren gehen.

const hostAlertViews = new Map();
const MUTE_OPTIONS = [["1 h", 60], ["4 h", 240], ["24 h", 1440], ["unbefristet", 0]];
const MUTE_FOREVER_AFTER = Date.UTC(9000, 0, 1) / 1000;

function hostAlertSection(h) {
  let v = hostAlertViews.get(h.id);
  if (!v) {
    v = createHostAlertView(h);
    hostAlertViews.set(h.id, v);
    loadHostRules(v);
  }
  updateMuteStatus(v, h.muted_until);
  return v.el;
}

function ruleSummary(r) {
  if (!r.enabled) return "aus";
  const dur = Math.round(r.for_sec / 60 * 100) / 100 + " min";
  return r.kind === "offline" ? "nach " + dur : r.threshold + ruleUnit(r.kind).unit + " · " + dur;
}

function createHostAlertView(h) {
  const v = { id: h.id, state: null, el: el("section", null, "host-alert-settings") };
  v.el.append(el("h3", "Alarme für diesen Host"));

  v.muteText = el("p", "", "mute-status small");
  const row = el("div", null, "mute-row");
  const group = el("div", null, "segmented");
  group.setAttribute("role", "group");
  group.setAttribute("aria-label", "Stumm schalten für");
  for (const [label, minutes] of MUTE_OPTIONS) {
    const b = el("button", label);
    b.type = "button";
    b.addEventListener("click", () => setMute(v, minutes));
    group.append(b);
  }
  v.unmute = el("button", "Aufheben", "link");
  v.unmute.type = "button";
  v.unmute.addEventListener("click", () => setMute(v, null));
  row.append(el("span", "Stumm:", "muted small"), group, v.unmute);
  v.el.append(v.muteText, row);

  v.form = el("form", null, "host-rules");
  const table = el("table", null, "mini");
  const head = el("tr");
  for (const [t, cls] of [["Regel"], ["Eigene", "num"], ["Aktiv", "num"], ["Schwelle", "num"], ["Dauer", "num"]]) {
    head.append(el("th", t, cls));
  }
  const thead = el("thead");
  thead.append(head);
  v.tbody = el("tbody");
  table.append(thead, v.tbody);
  const actions = el("div", null, "form-actions");
  const save = el("button", "Speichern", "secondary");
  save.type = "submit";
  v.status = el("span", "", "small");
  v.status.setAttribute("role", "status");
  actions.append(save, v.status);
  v.form.append(table, actions);
  v.form.addEventListener("submit", ev => { ev.preventDefault(); saveHostRules(v); });
  v.el.append(v.form);
  return v;
}

function updateMuteStatus(v, until) {
  v.unmute.hidden = !until;
  if (!until) v.muteText.textContent = "Benachrichtigungen aktiv.";
  else if (until > MUTE_FOREVER_AFTER) v.muteText.textContent = "🔕 Stumm (unbefristet) – Alarme werden erfasst, aber nicht gemeldet.";
  else v.muteText.textContent = "🔕 Stumm bis " + fmtDateTime(until) + " – Alarme werden erfasst, aber nicht gemeldet.";
}

async function setMute(v, minutes) {
  try {
    const opts = minutes === null
      ? { method: "DELETE" }
      : { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ minutes }) };
    const resp = await fetch("api/hosts/" + encodeURIComponent(v.id) + "/mute", opts);
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    await refresh();
  } catch (e) {
    v.muteText.textContent = "Stummschalten fehlgeschlagen: " + e.message;
  }
}

async function loadHostRules(v) {
  try {
    const resp = await fetch("api/hosts/" + encodeURIComponent(v.id) + "/alert-rules", { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    v.state = await resp.json();
    renderHostRules(v);
  } catch (e) {
    v.status.className = "small error";
    v.status.textContent = "Laden fehlgeschlagen: " + e.message;
  }
}

function renderHostRules(v) {
  const rows = v.state.defaults.map(d => {
    const own = v.state.rules.find(r => r.kind === d.kind);
    const r = own || d;
    const label = ALERT_LABELS[d.kind] || d.kind;
    const tr = el("tr");
    tr.dataset.kind = d.kind;

    const name = el("td", null, "name");
    name.append(el("strong", label), el("span", "Standard: " + ruleSummary(d), "muted small rule-help"));

    const check = (cls, checked, aria) => {
      const c = el("input");
      c.type = "checkbox";
      c.className = cls;
      c.checked = checked;
      c.setAttribute("aria-label", label + " " + aria);
      const td = el("td", null, "num");
      td.append(c);
      return td;
    };
    const tdOwn = check("own", !!own, "eigene Regel");
    const tdOn = check("rule-enabled", r.enabled, "aktiv");

    const tdThr = el("td", null, "num");
    if (d.kind === "offline") tdThr.append(el("span", "–", "muted"));
    else {
      const u = ruleUnit(d.kind);
      const i = numberInput(r.threshold, u.min, u.max, 1, label + " Schwelle " + u.aria);
      i.className = "rule-threshold";
      tdThr.append(i, el("span", u.unit, "unit"));
    }
    const tdDur = el("td", null, "num");
    const di = numberInput(Math.round(r.for_sec / 60 * 100) / 100, d.kind === "offline" ? 0.5 : 0, 1440, "any",
      label + " Mindestdauer in Minuten");
    di.className = "rule-duration";
    tdDur.append(di, el("span", " min", "unit"));

    tr.append(name, tdOwn, tdOn, tdThr, tdDur);
    const sync = () => {
      const on = tdOwn.querySelector("input").checked;
      for (const i of tr.querySelectorAll("input:not(.own)")) i.disabled = !on;
      tr.classList.toggle("inherited", !on);
    };
    tdOwn.querySelector("input").addEventListener("change", sync);
    sync();
    return tr;
  });
  v.tbody.replaceChildren(...rows);
}

async function saveHostRules(v) {
  const rules = [...v.tbody.querySelectorAll("tr")].filter(tr => tr.querySelector(".own").checked).map(tr => {
    const thr = tr.querySelector(".rule-threshold");
    return {
      kind: tr.dataset.kind,
      enabled: tr.querySelector(".rule-enabled").checked,
      threshold: thr ? Number(thr.value) : 0,
      for_sec: Math.round(Number(tr.querySelector(".rule-duration").value) * 60),
    };
  });
  v.status.className = "small muted";
  v.status.textContent = "Speichere …";
  try {
    const resp = await fetch("api/hosts/" + encodeURIComponent(v.id) + "/alert-rules", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ rules }),
    });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    v.state = await resp.json();
    renderHostRules(v);
    v.status.className = "small ok-text";
    v.status.textContent = rules.length ? "✓ Gespeichert" : "✓ Gespeichert – alle Regeln wie Standard";
  } catch (e) {
    v.status.className = "small error";
    v.status.textContent = "Speichern fehlgeschlagen: " + e.message;
  }
}
