// Alarme: Markierung in der Kopfzeile, Alarmansicht und Regel-Einstellungen.
// Wird vor app.js geladen und nutzt dessen Hilfsfunktionen (el, cell, fmtPct, toLogin)
// erst beim Aufruf. Werte werden ausschließlich per textContent gesetzt.
"use strict";

const ALERT_LABELS = { offline: "Offline", cpu: "CPU", mem: "RAM", swap: "Swap", disk: "Disk" };
const RULE_HELP = {
  offline: "Agent nicht verbunden",
  cpu: "CPU-Auslastung gesamt",
  mem: "Arbeitsspeicher belegt",
  swap: "Swap belegt (nur Hosts mit Swap)",
  disk: "Belegung je Mountpoint",
};
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
  return new Date(unix * 1000).toLocaleString("de-DE",
    { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });
}

// Wert eines Alarms: Prozent, bei "offline" die Zeit ohne Verbindung (in Minuten gespeichert).
function alertValue(a, v) { return a.kind === "offline" ? fmtDuration(v * 60) : fmtPct(v); }

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
  if (a.kind !== "offline" && fmtPct(a.peak) !== fmtPct(a.value)) value += " (Spitze " + fmtPct(a.peak) + ")";
  tr.append(cell("value", "Wert", value, "num"));
  tr.append(cell("threshold", "Schwelle", a.kind === "offline" ? "–" : fmtPct(a.threshold), "num hide-sm"));
  tr.append(cell("start", "Beginn", fmtDateTime(a.started_at)));
  const end = active ? Date.now() / 1000 : a.resolved_at;
  tr.append(cell("dur", "Dauer", fmtDuration(end - a.started_at), "num"));

  tr.title = "Host anzeigen";
  tr.addEventListener("click", () => {
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
    if (r.kind === "offline") {
      thr.append(el("span", "–", "muted"));
    } else {
      thr.append(numberInput(r.threshold, 10, 100, 1, label + " Schwelle in Prozent"), el("span", " %", "unit"));
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
  document.getElementById("rules-hint").textContent =
    "Ein Alarm löst aus, wenn der Wert die Mindestdauer lang ununterbrochen über der Schwelle liegt, " +
    "und endet, wenn er " + rulesState.hysteresis + " Prozentpunkte unter die Schwelle fällt.";

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
      body: JSON.stringify({ rules }),
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
