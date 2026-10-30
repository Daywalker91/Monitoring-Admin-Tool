// Liniendiagramme als SVG, ohne Bibliothek.
// - eine y-Achse pro Diagramm (mehrere Größen → mehrere Diagramme)
// - Lücken, wo Messpunkte fehlen (Host offline), statt durchgezogener Linien
// - Fadenkreuz + Tooltip mit allen Serien, auch per Tastatur (Pfeiltasten)
// Texte werden nur per textContent gesetzt.
"use strict";

const Charts = (() => {
  const SVG = "http://www.w3.org/2000/svg";
  const HEIGHT = 150;
  const M = { top: 10, right: 10, bottom: 22, left: 48 };
  const MIN_TICK_PX = 72;

  const TIME_STEPS = [300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800];

  function svgEl(tag, attrs, cls) {
    const e = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(attrs || {})) e.setAttribute(k, v);
    if (cls) e.setAttribute("class", cls);
    return e;
  }

  function div(cls, text) {
    const e = document.createElement("div");
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }

  // Runde Obergrenze und Tick-Abstand für eine Werteachse (ca. 3–4 Ticks).
  function niceScale(max) {
    if (!(max > 0)) return { max: 1, step: 0.25 };
    const raw = max / 3;
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const step = [1, 2, 2.5, 5, 10].map(f => f * mag).find(s => s >= raw);
    return { max: Math.ceil(max / step) * step, step };
  }

  // Byte-Basis 1024 braucht eigene Stufen, damit Ticks auf runden KB/MB-Werten liegen.
  function niceBytesScale(max) {
    if (!(max > 0)) return { max: 1024, step: 256 };
    const unit = Math.pow(1024, Math.max(0, Math.floor(Math.log(max) / Math.log(1024))));
    const s = niceScale(max / unit);
    return { max: s.max * unit, step: s.step * unit };
  }

  function timeLabel(ts, step) {
    const d = new Date(ts * 1000);
    if (step >= 86400) return d.toLocaleDateString("de-DE", { weekday: "short", day: "numeric" });
    return d.toLocaleTimeString("de-DE", { hour: "2-digit", minute: "2-digit" });
  }

  function tooltipTime(ts, rangeSec) {
    const d = new Date(ts * 1000);
    const opts = { hour: "2-digit", minute: "2-digit" };
    if (rangeSec > 86400) Object.assign(opts, { weekday: "short", day: "numeric", month: "numeric" });
    return d.toLocaleString("de-DE", opts);
  }

  // Ticks auf runden Ortszeit-Werten (z. B. volle Stunden, Mitternacht).
  function timeTicks(start, end, width) {
    const span = end - start;
    const maxTicks = Math.max(2, Math.floor(width / MIN_TICK_PX));
    const step = TIME_STEPS.find(s => span / s <= maxTicks) || TIME_STEPS[TIME_STEPS.length - 1];
    const off = -new Date(start * 1000).getTimezoneOffset() * 60;
    const ticks = [];
    for (let t = Math.ceil((start + off) / step) * step - off; t <= end; t += step) ticks.push(t);
    return { ticks, step };
  }

  // Index des Punkts mit dem nächstgelegenen Zeitstempel (binäre Suche).
  function nearest(points, ts) {
    let lo = 0, hi = points.length - 1;
    while (hi - lo > 1) {
      const mid = (lo + hi) >> 1;
      if (points[mid].ts < ts) lo = mid; else hi = mid;
    }
    return Math.abs(points[lo].ts - ts) <= Math.abs(points[hi].ts - ts) ? lo : hi;
  }

  /**
   * Erzeugt ein Liniendiagramm.
   * opts: {
   *   title, series: [{ key, name, cls }], format(v), scale: "pct" | "bytes",
   *   area: bool (Fläche unter einer einzelnen Serie)
   * }
   * Rückgabe: { el, update(points, startTs, endTs, gapSec) }
   */
  function lineChart(opts) {
    const fig = document.createElement("figure");
    fig.className = "chart";

    const head = div("chart-head");
    head.append(div("chart-title", opts.title));
    // Legende nur bei mehreren Serien; bei einer nennt der Titel die Serie.
    if (opts.series.length > 1) {
      const legend = div("chart-legend");
      for (const s of opts.series) {
        const item = div("legend-item");
        item.append(div("line-key " + s.cls), document.createTextNode(s.name));
        legend.append(item);
      }
      head.append(legend);
    }
    fig.append(head);

    const plot = div("chart-plot");
    const svg = svgEl("svg", { height: HEIGHT, role: "img", tabindex: "0" });
    svg.setAttribute("aria-label", opts.title + " – Verlauf, Pfeiltasten zeigen einzelne Werte");
    const tip = div("chart-tip");
    tip.hidden = true;
    plot.append(svg, tip);
    fig.append(plot);

    const empty = div("chart-empty", "Keine Daten im Zeitraum");
    empty.hidden = true;
    plot.append(empty);

    let state = null; // { points, start, end, gap }
    let drawnWidth = 0;
    let hoverIdx = -1;
    let geom = null;

    function draw() {
      const width = plot.clientWidth;
      if (!state || width === 0) return;
      drawnWidth = width;
      svg.setAttribute("width", width);
      svg.setAttribute("viewBox", `0 0 ${width} ${HEIGHT}`);
      svg.replaceChildren();

      const { points, start, end, gap } = state;
      const iw = width - M.left - M.right;
      const ih = HEIGHT - M.top - M.bottom;

      let maxVal = 0;
      for (const p of points) for (const s of opts.series) maxVal = Math.max(maxVal, p[s.key] || 0);
      const sc = opts.scale === "pct" ? { max: 100, step: 25 }
        : opts.scale === "bytes" ? niceBytesScale(maxVal) : niceScale(maxVal);

      const x = ts => M.left + ((ts - start) / (end - start)) * iw;
      const y = v => M.top + ih - (Math.min(v, sc.max) / sc.max) * ih;
      geom = { x, y };

      // Raster und Achsen (Haarlinien, zurückhaltend)
      const grid = svgEl("g", {}, "grid");
      let lastLabel = null;
      for (let v = 0; v <= sc.max + 1e-9; v += sc.step) {
        const yy = Math.round(y(v)) + 0.5;
        grid.append(svgEl("line", { x1: M.left, x2: M.left + iw, y1: yy, y2: yy }));
        // Ohne Daten nur die Nulllinie beschriften; gerundet gleiche Werte nicht doppelt.
        const label = opts.format(v, true);
        if (label === lastLabel || (maxVal === 0 && v > 0)) continue;
        lastLabel = label;
        const t = svgEl("text", { x: M.left - 6, y: yy, "dominant-baseline": "middle", "text-anchor": "end" }, "axis-label");
        t.textContent = label;
        grid.append(t);
      }
      const { ticks, step } = timeTicks(start, end, iw);
      for (const ts of ticks) {
        const t = svgEl("text", { x: x(ts), y: HEIGHT - 6, "text-anchor": "middle" }, "axis-label");
        t.textContent = timeLabel(ts, step);
        grid.append(t);
      }
      svg.append(grid);

      // Linien in Segmenten; eine Lücke > gap trennt die Segmente.
      for (const s of opts.series) {
        const segments = [];
        let cur = [];
        let prevTs = null;
        for (const p of points) {
          // Fehlender Wert (z. B. Temperatur ohne Sensor) unterbricht die Linie.
          if (p[s.key] == null) {
            if (cur.length) segments.push(cur);
            cur = [];
            prevTs = null;
            continue;
          }
          if (prevTs !== null && p.ts - prevTs > gap) { segments.push(cur); cur = []; }
          cur.push(p);
          prevTs = p.ts;
        }
        if (cur.length) segments.push(cur);

        for (const seg of segments) {
          if (seg.length === 1) {
            svg.append(svgEl("circle", { cx: x(seg[0].ts), cy: y(seg[0][s.key]), r: 4 }, "dot " + s.cls));
            continue;
          }
          const d = seg.map((p, i) => (i ? "L" : "M") + x(p.ts).toFixed(1) + "," + y(p[s.key]).toFixed(1)).join("");
          if (opts.area && opts.series.length === 1) {
            const base = y(0).toFixed(1);
            const a = d + `L${x(seg[seg.length - 1].ts).toFixed(1)},${base}L${x(seg[0].ts).toFixed(1)},${base}Z`;
            svg.append(svgEl("path", { d: a }, "area " + s.cls));
          }
          svg.append(svgEl("path", { d }, "line " + s.cls));
        }
      }

      // Hover-Ebene: Fadenkreuz und Punkte, über die volle Plotfläche ansprechbar.
      const hover = svgEl("g", { visibility: "hidden" }, "hover");
      hover.append(svgEl("line", { y1: M.top, y2: M.top + ih }, "crosshair"));
      for (const s of opts.series) hover.append(svgEl("circle", { r: 4 }, "dot " + s.cls));
      svg.append(hover);
      svg.append(svgEl("rect", { x: M.left, y: M.top, width: iw, height: ih, fill: "transparent" }, "hit"));

      if (hoverIdx >= 0) showHover(Math.min(hoverIdx, points.length - 1));
    }

    function showHover(idx) {
      if (!state || !state.points.length || !geom) return;
      hoverIdx = idx;
      const p = state.points[idx];
      const hover = svg.querySelector(".hover");
      const px = geom.x(p.ts);
      const cross = hover.querySelector(".crosshair");
      cross.setAttribute("x1", px);
      cross.setAttribute("x2", px);
      hover.querySelectorAll(".dot").forEach((dot, i) => {
        dot.setAttribute("cx", px);
        const v = p[opts.series[i].key];
        dot.setAttribute("cy", geom.y(v ?? 0));
        dot.setAttribute("visibility", v == null ? "hidden" : "visible");
      });
      hover.setAttribute("visibility", "visible");

      // Tooltip: Wert zuerst (betont), Serienname danach.
      tip.replaceChildren(div("tip-time", tooltipTime(p.ts, state.end - state.start)));
      for (const s of opts.series) {
        const row = div("tip-row");
        const val = document.createElement("strong");
        val.textContent = opts.format(p[s.key]);
        row.append(div("line-key " + s.cls), val, document.createTextNode(" " + s.name));
        tip.append(row);
      }
      tip.hidden = false;
      const w = plot.clientWidth;
      const tw = tip.offsetWidth;
      tip.style.left = Math.max(0, Math.min(w - tw, px + 12 > w - tw ? px - tw - 12 : px + 12)) + "px";
      tip.style.top = M.top + "px";
    }

    function hideHover() {
      hoverIdx = -1;
      svg.querySelector(".hover")?.setAttribute("visibility", "hidden");
      tip.hidden = true;
    }

    svg.addEventListener("pointermove", ev => {
      if (!state || !state.points.length || !geom) return;
      const r = svg.getBoundingClientRect();
      const px = ev.clientX - r.left;
      const ts = state.start + ((px - M.left) / (drawnWidth - M.left - M.right)) * (state.end - state.start);
      showHover(nearest(state.points, ts));
    });
    svg.addEventListener("pointerleave", hideHover);
    svg.addEventListener("blur", hideHover);
    svg.addEventListener("keydown", ev => {
      if (!state || !state.points.length) return;
      const n = state.points.length;
      if (ev.key === "ArrowLeft") showHover(hoverIdx < 0 ? n - 1 : Math.max(0, hoverIdx - 1));
      else if (ev.key === "ArrowRight") showHover(hoverIdx < 0 ? n - 1 : Math.min(n - 1, hoverIdx + 1));
      else if (ev.key === "Escape") hideHover();
      else return;
      ev.preventDefault();
    });

    // Neu zeichnen, wenn sich die Breite ändert (Drehen des Handys, Fenstergröße).
    new ResizeObserver(() => {
      if (plot.clientWidth && plot.clientWidth !== drawnWidth) draw();
    }).observe(plot);

    return {
      el: fig,
      update(points, start, end, gap) {
        state = { points, start, end, gap };
        empty.hidden = points.length > 0;
        if (hoverIdx >= points.length) hideHover();
        draw();
      },
    };
  }

  return { lineChart };
})();
