// SMART-Werte der Datenträger in den Host-Details. Nutzt el, fmtBytes, fmtAgo,
// fetchJSON (app.js) erst beim Aufruf. Werte nur per textContent.
"use strict";

const SMART_RELOAD_MS = 60000;
const smartViews = new Map();

function hostSmartSection(h) {
  let v = smartViews.get(h.id);
  if (!v) {
    v = createSmartView(h);
    smartViews.set(h.id, v);
  }
  if (!v.loadedAt || Date.now() - v.loadedAt > SMART_RELOAD_MS) loadSmart(v);
  return v.el;
}

function createSmartView(h) {
  const v = { id: h.id, data: undefined, loadedAt: 0, el: el("section", null, "host-smart") };
  v.el.append(el("h3", "Datenträger (SMART)"));
  v.summary = el("p", "", "muted small");
  v.table = el("table", null, "mini smart-list");
  v.el.append(v.summary, v.table);
  v.el.addEventListener("click", ev => ev.stopPropagation());
  return v;
}

async function loadSmart(v) {
  v.loadedAt = Date.now();
  try {
    v.data = await fetchJSON("api/hosts/" + encodeURIComponent(v.id) + "/smart");
  } catch (e) {
    v.summary.textContent = "SMART: " + e.message;
    return;
  }
  renderSmart(v);
}

function fmtHours(h) {
  if (h == null) return "–";
  return h.toLocaleString("de-DE") + " h" + (h >= 48 ? " (" + Math.round(h / 24).toLocaleString("de-DE") + " d)" : "");
}

// smartWear beschreibt Abnutzung und Sektoren; level: "", "warn" oder "crit".
function smartWear(d) {
  const parts = [];
  let level = "";
  const add = (text, lvl) => {
    parts.push(text);
    if (lvl === "crit" || (lvl === "warn" && !level)) level = lvl;
  };
  if (d.percent_used != null) add(d.percent_used + " % genutzt", d.percent_used >= 90 ? "warn" : "");
  if (d.available_spare != null) add("Reserve " + d.available_spare + " %",
    d.spare_threshold != null && d.available_spare <= d.spare_threshold ? "crit" : "");
  if (d.media_errors) add(d.media_errors + " Medienfehler", "warn");
  if (d.reallocated != null) add(d.reallocated + " neu zugewiesen", d.reallocated > 0 ? "warn" : "");
  if (d.pending) add(d.pending + " ausstehend", "crit");
  if (d.uncorrectable) add(d.uncorrectable + " nicht korrigierbar", "crit");
  if (d.crc_errors) add(d.crc_errors + " CRC-Fehler (Kabel?)", "warn");
  return { text: parts.join(" · ") || "–", level };
}

function smartProblem(d) {
  if (d.passed === false) return "FAILED";
  if (d.critical_warning) return "kritische Warnung";
  if (d.pending) return "ausstehende Sektoren";
  if (d.uncorrectable) return "nicht korrigierbare Sektoren";
  return "";
}

function renderSmart(v) {
  const d = v.data;
  if (d === null) {
    v.summary.textContent = "Noch kein SMART-Stand – der Agent meldet ihn ab Version 1.2.";
    v.table.hidden = true;
    return;
  }
  if (d.error) {
    v.summary.textContent = d.error + ". Linux: „apt install smartmontools“, Windows: smartmontools.org.";
    v.table.hidden = true;
    return;
  }
  const disks = d.disks || [];
  v.summary.textContent = disks.length
    ? disks.length + (disks.length === 1 ? " Datenträger" : " Datenträger") + " · Stand " + fmtAgo(Math.floor(Date.parse(d.time) / 1000))
    : "Keine Datenträger mit SMART gefunden (SD-Karte und eMMC melden meist keine Werte).";
  v.table.hidden = disks.length === 0;
  const head = el("tr");
  for (const [t, c] of [["Gerät", ""], ["Status", ""], ["Temp", "num"], ["Betrieb", "num hide-sm"], ["Abnutzung / Sektoren", "hide-sm"]]) {
    head.append(el("th", t, c));
  }
  const thead = el("thead");
  thead.append(head);
  const tbody = el("tbody");
  for (const x of disks) {
    const tr = el("tr");
    const name = el("td", null, "name");
    name.append(el("span", x.device, "mono"));
    const model = [x.model, x.bytes ? fmtBytes(x.bytes) : ""].filter(Boolean).join(" · ");
    if (model) name.append(el("span", model, "muted small svc-desc"));
    name.title = [x.model, x.serial && "Seriennummer " + x.serial, x.firmware && "Firmware " + x.firmware].filter(Boolean).join("\n");

    const st = el("td", null, "svc-state");
    const problem = smartProblem(x);
    if (problem) st.append(el("span", null, "dot crit"), problem);
    else if (x.passed == null) st.append(el("span", null, "dot"), x.standby ? "schläft" : "unbekannt");
    else st.append(el("span", null, "dot online"), "in Ordnung");
    if (x.standby && x.passed != null) {
      st.append(el("span", " (schläft)", "muted small"));
      st.title = "Platte war im Standby; Werte vom letzten Lauf mit Antwort.";
    }

    const wear = smartWear(x);
    const tdWear = el("td", wear.text, "small hide-sm" + (wear.level ? " lvl-" + wear.level : ""));
    tr.append(name, st, el("td", x.temp_c != null ? x.temp_c.toFixed(0) + " °C" : "–", "num"),
      el("td", fmtHours(x.power_on_hours), "num hide-sm"), tdWear);
    tbody.append(tr);
  }
  v.table.replaceChildren(thead, tbody);
}
