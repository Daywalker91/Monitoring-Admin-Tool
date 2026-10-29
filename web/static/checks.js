// Externe Checks: Liste, Formular zum Anlegen/Bearbeiten und Verlauf der
// Antwortzeit. Nutzt el(), cell(), fmtAgo(), fmtDateTime(), toLogin() und Charts
// erst beim Aufruf. Werte werden ausschließlich per textContent gesetzt.
"use strict";

const CHECK_KINDS = {
  http: { label: "HTTP(S)", placeholder: "https://beispiel.de/health" },
  ping: { label: "Ping", placeholder: "192.168.1.10 oder nas.lan" },
  tcp: { label: "TCP-Port", placeholder: "nas.lan:22" },
};
const CHECK_HISTORY_POINTS = 288;

let checksList = [];
let checksAvailable = true; // false, wenn der Server keine Checks kennt (404)
const openChecks = new Set();
const checkCharts = new Map(); // id → { el, chart, fetchedAt }

function fmtMs(v) {
  if (v == null) return "–";
  if (v < 0.1) return "< 0.1 ms";
  return (v < 10 ? v.toFixed(1) : Math.round(v)) + " ms";
}

// Restlaufzeit des Zertifikats in Tagen (ganze Tage, abgerundet).
function certDaysLeft(r) { return r?.cert_not_after ? Math.floor((r.cert_not_after - Date.now() / 1000) / 86400) : null; }

async function loadChecks() {
  if (!checksAvailable) return;
  try {
    const resp = await fetch("api/checks", { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    if (resp.status === 404) {
      checksAvailable = false;
      document.querySelector('#tabs [data-view="checks"]').hidden = true;
      return;
    }
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    checksList = await resp.json();
    renderChecks();
  } catch (e) {
    console.warn("Checks laden fehlgeschlagen:", e);
  }
}

function checkState(c) {
  if (!c.enabled) return ["aus", "dot"];
  if (!c.last) return ["wartet", "dot"];
  return c.last.ok ? ["ok", "dot online"] : ["fehler", "dot crit"];
}

function checkRow(c) {
  const tr = el("tr", null, "host check" + (openChecks.has(c.id) ? " open" : ""));
  const [stateText, dotCls] = checkState(c);
  const status = cell("status", null, null);
  status.append(el("span", null, dotCls), stateText);
  tr.append(status);

  const name = cell("host", null, null);
  name.append(el("span", c.name, "hostname"));
  if (c.alerts) name.append(el("span", alertBadgeText(c.alerts), "pill crit host-alerts"));
  tr.append(name);
  tr.append(cell("kind", "Art", CHECK_KINDS[c.kind]?.label || c.kind));
  tr.append(cell("target", "Ziel", c.target, "target"));

  const r = c.last;
  let result = "–";
  if (r) result = r.ok ? fmtMs(r.latency_ms) + (r.status ? " · " + r.status : "") : r.error || "fehlgeschlagen";
  tr.append(cell("result", "Ergebnis", result, r && !r.ok ? "error-text" : ""));

  const days = certDaysLeft(r);
  const cert = cell("cert", "Zertifikat", days == null ? "–" : days < 0 ? "abgelaufen" : days + " Tage", "num");
  if (days != null && days < 7) cert.classList.add("lvl-crit");
  else if (days != null && days < 30) cert.classList.add("lvl-warn");
  tr.append(cert);

  const up = c.uptime_24h;
  const upCell = cell("uptime", "24 h", up == null ? "–" : up.toFixed(up === 100 ? 0 : 1) + " %", "num");
  if (up != null && up < 99) upCell.classList.add(up < 90 ? "lvl-crit" : "lvl-warn");
  tr.append(upCell);
  tr.append(cell("seen", "Zuletzt", r ? fmtAgo(r.ts) : "–"));

  tr.addEventListener("click", () => {
    if (openChecks.has(c.id)) openChecks.delete(c.id); else openChecks.add(c.id);
    renderChecks();
  });
  return tr;
}

function checkDetailRow(c) {
  const tr = el("tr", null, "detail");
  const td = el("td");
  td.colSpan = 8;
  const box = el("div", null, "check-detail");

  let v = checkCharts.get(c.id);
  if (!v) {
    v = { fetchedAt: 0, loading: false };
    v.chart = Charts.lineChart({ title: "Antwortzeit (24 h)", format: (n, axis) => axis ? Math.round(n) + " ms" : fmtMs(n),
      series: [{ key: "latency_ms", name: "Antwortzeit", cls: "s1" }] });
    checkCharts.set(c.id, v);
  }
  v.interval = c.interval_sec;
  box.append(v.chart.el);
  loadCheckHistory(c.id, v);

  const info = el("dl", null, "info");
  const add = (k, val) => info.append(el("dt", k), el("dd", val || "–"));
  add("Intervall", c.interval_sec + " s, Timeout " + c.timeout_sec + " s");
  if (c.kind === "http") {
    add("Erwartet", c.expect_status ? "Status " + c.expect_status : "Status 200–399");
    if (c.target.startsWith("https:")) add("Zertifikat", c.skip_verify ? "nicht geprüft (selbst signiert erlaubt)" : "wird geprüft");
  }
  if (c.last?.cert_not_after) add("Gültig bis", fmtDateTime(c.last.cert_not_after));
  if (c.last && !c.last.ok) add("Fehler", c.last.error);
  box.append(info);

  const actions = el("div", null, "form-actions");
  const edit = el("button", "Bearbeiten", "secondary");
  edit.type = "button";
  edit.addEventListener("click", ev => { ev.stopPropagation(); openCheckForm(c); });
  const del = el("button", "Löschen", "danger");
  del.type = "button";
  del.addEventListener("click", ev => { ev.stopPropagation(); deleteCheck(c); });
  actions.append(edit, del);
  box.append(actions);

  td.append(box);
  tr.append(td);
  return tr;
}

async function loadCheckHistory(id, v) {
  if (v.loading || Date.now() - v.fetchedAt < 30000) return;
  v.loading = true;
  try {
    const resp = await fetch(`api/checks/${id}/history?hours=24&points=${CHECK_HISTORY_POINTS}`, { cache: "no-store" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    const data = await resp.json();
    v.fetchedAt = Date.now();
    const end = Date.now() / 1000;
    const start = end - 86400;
    const gap = Math.max((86400 / CHECK_HISTORY_POINTS) * 2.5, (v.interval || 60) * 3);
    v.chart.update(data, start, end, gap);
  } catch (e) {
    console.warn("Check-Verlauf laden fehlgeschlagen:", e);
  } finally {
    v.loading = false;
  }
}

function renderChecks() {
  const rows = [];
  for (const c of checksList) {
    rows.push(checkRow(c));
    if (openChecks.has(c.id)) rows.push(checkDetailRow(c));
  }
  document.querySelector("#checks tbody").replaceChildren(...rows);
  document.getElementById("checks-empty").hidden = checksList.length > 0;
  document.getElementById("checks-table").hidden = checksList.length === 0;
}

// --- Formular ----------------------------------------------------------------

let editingCheck = null; // null = neuer Check

function openCheckForm(c) {
  editingCheck = c || null;
  const f = document.getElementById("check-form");
  f.hidden = false;
  document.getElementById("check-form-title").textContent = c ? "Check bearbeiten" : "Neuer Check";
  f.elements.name.value = c?.name || "";
  f.elements.kind.value = c?.kind || "http";
  f.elements.target.value = c?.target || "";
  f.elements.interval.value = c?.interval_sec || 60;
  f.elements.timeout.value = c?.timeout_sec || 10;
  f.elements.expect.value = c?.expect_status || "";
  f.elements.skip_verify.checked = !!c?.skip_verify;
  f.elements.enabled.checked = c ? c.enabled : true;
  document.getElementById("check-form-status").textContent = "";
  updateCheckForm();
  f.scrollIntoView({ block: "nearest" });
  f.elements.name.focus();
}

// Felder je nach Art ein- und ausblenden.
function updateCheckForm() {
  const f = document.getElementById("check-form");
  const kind = f.elements.kind.value;
  f.elements.target.placeholder = CHECK_KINDS[kind].placeholder;
  document.getElementById("check-http-opts").hidden = kind !== "http";
  document.getElementById("check-skip-row").hidden = !(kind === "http" && f.elements.target.value.startsWith("https:"));
}

async function saveCheck(ev) {
  ev.preventDefault();
  const f = ev.target;
  const out = document.getElementById("check-form-status");
  const body = {
    name: f.elements.name.value,
    kind: f.elements.kind.value,
    target: f.elements.target.value,
    interval_sec: Number(f.elements.interval.value),
    timeout_sec: Number(f.elements.timeout.value),
    expect_status: f.elements.kind.value === "http" ? Number(f.elements.expect.value || 0) : 0,
    skip_verify: f.elements.skip_verify.checked,
    enabled: f.elements.enabled.checked,
  };
  out.textContent = "Speichern …";
  try {
    const resp = await fetch(editingCheck ? "api/checks/" + editingCheck.id : "api/checks", {
      method: editingCheck ? "PUT" : "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    const saved = await resp.json();
    f.hidden = true;
    out.textContent = "";
    openChecks.add(saved.id);
    checkCharts.delete(saved.id);
    setTimeout(loadChecks, 1500); // erstes Ergebnis abwarten
    await loadChecks();
  } catch (e) {
    out.textContent = "Fehler: " + e.message;
  }
}

async function deleteCheck(c) {
  if (!confirm("Check „" + c.name + "“ samt Verlauf und Alarmen löschen?")) return;
  try {
    const resp = await fetch("api/checks/" + c.id, { method: "DELETE" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok && resp.status !== 404) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    openChecks.delete(c.id);
    checkCharts.delete(c.id);
    await loadChecks();
  } catch (e) {
    alert("Löschen fehlgeschlagen: " + e.message);
  }
}

document.getElementById("check-add").addEventListener("click", () => openCheckForm(null));
document.getElementById("check-cancel").addEventListener("click", () => { document.getElementById("check-form").hidden = true; });
document.getElementById("check-form").addEventListener("submit", saveCheck);
document.getElementById("check-form").elements.kind.addEventListener("change", updateCheckForm);
document.getElementById("check-form").elements.target.addEventListener("input", updateCheckForm);
