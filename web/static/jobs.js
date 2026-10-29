// Aktionen (Jobs) in den Host-Details: Knöpfe für freigeschaltete Aktionen und
// der Verlauf (Audit-Log) mit Ausgabe. Nutzt el, fmtDateTime, toLogin, hostName,
// fetchJSON aus app.js/alerts.js erst beim Aufruf. Werte nur per textContent.
"use strict";

// Anzeigenamen der Aktionen (Whitelist steht in proto/jobs.go).
const ACTION_LABELS = {
  "agent.update": "Agent-Update",
  "service.start": "Dienst starten",
  "service.stop": "Dienst stoppen",
  "service.restart": "Dienst neu starten",
};

// Aktionen mit Knopf in den Host-Details; jede braucht ihre Fähigkeit.
// { action, cap, label, confirm(h) } – wird mit den Admin-Jobs gefüllt.
const HOST_ACTIONS = [];

const JOB_RELOAD_MS = 15000; // Verlauf neu laden; mit laufendem Job bei jedem Refresh

const jobViews = new Map();

function jobLabel(j) {
  const params = Object.entries(j.params || {}).map(([k, v]) => k + "=" + v).join(", ");
  return (ACTION_LABELS[j.action] || j.action) + (params ? " (" + params + ")" : "");
}

function jobState(j) {
  if (j.finished_at === null) return ["läuft …", "muted"];
  if (j.ok) return ["✓ erledigt", "ok-text"];
  if (j.exit_code === -1) return ["✗ keine Rückmeldung", "error-text"];
  return ["✗ " + (j.error || "fehlgeschlagen"), "error-text"];
}

function hostJobSection(h) {
  let v = jobViews.get(h.id);
  if (!v) {
    v = createJobView(h);
    jobViews.set(h.id, v);
  }
  renderJobButtons(v, h);
  const running = v.jobs?.some(j => j.finished_at === null);
  if (!v.loadedAt || running || Date.now() - v.loadedAt > JOB_RELOAD_MS) loadJobs(v);
  return v.el;
}

function createJobView(h) {
  const v = { id: h.id, jobs: null, loadedAt: 0, openJob: null, el: el("section", null, "host-jobs") };
  v.el.append(el("h3", "Aktionen"));
  v.buttons = el("div", null, "job-buttons");
  v.hint = el("p", "", "muted small");
  v.status = el("p", "", "small");
  v.status.setAttribute("role", "status");
  const table = el("table", null, "mini job-list");
  const head = el("tr");
  for (const [t, c] of [["Zeit", ""], ["Aktion", ""], ["Von", "hide-sm"], ["Status", ""]]) head.append(el("th", t, c));
  const thead = el("thead");
  thead.append(head);
  v.tbody = el("tbody");
  table.append(thead, v.tbody);
  v.empty = el("p", "Noch keine Aktionen.", "muted small");
  v.output = el("pre", "", "log-text job-output");
  v.output.hidden = true;
  v.el.append(v.buttons, v.hint, v.status, table, v.empty, v.output);
  v.el.addEventListener("click", ev => ev.stopPropagation());
  return v;
}

function renderJobButtons(v, h) {
  const caps = h.capabilities || [];
  const avail = HOST_ACTIONS.filter(a => caps.includes(a.cap));
  v.buttons.replaceChildren(...avail.map(a => {
    const b = el("button", a.label, "secondary");
    b.type = "button";
    b.disabled = !h.online;
    b.addEventListener("click", () => runJob(v, h, a, b));
    return b;
  }));
  v.buttons.hidden = avail.length === 0;
  const admin = caps.some(c => ["services", "packages", "reboot"].includes(c));
  v.hint.textContent = !admin ? "Admin-Aktionen sind für diesen Host nicht freigeschaltet (\"jobs\" in agent.json)."
    : avail.length && !h.online ? "Host ist offline." : "";
  v.hint.hidden = !v.hint.textContent;
}

// startJob schickt einen Job an den Host und zeigt ihn im Verlauf aufgeklappt.
async function startJob(hostID, action, params = {}) {
  const resp = await fetch("api/hosts/" + encodeURIComponent(hostID) + "/jobs", {
    method: "POST", headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ action, params }),
  });
  if (resp.status === 401) { toLogin(); throw new Error("nicht angemeldet"); }
  if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
  const j = await resp.json();
  const v = jobViews.get(hostID);
  if (v) {
    v.openJob = j.id;
    loadJobs(v);
  }
  return j;
}

async function runJob(v, h, a, btn) {
  if (!confirm(a.confirm(h))) return;
  btn.disabled = true;
  v.status.className = "small muted";
  v.status.textContent = a.label + " wird gestartet …";
  try {
    await startJob(h.id, a.action);
    v.status.textContent = "";
  } catch (e) {
    v.status.className = "small error-text";
    v.status.textContent = e.message;
  } finally {
    btn.disabled = false;
  }
}

async function loadJobs(v) {
  v.loadedAt = Date.now();
  try {
    v.jobs = await fetchJSON("api/hosts/" + encodeURIComponent(v.id) + "/jobs");
  } catch (e) {
    v.status.className = "small error-text";
    v.status.textContent = "Verlauf: " + e.message;
    return;
  }
  renderJobs(v);
  if (v.openJob) loadJobOutput(v, v.openJob);
}

function renderJobs(v) {
  v.empty.hidden = v.jobs.length > 0;
  v.tbody.parentElement.hidden = v.jobs.length === 0;
  v.tbody.replaceChildren(...v.jobs.map(j => {
    const tr = el("tr", null, "job-row" + (j.id === v.openJob ? " open" : ""));
    const [text, cls] = jobState(j);
    tr.append(el("td", fmtDateTime(j.created_at), "num"), el("td", jobLabel(j), "name"),
      el("td", j.user || "–", "hide-sm"), el("td", text, cls));
    tr.title = "Ausgabe anzeigen";
    tr.addEventListener("click", () => {
      v.openJob = v.openJob === j.id ? null : j.id;
      renderJobs(v);
      if (v.openJob) loadJobOutput(v, j.id);
      else v.output.hidden = true;
    });
    return tr;
  }));
}

async function loadJobOutput(v, id) {
  try {
    const j = await fetchJSON("api/jobs/" + encodeURIComponent(id));
    if (v.openJob !== id) return;
    const parts = [];
    if (j.output) parts.push(j.output);
    if (j.error) parts.push("Fehler: " + j.error);
    if (!parts.length) parts.push(j.finished_at === null ? "läuft noch …" : "(keine Ausgabe)");
    v.output.textContent = parts.join("\n");
    v.output.hidden = false;
  } catch (e) {
    v.output.textContent = "Ausgabe: " + e.message;
    v.output.hidden = false;
  }
}
