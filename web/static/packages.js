// Paket-Updates und Neustart in den Host-Details sowie Hinweise in der
// Host-Liste. Nutzt el, fmtAgo, fetchJSON, hostName (app.js) und startJob
// (jobs.js) erst beim Aufruf. Werte nur per textContent.
"use strict";

const pkgViews = new Map();

// Hinweise neben dem Hostnamen: offene Updates und ausstehender Neustart.
function hostPackagePills(h) {
  const u = h.updates;
  if (!u) return [];
  const pills = [];
  if (u.upgradable > 0) {
    const p = el("span", u.upgradable + (u.upgradable === 1 ? " Update" : " Updates"),
      "pill host-alerts " + (u.security > 0 ? "warn" : "info"));
    if (u.security > 0) p.title = u.security + " davon Sicherheitsupdates";
    pills.push(p);
  }
  if (u.reboot_required) pills.push(el("span", "↻ Neustart nötig", "pill warn host-alerts"));
  return pills;
}

function hostPackageSection(h) {
  let v = pkgViews.get(h.id);
  if (!v) {
    v = createPackageView(h);
    pkgViews.set(h.id, v);
  }
  v.host = h;
  if (h.updates && h.updates.at !== v.loadedAt) loadPackages(v);
  renderPackageView(v);
  return v.el;
}

function createPackageView(h) {
  const v = { id: h.id, host: h, data: null, loadedAt: null, el: el("section", null, "host-packages") };
  v.el.append(el("h3", "Updates und Neustart"));
  v.summary = el("p", "", "small");
  v.reboot = el("p", "", "small warn-text");
  v.error = el("p", "", "small error-text");
  v.list = el("details", null, "pkg-details");
  v.listTitle = el("summary", "");
  v.table = el("table", null, "mini pkg-list");
  v.list.append(v.listTitle, v.table);
  v.buttons = el("div", null, "job-buttons");
  v.k3sHint = el("p", "", "muted small");
  v.status = el("p", "", "small");
  v.status.setAttribute("role", "status");
  v.el.append(v.summary, v.reboot, v.error, v.list, v.buttons, v.k3sHint, v.status);
  v.el.addEventListener("click", ev => ev.stopPropagation());
  return v;
}

async function loadPackages(v) {
  v.loadedAt = v.host.updates?.at ?? null;
  try {
    v.data = await fetchJSON("api/hosts/" + encodeURIComponent(v.id) + "/updates");
  } catch (e) {
    v.status.className = "small error-text";
    v.status.textContent = "Updates: " + e.message;
    return;
  }
  renderPackageView(v);
}

function renderPackageView(v) {
  const h = v.host;
  const u = h.updates;
  if (!u) {
    v.summary.textContent = h.online ? "Noch kein Update-Stand – der Agent meldet ihn ab Version 1.2." : "Kein Update-Stand.";
    for (const e of [v.reboot, v.error, v.list]) e.hidden = true;
  } else {
    if (!u.manager) v.summary.textContent = "Keine unterstützte Paketverwaltung (nur Neustart-Hinweis) · Stand " + fmtAgo(u.at);
    else if (u.upgradable === 0) v.summary.textContent = "✓ Alle Pakete aktuell · Stand " + fmtAgo(u.at);
    else v.summary.textContent = u.upgradable + (u.upgradable === 1 ? " Update" : " Updates") +
      (u.security ? ", davon " + u.security + " Sicherheit" : "") + " · Stand " + fmtAgo(u.at);
    v.reboot.textContent = "↻ Das System verlangt einen Neustart (z. B. nach einem Kernel-Update).";
    v.reboot.hidden = !u.reboot_required;
    v.error.textContent = u.error || "";
    v.error.hidden = !u.error;
    renderPackageList(v);
  }

  const caps = h.capabilities || [];
  const actions = [];
  if (caps.includes("packages")) {
    actions.push(["packages.refresh", "Paketlisten holen", "Paketlisten auf „" + hostName(h) + "“ aktualisieren (apt-get update)?"]);
    if (u?.upgradable > 0) {
      actions.push(["packages.upgrade", "Updates installieren (" + u.upgradable + ")",
        u.upgradable + " Updates auf „" + hostName(h) + "“ installieren (apt-get upgrade)?\n" +
        "Geänderte Konfigurationsdateien bleiben erhalten." + k3sWarning(h)]);
    }
  }
  if (caps.includes("reboot")) {
    actions.push(["system.reboot", "Neu starten", "„" + hostName(h) + "“ jetzt neu starten?\n" +
      "Der Host ist danach einige Minuten nicht erreichbar." + k3sWarning(h)]);
  }
  v.buttons.replaceChildren(...actions.map(([action, label, question]) => {
    const b = el("button", label, action === "system.reboot" ? "danger" : "secondary");
    b.type = "button";
    b.disabled = !h.online;
    b.addEventListener("click", () => packageJob(v, action, label, question, b));
    return b;
  }));
  v.buttons.hidden = actions.length === 0;
  v.k3sHint.textContent = h.k8s_node && actions.length
    ? "k3s-Node: vor Upgrade oder Neustart am besten erst „kubectl drain " + h.k8s_node.name +
      " --ignore-daemonsets“, danach „kubectl uncordon“. Immer nur einen Node gleichzeitig."
    : "";
  v.k3sHint.hidden = !v.k3sHint.textContent;
}

function k3sWarning(h) {
  return h.k8s_node ? "\n\nAchtung: k3s-Node „" + h.k8s_node.name + "“ – laufende Pods werden unterbrochen." : "";
}

function renderPackageList(v) {
  const pkgs = v.data?.packages || [];
  v.list.hidden = pkgs.length === 0;
  if (!pkgs.length) return;
  const more = (v.data.upgradable || 0) - pkgs.length;
  v.listTitle.textContent = "Pakete anzeigen" + (more > 0 ? " (erste " + pkgs.length + ")" : "");
  const head = el("tr");
  for (const [t, c] of [["Paket", ""], ["installiert", "hide-sm"], ["neu", ""]]) head.append(el("th", t, c));
  const thead = el("thead");
  thead.append(head);
  const tbody = el("tbody");
  for (const p of pkgs) {
    const tr = el("tr");
    const name = el("td", null, "name");
    name.append(el("span", p.name, "mono"));
    if (p.security) name.append(el("span", "Sicherheit", "pill warn pkg-sec"));
    tr.append(name, el("td", p.from || "neu", "mono hide-sm"), el("td", p.to, "mono"));
    tbody.append(tr);
  }
  v.table.replaceChildren(thead, tbody);
}

async function packageJob(v, action, label, question, btn) {
  if (!confirm(question)) return;
  btn.disabled = true;
  v.status.className = "small muted";
  v.status.textContent = label + " … (Ergebnis unter „Aktionen“)";
  try {
    await startJob(v.id, action);
  } catch (e) {
    v.status.className = "small error-text";
    v.status.textContent = e.message;
    btn.disabled = false;
  }
}
