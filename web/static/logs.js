// Logs eines Hosts in den Host-Details: Quelle wählen, als Job lesen lassen,
// Zeilen filtern. Nutzt el, fetchJSON (app.js) und startJob (jobs.js) erst
// beim Aufruf. Werte nur per textContent.
"use strict";

const LOG_LINE_OPTIONS = [100, 200, 500, 1000];
const LOG_POLL_MS = 700;
const LOG_WAIT_MS = 75000;

const logViews = new Map();

function logSourceLabel(s) {
  if (s === "journal") return "Journal (alles)";
  if (s === "journal:kernel") return "Kernel";
  const [kind, name] = s.split(/:(.*)/s);
  return (kind === "eventlog" ? "Ereignisse: " : "Journal: ") + name;
}

function hostLogSection(h) {
  let v = logViews.get(h.id);
  if (!v) {
    v = createLogSection(h);
    logViews.set(h.id, v);
  }
  v.host = h;
  renderLogSection(v);
  return v.el;
}

function createLogSection(h) {
  const v = { id: h.id, host: h, text: "", sourcesKey: "", el: el("section", null, "host-logs") };
  v.el.append(el("h3", "Logs"));
  v.hint = el("p", "", "muted small");
  v.bar = el("div", null, "log-bar");
  v.source = el("select");
  v.source.setAttribute("aria-label", "Log-Quelle");
  v.lines = el("select");
  v.lines.setAttribute("aria-label", "Anzahl Zeilen");
  for (const n of LOG_LINE_OPTIONS) {
    const o = el("option", n + " Zeilen");
    o.value = String(n);
    v.lines.append(o);
  }
  v.lines.value = "200";
  v.load = el("button", "Laden", "secondary");
  v.load.type = "button";
  v.load.addEventListener("click", () => loadHostLog(v));
  v.filter = el("input");
  v.filter.type = "search";
  v.filter.placeholder = "filtern";
  v.filter.setAttribute("aria-label", "Zeilen bzw. Ereignisse filtern");
  v.filter.addEventListener("input", () => renderLogText(v));
  v.bar.append(v.source, v.lines, v.load, v.filter);
  v.status = el("p", "", "small");
  v.status.setAttribute("role", "status");
  v.out = el("pre", "", "log-text");
  v.out.hidden = true;
  v.el.append(v.hint, v.bar, v.status, v.out);
  v.el.addEventListener("click", ev => ev.stopPropagation());
  return v;
}

function renderLogSection(v) {
  const h = v.host;
  const sources = (h.capabilities || []).includes("logs") ? (h.log_sources || []) : [];
  const key = sources.join("|");
  if (key !== v.sourcesKey) {
    const keep = v.source.value;
    v.source.replaceChildren(...sources.map(s => {
      const o = el("option", logSourceLabel(s));
      o.value = s;
      return o;
    }));
    if (sources.includes(keep)) v.source.value = keep;
    v.sourcesKey = key;
  }
  v.bar.hidden = sources.length === 0;
  v.hint.textContent = sources.length === 0
    ? "Keine Logs freigegeben (\"log_sources\" in agent.json, z. B. [\"journal:sshd\", \"eventlog:System\"])."
    : h.online ? "" : "Host ist offline.";
  v.hint.hidden = !v.hint.textContent;
  v.load.disabled = !h.online || v.busy;
}

async function loadHostLog(v) {
  v.busy = true;
  v.load.disabled = true;
  v.status.className = "small muted";
  v.status.textContent = "lese " + logSourceLabel(v.source.value) + " …";
  try {
    const job = await startJob(v.id, "log.read", { source: v.source.value, lines: v.lines.value }, false);
    const deadline = Date.now() + LOG_WAIT_MS;
    let j = job;
    while (j.finished_at === null) {
      if (Date.now() > deadline) throw new Error("keine Antwort vom Agenten");
      await new Promise(r => setTimeout(r, LOG_POLL_MS));
      j = await fetchJSON("api/jobs/" + encodeURIComponent(job.id));
    }
    if (!j.ok) throw new Error(j.error || "fehlgeschlagen");
    v.text = j.output || "";
    v.status.className = "small muted";
    v.status.textContent = logSourceLabel(v.source.value) + " · " + new Date().toLocaleTimeString("de-DE");
    renderLogText(v);
  } catch (e) {
    v.status.className = "small error-text";
    v.status.textContent = e.message;
  } finally {
    v.busy = false;
    renderLogSection(v);
  }
}

function renderLogText(v) {
  const q = v.filter.value.trim().toLowerCase();
  // Windows-Ereignisse gehen über mehrere Zeilen: dort ganze Ereignisse filtern.
  const eventlog = v.source.value.startsWith("eventlog:");
  const parts = eventlog ? v.text.split(/\n(?=Event\[\d+\])/) : v.text.split("\n");
  const shown = q ? parts.filter(l => l.toLowerCase().includes(q)) : parts;
  v.out.textContent = shown.join("\n") || (q ? "(keine Treffer)" : "");
  v.out.hidden = !v.text;
  // Journal: neueste Zeilen unten, also ans Ende springen; Ereignisprotokoll: neueste oben.
  v.out.scrollTop = v.source.value.startsWith("eventlog:") ? 0 : v.out.scrollHeight;
}
