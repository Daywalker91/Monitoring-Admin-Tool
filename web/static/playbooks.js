// Tab „Playbooks“ (M12a/b): Liste der Playbooks aus MAT_PLAYBOOK_DIR, Probelauf
// (Einschätzung Host × Schritt) und Start mit Bestätigung, Laufansicht Host × Schritt, Verlauf.
// Nutzt el, fetchJSON, fmtDateTime, fmtDuration, toLogin, ACTION_LABELS,
// openJobByID aus den anderen Dateien. Werte nur per textContent.
"use strict";

const RUN_STATUS = {
  running: ["läuft", "muted"], ok: ["erfolgreich", "ok-text"], failed: ["mit Fehlern", "error-text"],
  stopped: ["angehalten", "error-text"], cancelled: ["abgebrochen", "muted"], interrupted: ["unterbrochen", "error-text"],
};
const STEP_STATUS = {
  pending: ["·", "wartet", "step-pending"], running: ["…", "läuft", "step-running"], ok: ["✓", "ok", "step-ok"],
  failed: ["✗", "Fehler", "step-failed"], skipped: ["–", "übersprungen", "step-skipped"],
};

const pb = { list: null, openRun: null, run: null };

const GATE_LABELS = {
  host_healthy: "warten bis Host gesund", cluster_healthy: "warten bis Cluster gesund", check_ok: "warten bis Check ok:",
};

function stepLabel(st) {
  if (st.gate) {
    return (GATE_LABELS[st.gate] || st.gate) + (st.check ? " " + st.check : "") +
      " (höchstens " + (st.timeout || "10m0s") + ")";
  }
  if (st.approve) return "Freigabe: " + st.approve + (st.once ? " (gilt für alle Hosts)" : "");
  if (!st.action) return "warten " + st.wait;
  if (st.action === "command.run") return "Befehl " + (st.params?.name || "");
  const params = Object.entries(st.params || {}).map(([k, v]) => k + "=" + v).join(", ");
  return (ACTION_LABELS[st.action] || st.action) + (params ? " (" + params + ")" : "");
}

async function postPlaybook(url) {
  const resp = await fetch(url, { method: "POST" });
  if (resp.status === 401) { toLogin(); throw new Error("nicht angemeldet"); }
  if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
  return resp.status === 204 ? null : resp.json();
}

function pbStatus(text, isError) {
  const s = document.getElementById("pb-status");
  s.className = "small " + (isError ? "error-text" : "muted");
  s.textContent = text;
}

// loadPlaybooks wird bei jedem Refresh im Tab aufgerufen.
async function loadPlaybooks() {
  const [list, runs] = await Promise.all([fetchJSON("api/playbooks"), fetchJSON("api/playbook-runs")]);
  pb.list = list;
  renderPlaybookList(list);
  renderRunHistory(runs);
  if (pb.openRun) await loadRun(pb.openRun);
}

function renderPlaybookList(list) {
  const off = document.getElementById("pb-disabled");
  off.hidden = list.enabled;
  document.getElementById("pb-reload").hidden = !list.enabled;
  const dirErr = document.getElementById("pb-dir-error");
  dirErr.textContent = list.error ? "Ordner nicht lesbar: " + list.error : "";
  dirErr.hidden = !list.error;
  document.getElementById("pb-loaded").textContent = list.loaded_at ? "geladen " + fmtDateTime(list.loaded_at) : "";
  document.getElementById("pb-empty").hidden = !list.enabled || list.files.length > 0;
  const table = document.getElementById("pb-list");
  table.hidden = list.files.length === 0;
  table.tBodies[0].replaceChildren(...list.files.map(f => {
    const p = f.playbook;
    const tr = el("tr");
    const name = el("td", null, "name");
    name.append(el("span", p ? p.name : f.file, "hostname"), el("span", f.file, "agent-id"));
    if (p?.description) name.append(el("div", p.description, "muted small"));
    if (f.schedule) name.append(scheduleLine(p, f.schedule));
    tr.append(name, el("td", p ? p.targets.join(", ") : "–", "hide-sm"),
      el("td", p ? p.steps.length + " · " + (p.mode === "parallel" ? "parallel" : "nacheinander") : "–", "nowrap"));
    const state = el("td");
    if (f.error) {
      state.append(el("span", "fehlerhaft", "error-text"), el("pre", f.error, "pb-error small"));
    } else if (f.running) {
      const b = el("button", "Lauf ansehen", "secondary");
      b.type = "button";
      b.addEventListener("click", () => openRun(f.running));
      state.append(b);
    } else {
      const b = el("button", "Starten", "primary");
      b.type = "button";
      b.addEventListener("click", () => openProbe(p.name, b, true));
      const probe = el("button", "Probelauf", "secondary");
      probe.type = "button";
      probe.addEventListener("click", () => openProbe(p.name, probe, false));
      const tools = el("span", null, "pb-actions");
      tools.append(b, probe);
      state.append(tools);
    }
    tr.append(state);
    return tr;
  }));
}

// scheduleLine: Zeitplan mit nächstem Termin und Pausieren/Fortsetzen.
function scheduleLine(p, sched) {
  const line = el("div", null, "small pb-schedule");
  const text = "⏱ " + p.schedule + (sched.paused ? " · pausiert" :
    sched.next ? " · nächster Lauf " + fmtDateTime(sched.next) : "");
  line.append(el("span", text, sched.paused ? "warn-text" : "muted"));
  const b = el("button", sched.paused ? "Fortsetzen" : "Pausieren", "secondary small-btn");
  b.type = "button";
  b.addEventListener("click", async () => {
    b.disabled = true;
    try {
      const resp = await fetch("api/playbooks/" + encodeURIComponent(p.name) + "/schedule", {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ paused: !sched.paused }),
      });
      if (resp.status === 401) { toLogin(); return; }
      if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
      await loadPlaybooks();
    } catch (e) {
      pbStatus(e.message, true);
      b.disabled = false;
    }
  });
  line.append(b);
  return line;
}

function renderRunHistory(runs) {
  document.getElementById("pb-runs-empty").hidden = runs.length > 0;
  const table = document.getElementById("pb-runs");
  table.hidden = runs.length === 0;
  table.tBodies[0].replaceChildren(...runs.map(r => {
    const tr = el("tr", null, "job-row" + (r.id === pb.openRun ? " open" : ""));
    const [text, cls] = RUN_STATUS[r.status] || [r.status, ""];
    const end = r.finished_at ?? Date.now() / 1000;
    tr.append(el("td", fmtDateTime(r.created_at), "num nowrap"), el("td", r.name, "name"),
      el("td", r.user || "–", "hide-sm nowrap"), el("td", text, cls),
      el("td", r.message || "", "hide-sm small"), el("td", fmtDuration(end - r.created_at), "num nowrap"));
    tr.title = "Lauf anzeigen";
    tr.addEventListener("click", () => openRun(r.id));
    return tr;
  }));
}

// --- Start mit Bestätigung ------------------------------------------------------

let pbDialog = null;

function getStartDialog() {
  if (pbDialog) return pbDialog;
  const d = { el: el("dialog", null, "job-dialog pb-dialog") };
  d.title = el("h2", "");
  d.body = el("div");
  d.status = el("p", "", "small error-text");
  d.start = el("button", "Starten", "primary");
  d.start.type = "button";
  d.cancel = el("button", "Abbrechen", "secondary");
  d.cancel.type = "button";
  d.cancel.addEventListener("click", () => d.el.close());
  const actions = el("div", null, "form-actions");
  actions.append(d.start, d.cancel, d.status);
  d.el.append(d.title, d.body, actions);
  d.el.addEventListener("click", ev => { if (ev.target === d.el) d.el.close(); });
  document.body.append(d.el);
  pbDialog = d;
  return d;
}

// Probelauf: Einschätzung je Host und Schritt (Server sendet nichts).
const PROBE_STATE = { ok: ["✓", "step-ok"], info: ["ℹ", "probe-info"], warn: ["⚠", "probe-warn"], fail: ["✗", "step-failed"] };

function probeTable(preview) {
  const steps = preview.playbook.steps;
  const head = el("tr");
  head.append(el("th", "Host"));
  steps.forEach((st, i) => {
    const th = el("th", (i + 1) + ". " + st.id);
    th.title = stepLabel(st);
    head.append(th);
  });
  const table = el("table", null, "mini pb-grid");
  table.append(el("thead"), el("tbody"));
  table.tHead.append(head);
  for (const h of preview.hosts) {
    const tr = el("tr");
    const name = el("td", h.name, "name nowrap");
    if (!h.online) name.append(el("div", "offline", "warn-text small"));
    tr.append(name);
    h.steps.forEach((ps, i) => {
      const [sym, cls] = PROBE_STATE[ps.state] || ["?", ""];
      const td = el("td", null, "pb-step " + cls);
      td.append(el("span", sym), el("div", ps.text, "small pb-msg"));
      if (steps[i].when) td.append(el("div", "nur wenn " + steps[i].when.step + " …", "muted small"));
      td.title = stepLabel(steps[i]);
      tr.append(td);
    });
    table.tBodies[0].append(tr);
  }
  const wrap = el("div", null, "table-wrap probe-wrap");
  wrap.append(table);
  return wrap;
}

// openProbe zeigt den Probelauf; mit startable zusätzlich den Start.
async function openProbe(name, btn, startable) {
  btn.disabled = true;
  let preview;
  try {
    preview = await fetchJSON("api/playbooks/" + encodeURIComponent(name) + "/preview");
  } catch (e) {
    pbStatus(name + ": " + e.message, true);
    btn.disabled = false;
    return;
  }
  btn.disabled = false;
  const p = preview.playbook;
  const d = getStartDialog();
  d.title.textContent = startable ? "Playbook „" + p.name + "“ starten?" : "Probelauf „" + p.name + "“";
  d.status.textContent = "";
  const count = { fail: 0, warn: 0 };
  for (const h of preview.hosts) for (const ps of h.steps) if (ps.state in count) count[ps.state]++;
  const blockers = (preview.blockers || []).map(b => el("p", "⛔ " + b, "banner warn small"));
  const steps = el("ol", null, "pb-steps");
  for (const st of p.steps) {
    const li = el("li", stepLabel(st));
    const extra = [];
    if (st.when) extra.push("nur wenn " + st.when.step + (st.when.exit_code !== undefined ? " Exit-Code " + st.when.exit_code : st.when.ok ? " ok" : " fehlgeschlagen"));
    if (st.ok_exit_codes) extra.push("ok bei Exit-Code " + st.ok_exit_codes.join(", "));
    if ((st.on_error || p.on_error) === "continue") extra.push("bei Fehler weiter");
    if (extra.length) li.append(el("span", " · " + extra.join(" · "), "muted small"));
    steps.append(li);
  }
  const mode = p.mode === "parallel" ? "parallel, bis " + (p.max_parallel || 10) + " Hosts gleichzeitig" : "ein Host nach dem anderen";
  const summary = [];
  if (count.fail) summary.push(count.fail + " Schritt(e) schlagen nach heutigem Stand sicher fehl (✗).");
  if (count.warn) summary.push(count.warn + " Schritt(e) sind gerade nicht erfüllt (⚠), z. B. Host offline oder Gate offen.");
  summary.push("Gates zeigen den Stand von jetzt. Bei einem Fehler " +
    ((p.on_error || "stop") === "stop" ? "hält der Lauf an" : "geht es weiter") + " (sofern nicht je Schritt anders).");
  d.body.replaceChildren(
    el("p", p.description || "", "muted"), ...blockers,
    el("h3", "Schritte"), steps,
    el("h3", "Probelauf (" + preview.hosts.length + (preview.hosts.length === 1 ? " Host, " : " Hosts, ") + mode + ")"),
    probeTable(preview),
    el("p", summary.join(" "), "small"));
  d.start.hidden = !startable;
  d.cancel.textContent = startable ? "Abbrechen" : "Schließen";
  d.start.disabled = blockers.length > 0;
  d.start.onclick = async () => {
    d.start.disabled = true;
    try {
      const run = await postPlaybook("api/playbooks/" + encodeURIComponent(p.name) + "/run");
      d.el.close();
      pbStatus("");
      await openRun(run.id);
      loadPlaybooks();
    } catch (e) {
      d.status.textContent = e.message;
      d.start.disabled = false;
    }
  };
  if (!d.el.open) d.el.showModal();
}

// --- Laufansicht ------------------------------------------------------------------

async function openRun(id) {
  pb.openRun = id;
  await loadRun(id);
  document.getElementById("pb-run").scrollIntoView({ block: "nearest" });
}

async function loadRun(id) {
  let r;
  try {
    r = await fetchJSON("api/playbook-runs/" + encodeURIComponent(id));
  } catch (e) {
    pbStatus("Lauf: " + e.message, true);
    return;
  }
  if (pb.openRun !== id) return;
  pb.run = r;
  renderRun(r);
}

function renderRun(r) {
  const box = document.getElementById("pb-run");
  box.hidden = false;
  const [text, cls] = RUN_STATUS[r.status] || [r.status, ""];
  document.getElementById("pb-run-title").textContent = "Lauf „" + r.name + "“";
  const meta = document.getElementById("pb-run-meta");
  meta.className = "small " + cls;
  const end = r.finished_at ?? r.now;
  meta.textContent = fmtDateTime(r.created_at) + " · " + (r.user || "–") + " · " + text +
    (r.cancel && !r.finished_at ? " (Abbruch vorgemerkt: endet nach dem aktuellen Schritt)" : "") +
    " · " + fmtDuration(end - r.created_at) + (r.message ? " · " + r.message : "");
  const cancel = document.getElementById("pb-run-cancel");
  cancel.hidden = r.finished_at !== null || r.cancel;

  const steps = r.playbook?.steps || [];
  const head = el("tr");
  head.append(el("th", "Host"));
  steps.forEach((st, i) => {
    const th = el("th", (i + 1) + ". " + st.id);
    th.title = stepLabel(st);
    head.append(th);
  });
  const rows = r.hosts.map(hostID => {
    const tr = el("tr");
    tr.append(el("td", r.host_names[hostID] || hostID, "name nowrap"));
    for (const st of r.steps.filter(s => s.host_id === hostID)) {
      const [sym, label, scls] = STEP_STATUS[st.status] || ["?", st.status, ""];
      const td = el("td", null, "pb-step " + scls);
      td.append(el("span", sym + " " + label));
      if (st.started_at) {
        const stop = st.finished_at || r.now;
        td.append(el("span", " " + fmtDuration(stop - st.started_at), "muted small"));
      }
      if (st.status === "running" && st.until && st.until > r.now) {
        const def = steps[st.index] || {};
        td.append(el("div", (def.gate || def.approve ? "Frist noch " : "noch ") + fmtDuration(st.until - r.now), "muted small"));
      }
      if (st.message) td.append(el("div", st.message, "small pb-msg"));
      if (st.status === "running" && steps[st.index]?.approve && !r.finished_at) td.append(approveButtons(r, hostID, st.index));
      const tip = [stepLabel(steps[st.index] || {}), st.message].filter(Boolean);
      if (st.job_id) {
        td.classList.add("clickable");
        tip.push("Klick: Ausgabe anzeigen");
        td.addEventListener("click", () => openJobByID(hostID, st.job_id));
      }
      td.title = tip.join("\n");
      tr.append(td);
    }
    return tr;
  });
  const table = document.getElementById("pb-grid");
  table.tHead.replaceChildren(head);
  table.tBodies[0].replaceChildren(...rows);
}

// approveButtons: Freigeben oder Ablehnen eines wartenden Freigabe-Schritts.
function approveButtons(r, hostID, index) {
  const step = r.playbook.steps[index];
  const box = el("div", null, "pb-approve");
  for (const [label, ok, cls] of [["Freigeben", true, "primary"], ["Ablehnen", false, "danger"]]) {
    const b = el("button", label, cls + " small-btn");
    b.type = "button";
    b.addEventListener("click", async ev => {
      ev.stopPropagation();
      const who = step.once ? "alle Hosts dieses Laufs" : (r.host_names[hostID] || hostID);
      if (!confirm(label + " für " + who + "?\n\n" + step.approve)) return;
      box.querySelectorAll("button").forEach(x => { x.disabled = true; });
      try {
        const resp = await fetch("api/playbook-runs/" + encodeURIComponent(r.id) + "/approve", {
          method: "POST", headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ host: hostID, step: index, ok }),
        });
        if (resp.status === 401) { toLogin(); return; }
        if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
        await loadRun(r.id);
      } catch (e) {
        pbStatus(e.message, true);
        box.querySelectorAll("button").forEach(x => { x.disabled = false; });
      }
    });
    box.append(b);
  }
  return box;
}

document.getElementById("pb-run-cancel").addEventListener("click", async () => {
  const r = pb.run;
  if (!r || !confirm("Lauf „" + r.name + "“ abbrechen?\nEr endet nach dem aktuellen Schritt; ein laufender Job wird nicht abgebrochen.")) return;
  try {
    await postPlaybook("api/playbook-runs/" + encodeURIComponent(r.id) + "/cancel");
    await loadRun(r.id);
  } catch (e) {
    pbStatus(e.message, true);
  }
});

document.getElementById("pb-run-close").addEventListener("click", () => {
  pb.openRun = pb.run = null;
  document.getElementById("pb-run").hidden = true;
  loadPlaybooks();
});

document.getElementById("pb-reload").addEventListener("click", async ev => {
  ev.target.disabled = true;
  try {
    const list = await postPlaybook("api/playbooks/reload");
    renderPlaybookList(list);
    const bad = list.files.filter(f => f.error).length;
    pbStatus(list.files.length + " Playbooks geladen" + (bad ? ", " + bad + " fehlerhaft" : ""), bad > 0);
  } catch (e) {
    pbStatus(e.message, true);
  } finally {
    ev.target.disabled = false;
  }
});
