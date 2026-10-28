// Host-Liste: lädt /api/hosts regelmäßig und rendert die Tabelle.
// Sicherheit: Werte werden ausschließlich per textContent gesetzt, nie per innerHTML.
"use strict";

const REFRESH_MS = 5000;
const openHosts = new Set(); // IDs der aufgeklappten Hosts

function el(tag, text, cls) {
  const e = document.createElement(tag);
  if (text !== undefined && text !== null) e.textContent = text;
  if (cls) e.className = cls;
  return e;
}

function fmtBytes(n) {
  if (!n) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return n.toFixed(n < 10 && i > 0 ? 1 : 0) + " " + units[i];
}

function fmtRate(n) { return fmtBytes(n) + "/s"; }
function fmtPct(n) { return (n ?? 0).toFixed(1) + " %"; }

function fmtAgo(unix) {
  const s = Math.max(0, Math.round(Date.now() / 1000 - unix));
  if (s < 60) return "vor " + s + " s";
  if (s < 3600) return "vor " + Math.round(s / 60) + " min";
  if (s < 86400) return "vor " + Math.round(s / 3600) + " h";
  return "vor " + Math.round(s / 86400) + " d";
}

function fmtUptime(bootUnix) {
  if (!bootUnix) return "–";
  const s = Date.now() / 1000 - bootUnix;
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600);
  return d + " d " + h + " h";
}

// Prozentzelle mit Warnstufe ab 80 % bzw. 90 %.
function pctCell(v) {
  const td = el("td", v === undefined ? "–" : fmtPct(v), "num");
  if (v >= 90) td.classList.add("lvl-crit");
  else if (v >= 80) td.classList.add("lvl-warn");
  return td;
}

function hostRow(h) {
  const m = h.metrics || {};
  const tr = el("tr", null, "host");
  tr.dataset.id = h.id;
  if (openHosts.has(h.id)) tr.classList.add("open");

  const status = el("td");
  status.append(el("span", null, "dot" + (h.online ? " online" : "")), h.online ? "online" : "offline");
  tr.append(status);

  tr.append(el("td", h.hostname));
  tr.append(el("td", [h.platform || h.os, h.platform_version, h.arch].filter(Boolean).join(" · ")));

  if (!h.metrics) {
    for (let i = 0; i < 6; i++) tr.append(el("td", "–", "num"));
  } else {
    tr.append(pctCell(m.cpu?.total_pct));
    tr.append(el("td", m.cpu ? m.cpu.load1.toFixed(2) : "–", "num"));
    tr.append(pctCell(m.mem?.used_pct));
    tr.append(m.swap?.total ? pctCell(m.swap.used_pct) : el("td", "–", "num"));
    let rx = 0, tx = 0;
    for (const n of m.net || []) { rx += n.rx_bps; tx += n.tx_bps; }
    tr.append(el("td", fmtRate(rx) + " / " + fmtRate(tx), "num"));
    const diskMax = Math.max(0, ...(m.disks || []).map(d => d.used_pct));
    tr.append(pctCell(diskMax));
  }
  tr.append(el("td", fmtAgo(h.last_seen)));

  tr.addEventListener("click", () => {
    if (openHosts.has(h.id)) openHosts.delete(h.id); else openHosts.add(h.id);
    render(lastHosts);
  });
  return tr;
}

function fillTable(tbody, rows) {
  for (const cells of rows) {
    const tr = el("tr");
    for (const [text, cls] of cells) tr.append(el("td", text, cls));
    tbody.append(tr);
  }
}

function procRows(list) {
  return (list || []).map(p => [
    [String(p.pid)], [p.name, "name"], [p.cpu_pct.toFixed(1) + " %", "num"], [fmtBytes(p.rss), "num"],
  ]);
}

function detailRow(h) {
  const tpl = document.getElementById("detail-tpl").content.cloneNode(true);
  const tr = tpl.querySelector("tr");
  const m = h.metrics || {};
  const p = h.processes || {};

  const cores = tr.querySelector(".cores");
  (m.cpu?.per_core || []).forEach((v, i) => {
    const c = el("div", "#" + i + " " + v.toFixed(0) + " %", "core");
    const bar = el("div", null, "bar");
    const fill = el("span");
    fill.style.width = Math.min(100, v) + "%";
    bar.append(fill);
    c.append(bar);
    cores.append(c);
  });

  fillTable(tr.querySelector(".disks tbody"), (m.disks || []).map(d => [
    [d.mount, "name"], [d.fstype], [fmtPct(d.used_pct), "num"], [fmtBytes(d.total), "num"],
  ]));
  fillTable(tr.querySelector(".ifaces tbody"), (m.net || []).map(n => [
    [n.name, "name"], [fmtRate(n.rx_bps), "num"], [fmtRate(n.tx_bps), "num"],
    [n.rx_pps.toFixed(0) + " / " + n.tx_pps.toFixed(0), "num"],
  ]));
  fillTable(tr.querySelector(".procs-cpu tbody"), procRows(p.top_cpu));
  fillTable(tr.querySelector(".procs-mem tbody"), procRows(p.top_mem));

  const info = tr.querySelector(".info");
  const add = (k, v) => { info.append(el("dt", k), el("dd", v || "–")); };
  add("Agent-ID", h.id);
  add("Version", h.agent_version);
  add("Kernel", h.kernel_version);
  add("Kerne", String(h.cpu_cores));
  add("RAM", m.mem ? fmtBytes(m.mem.used) + " / " + fmtBytes(m.mem.total) : "");
  add("Uptime", fmtUptime(h.boot_time));
  add("Load 1/5/15", m.cpu ? [m.cpu.load1, m.cpu.load5, m.cpu.load15].map(x => x.toFixed(2)).join(" / ") : "");
  add("Fähigkeiten", (h.capabilities || []).join(", "));
  add("Adresse", h.remote_addr);
  add("Intervall", h.interval_sec + " s");
  return tr;
}

let lastHosts = [];

function render(hosts) {
  lastHosts = hosts;
  const tbody = document.querySelector("#hosts tbody");
  const rows = [];
  for (const h of hosts) {
    rows.push(hostRow(h));
    if (openHosts.has(h.id)) rows.push(detailRow(h));
  }
  tbody.replaceChildren(...rows);

  const online = hosts.filter(h => h.online).length;
  document.getElementById("summary").textContent = hosts.length + " Hosts, " + online + " online";
  document.getElementById("empty").hidden = hosts.length > 0;
}

async function refresh() {
  const errBox = document.getElementById("error");
  try {
    const resp = await fetch("api/hosts", { cache: "no-store" });
    if (!resp.ok) throw new Error("HTTP " + resp.status);
    render(await resp.json());
    errBox.hidden = true;
    document.getElementById("updated").textContent = "aktualisiert " + new Date().toLocaleTimeString("de-DE");
  } catch (e) {
    errBox.textContent = "Server nicht erreichbar: " + e.message;
    errBox.hidden = false;
  }
}

refresh();
setInterval(refresh, REFRESH_MS);
