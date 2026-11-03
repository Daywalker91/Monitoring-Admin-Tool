// Web-Terminal (M11) in den Host-Details: Knopf „Terminal öffnen“ (nur wenn der
// Agent die Fähigkeit meldet), erneute Passworteingabe, Fenster mit xterm.js
// (vendor/xterm, lokal eingebunden) und die letzten Sitzungen aus dem Audit-Log.
// Nutzt el, fmtDateTime, fmtDuration, hostName, fetchJSON, toLogin aus
// app.js/alerts.js erst beim Aufruf. Fremddaten nur per textContent.
"use strict";

const TERM_RELOAD_MS = 30000; // Sitzungsliste neu laden
const TERM_LIST = 5;

const termViews = new Map();

function hostTerminalSection(h) {
  let v = termViews.get(h.id);
  if (!v) {
    v = createTermView(h);
    termViews.set(h.id, v);
  }
  const enabled = (h.capabilities || []).includes("terminal");
  v.el.hidden = !enabled;
  if (!enabled) return v.el;
  v.info.textContent = "Shell als " + (h.terminal_user || "?") + ". Eingaben und Ausgaben werden nicht gespeichert.";
  v.open.disabled = !h.online;
  v.open.title = h.online ? "" : "Host ist offline";
  if (!v.loadedAt || Date.now() - v.loadedAt > TERM_RELOAD_MS) loadTermSessions(v);
  return v.el;
}

function createTermView(h) {
  const v = { id: h.id, loadedAt: 0, el: el("section", null, "host-terminal") };
  v.el.append(el("h3", "Terminal"));
  v.info = el("p", "", "muted small");
  v.open = el("button", "Terminal öffnen", "secondary");
  v.open.type = "button";
  v.open.addEventListener("click", () => {
    const host = lastHosts.find(x => x.id === v.id);
    if (host) openTerminal(host, v);
  });
  const table = el("table", null, "mini term-list");
  const head = el("tr");
  for (const [t, c] of [["Beginn", ""], ["Von", "hide-sm"], ["Dauer", "num"], ["Ende", ""]]) head.append(el("th", t, c));
  const thead = el("thead");
  thead.append(head);
  v.tbody = el("tbody");
  table.append(thead, v.tbody);
  v.table = table;
  v.el.append(v.info, v.open, table);
  v.el.addEventListener("click", ev => ev.stopPropagation());
  return v;
}

async function loadTermSessions(v) {
  v.loadedAt = Date.now();
  let list;
  try {
    list = await fetchJSON("api/hosts/" + encodeURIComponent(v.id) + "/terminal-sessions");
  } catch {
    return; // Liste ist nur Zusatzinfo
  }
  const now = Date.now() / 1000;
  list = list.slice(0, TERM_LIST);
  v.table.hidden = list.length === 0;
  v.tbody.replaceChildren(...list.map(s => {
    const tr = el("tr");
    const end = s.ended_at ?? now;
    tr.append(el("td", fmtDateTime(s.started_at), "num nowrap"), el("td", s.user, "hide-sm"),
      el("td", fmtDuration(Math.max(0, end - s.started_at)), "num nowrap"),
      el("td", s.ended_at === null ? "läuft" : s.reason || "–", s.ended_at === null ? "ok-text" : "muted"));
    return tr;
  }));
}

// --- Fenster -------------------------------------------------------------------

let termDialog = null;

function getTermDialog() {
  if (termDialog) return termDialog;
  const d = { el: el("dialog", null, "job-dialog term-dialog") };
  const head = el("div", null, "job-dialog-head");
  d.title = el("h2", "");
  const close = el("button", "Schließen", "secondary");
  close.type = "button";
  close.addEventListener("click", () => d.el.close());
  head.append(d.title, close);
  d.status = el("p", "", "small");
  d.status.setAttribute("role", "status");

  // Erneute Passworteingabe (gilt nur für diese eine Sitzung).
  d.form = el("form", null, "job-form term-auth");
  const label = el("label", "Passwort (GUI-Anmeldung) zur Bestätigung");
  d.password = el("input");
  d.password.type = "password";
  d.password.autocomplete = "current-password";
  d.password.required = true;
  label.append(d.password);
  d.connect = el("button", "Verbinden", "primary");
  d.connect.type = "submit";
  d.form.append(label, d.connect);
  d.form.addEventListener("submit", async ev => {
    ev.preventDefault();
    // Doppeltes Absenden zählte sonst doppelt zur Sperre nach Fehlversuchen.
    if (d.busy) return;
    d.busy = true;
    try {
      await connectTerminal(d);
    } finally {
      d.busy = false;
    }
  });

  d.box = el("div", null, "term-box");
  d.el.append(head, d.status, d.form, d.box);
  d.el.addEventListener("close", () => endTerminal(d));
  document.body.append(d.el);
  termDialog = d;
  return d;
}

function openTerminal(h, view) {
  const d = getTermDialog();
  if (d.el.open) return;
  d.host = h;
  d.view = view;
  d.title.textContent = "Terminal · " + hostName(h) + " (als " + (h.terminal_user || "?") + ")";
  setTermStatus(d, "Passwort eingeben, um die Sitzung zu öffnen.", "muted");
  showTermAuth(d, true);
  d.password.value = "";
  d.connect.disabled = false;
  d.el.showModal();
  d.password.focus();
}

// showTermAuth wechselt zwischen Passwortabfrage (kleines Fenster) und Terminal.
function showTermAuth(d, auth) {
  d.form.hidden = !auth;
  d.box.hidden = auth;
  d.el.classList.toggle("term-auth-mode", auth);
}

function setTermStatus(d, text, cls) {
  d.status.className = "small " + cls;
  d.status.textContent = text;
}

async function connectTerminal(d) {
  if (typeof Terminal === "undefined") {
    setTermStatus(d, "xterm.js fehlt (vendor/xterm).", "error-text");
    return;
  }
  d.connect.disabled = true;
  setTermStatus(d, "verbinde …", "muted");

  // Terminal zuerst aufbauen, damit die Größe für den Agenten feststeht.
  showTermAuth(d, false);
  d.form.hidden = false; // bis das Ticket da ist, bleibt die Eingabe sichtbar
  d.box.replaceChildren();
  d.term = new Terminal({ cursorBlink: true, fontSize: 13, scrollback: 5000, convertEol: false,
    fontFamily: 'Consolas, "SFMono-Regular", Menlo, monospace',
    theme: { background: "#0d1117", foreground: "#e6edf3" } });
  d.fit = new FitAddon.FitAddon();
  d.term.loadAddon(d.fit);
  d.term.open(d.box);
  fitTerm(d);

  let ticket;
  try {
    const resp = await fetch("api/hosts/" + encodeURIComponent(d.host.id) + "/terminal", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password: d.password.value, cols: d.term.cols, rows: d.term.rows }),
    });
    d.password.value = "";
    if (resp.status === 401 && (await resp.clone().text()).includes("nicht angemeldet")) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    ticket = (await resp.json()).ticket;
  } catch (e) {
    disposeTerm(d);
    showTermAuth(d, true);
    d.connect.disabled = false;
    setTermStatus(d, e.message, "error-text");
    d.password.focus();
    return;
  }
  d.form.hidden = true;

  const base = location.pathname.replace(/[^/]*$/, "");
  const ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + base +
    "ws/terminal?ticket=" + encodeURIComponent(ticket));
  ws.binaryType = "arraybuffer";
  d.ws = ws;
  d.ended = false;
  const enc = new TextEncoder();
  const send = data => { if (ws.readyState === WebSocket.OPEN) ws.send(data); };
  d.term.onData(s => send(enc.encode(s)));
  d.term.onBinary(s => send(Uint8Array.from(s, c => c.charCodeAt(0) & 0xff)));
  d.term.onResize(({ cols, rows }) => send(JSON.stringify({ type: "resize", cols, rows })));
  d.resize = new ResizeObserver(() => fitTerm(d));
  d.resize.observe(d.box);

  ws.onopen = () => {
    setTermStatus(d, "verbunden – warte auf den Host …", "muted");
    d.term.focus();
  };
  let first = true;
  ws.onmessage = ev => {
    if (typeof ev.data !== "string") {
      if (first) {
        first = false;
        setTermStatus(d, "verbunden · Leerlauf und Gesamtdauer sind begrenzt", "ok-text");
      }
      d.term.write(new Uint8Array(ev.data));
      return;
    }
    try {
      const ctl = JSON.parse(ev.data);
      if (ctl.type === "exit") termEnded(d, ctl.reason || "beendet");
    } catch { /* unbekannte Nachricht */ }
  };
  ws.onclose = () => termEnded(d, "Verbindung geschlossen");
}

// fitTerm passt die Größe an das Fenster an, mit einer Spalte Reserve: Auf dem
// Handy (überlagerte Bildlaufleiste) schnitt die letzte Spalte sonst halb ab.
function fitTerm(d) {
  if (!d.term || !d.fit) return;
  const p = d.fit.proposeDimensions();
  if (!p || !(p.cols > 2) || !(p.rows > 1)) return;
  if (p.cols - 1 !== d.term.cols || p.rows !== d.term.rows) d.term.resize(p.cols - 1, p.rows);
}

function termEnded(d, reason) {
  if (d.ended) return;
  d.ended = true;
  setTermStatus(d, "Sitzung beendet: " + reason, "muted");
  if (d.term) d.term.options.disableStdin = true;
  if (d.view) loadTermSessions(d.view);
}

function disposeTerm(d) {
  if (d.resize) d.resize.disconnect();
  if (d.term) d.term.dispose();
  d.resize = d.term = d.fit = null;
}

function endTerminal(d) {
  d.ended = true;
  if (d.ws) {
    d.ws.onclose = d.ws.onmessage = null;
    d.ws.close();
    d.ws = null;
  }
  disposeTerm(d);
  d.box.replaceChildren();
  if (d.view) loadTermSessions(d.view);
}
