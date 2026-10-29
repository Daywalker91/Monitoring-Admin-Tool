// Agent-Updates: Hinweis in der Host-Liste, Abschnitt in den Host-Details und
// Übersicht unter „Einstellungen“. Nutzt die Hilfsfunktionen aus app.js/alerts.js
// (el, fmtDateTime, toLogin, refresh) erst beim Aufruf. Werte nur per textContent.
"use strict";

let updateStatus = null; // Antwort von /api/update

const UPDATE_STATES = {
  prepare: "Server lädt das Update …",
  running: "Agent lädt und prüft das Update …",
  restarting: "Installiert, Agent startet neu …",
  done: "✓ Aktualisiert",
  failed: "✗ Fehlgeschlagen",
};

function updateActive(u) { return u && ["prepare", "running", "restarting"].includes(u.state); }

// Kurzer Hinweis neben dem Hostnamen.
function hostUpdatePill(h) {
  if (updateActive(h.update)) return el("span", "⟳ Update läuft", "pill host-alerts");
  if (h.update?.state === "failed" || h.update_note) return el("span", "Update fehlgeschlagen", "pill warn host-alerts");
  if (h.update_available) return el("span", "⬆ " + h.update_available, "pill info host-alerts");
  return null;
}

// Abschnitt „Agent-Update“ in den Host-Details.
function hostUpdateSection(h) {
  const box = el("div", null, "host-update");
  const u = h.update;
  if (u) {
    let text = (UPDATE_STATES[u.state] || u.state) + " (" + u.target + ")";
    if (u.error) text += ": " + u.error;
    box.append(el("p", text, "small " + (u.state === "failed" ? "error" : u.state === "done" ? "ok-text" : "muted")));
  }
  if (h.update_note && !(u?.state === "failed" && u.error === h.update_note)) {
    box.append(el("p", "Agent meldet: " + h.update_note, "small error"));
  }
  if (h.update_available) {
    const b = el("button", "Auf " + h.update_available + " aktualisieren", "secondary");
    b.type = "button";
    b.addEventListener("click", ev => {
      ev.stopPropagation();
      startHostUpdate(h, b);
    });
    box.append(b);
  } else if (updateStatus?.enabled && !(h.capabilities || []).includes("update")) {
    box.append(el("p", "Self-Update nicht möglich: Der Agent läuft nicht als Windows-Dienst bzw. unter systemd.",
      "small muted"));
  }
  return box;
}

async function startHostUpdate(h, btn) {
  if (!confirm("Agent auf „" + hostName(h) + "“ auf " + h.update_available + " aktualisieren?\n" +
      "Der Agent lädt das signierte Release, prüft es und startet neu.")) return;
  btn.disabled = true;
  try {
    const resp = await fetch("api/hosts/" + encodeURIComponent(h.id) + "/update", { method: "POST" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    await refresh();
  } catch (e) {
    alert("Update nicht gestartet: " + e.message);
    btn.disabled = false;
  }
}

async function loadUpdateStatus() {
  try {
    updateStatus = await fetchJSON("api/update");
  } catch (e) {
    console.warn("Update-Status laden fehlgeschlagen:", e);
  }
}

// Hinweise über der Host-Liste: veraltete Agenten, bald ablaufendes Zertifikat.
function renderUpdateBanner(hosts) {
  const cert = document.getElementById("cert-banner");
  const left = updateStatus?.tls_not_after ? updateStatus.tls_not_after - Date.now() / 1000 : Infinity;
  cert.hidden = left >= 30 * 86400;
  if (!cert.hidden) {
    cert.textContent = left <= 0
      ? "Das Server-Zertifikat ist abgelaufen – Agenten können sich nicht mehr verbinden."
      : "Das Server-Zertifikat läuft am " + fmtDateTime(updateStatus.tls_not_after) +
        " ab. Neues Zertifikat erzeugen und server.crt auf allen Agenten hinterlegen.";
  }

  const banner = document.getElementById("update-banner");
  const outdated = hosts.filter(h => h.update_available).length;
  banner.hidden = outdated === 0;
  if (!outdated) return;
  document.getElementById("update-banner-text").textContent = "Agent-Version " + updateStatus?.latest +
    " verfügbar – " + outdated + (outdated === 1 ? " Host kann" : " Hosts können") + " aktualisiert werden.";
}

// Übersicht unter „Einstellungen“.
function renderUpdatePanel(hosts) {
  const st = updateStatus;
  const info = document.getElementById("update-info");
  info.replaceChildren();
  const add = (k, v, cls) => info.append(el("dt", k), el("dd", v || "–", cls));
  add("Server", st?.server_version);
  if (st?.tls_not_after) {
    const soon = st.tls_not_after - Date.now() / 1000 < 30 * 86400;
    add("Zertifikat gültig bis", fmtDateTime(st.tls_not_after) + (soon ? " – bald erneuern!" : ""), soon ? "error" : "");
  }
  if (!st?.enabled) {
    add("Status", st?.message || "abgeschaltet", "muted");
  } else {
    add("Quelle", "github.com/" + st.repo);
    add("Neueste Version", st.latest || "noch keine");
    add("Zuletzt geprüft", st.checked_at ? fmtDateTime(st.checked_at) : "noch nie");
    if (st.error) add("Fehler", st.error, "error");
    const outdated = hosts.filter(h => h.update_available).length;
    const capable = hosts.filter(h => (h.capabilities || []).includes("update")).length;
    add("Hosts", outdated + " veraltet, " + capable + " von " + hosts.length + " können sich selbst aktualisieren");
  }
  document.getElementById("update-check").hidden = !st?.enabled;
  document.getElementById("update-all").hidden = !hosts.some(h => h.update_available);
}

async function checkUpdates() {
  const out = document.getElementById("update-result");
  out.textContent = "Prüfe …";
  try {
    const resp = await fetch("api/update/check", { method: "POST" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    updateStatus = await resp.json();
    out.textContent = updateStatus.error ? "" : "✓ Geprüft";
    await refresh();
  } catch (e) {
    out.textContent = "Fehlgeschlagen: " + e.message;
  }
}

async function updateAll() {
  if (!confirm("Alle veralteten Agenten aktualisieren? Jeder lädt das signierte Release, prüft es und startet neu.")) return;
  try {
    const resp = await fetch("api/update/all", { method: "POST" });
    if (resp.status === 401) return toLogin();
    if (!resp.ok) throw new Error((await resp.text()).trim() || "HTTP " + resp.status);
    const r = await resp.json();
    document.getElementById("update-result").textContent = r.started + " Update(s) gestartet";
    await refresh();
  } catch (e) {
    alert("Updates nicht gestartet: " + e.message);
  }
}

document.getElementById("update-check").addEventListener("click", checkUpdates);
document.getElementById("update-all").addEventListener("click", updateAll);
document.getElementById("update-banner-all").addEventListener("click", updateAll);
