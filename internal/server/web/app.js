"use strict";
// S1 Panel widget editor (PART B): browser-side theme draft over the real
// panel state. Grid + snap-to-grid for orienting widgets; numbers come from
// live system telemetry via /api/poll; "Примерить" POSTs the draft to
// /api/preview which redraws the PHYSICAL panel and returns its PNG.

const $ = (s) => document.querySelector(s);

const state = {
  sensors: [],
  readings: {},
  draft: null,          // theme JSON (config.Theme), deep clone, mutated freely
  panelScreen: 0,       // screen index currently shown on the real panel
  screenIdx: 0,         // tab being edited
  selected: null,       // widget id on the current tab
  view: { zoom: 2, grid: true, snap: true, step: 10 },
  drag: null,
};

const cv = $("#cv");
const ctx = cv.getContext("2d");

// ---------- geometry helpers ----------

function themeSize() {
  return state.draft.orientation === "portrait" ? [170, 320] : [320, 170];
}
function curScreen() { return state.draft.screens[state.screenIdx]; }
function curWidget() {
  return curScreen().widgets.find((w) => w.id === state.selected) || null;
}
function snapVal(v) {
  const st = state.view.step;
  return state.view.snap && st > 0 ? Math.round(v / st) * st : Math.round(v);
}
function clampRect(r) {
  const [W, H] = themeSize();
  r.width = Math.max(2, Math.min(Math.round(r.width), W));
  r.height = Math.max(2, Math.min(Math.round(r.height), H));
  r.x = Math.max(0, Math.min(Math.round(r.x), W - r.width));
  r.y = Math.max(0, Math.min(Math.round(r.y), H - r.height));
}
function nextWidgetId() {
  let m = 0;
  for (const sc of state.draft.screens) for (const w of sc.widgets) m = Math.max(m, w.id);
  return m + 1;
}

// ---------- telemetry formatting (mirrors render.FormatTokens) ----------

function fmt(w) {
  if (!w.format && w.value) return w.value;
  const r = state.readings[w.sensor];
  if (!r) return "N/A";
  if (!w.format) {
    return r.text && r.text.length ? r.text[0] : String(Math.round(r.value));
  }
  return w.format.replace(/\{([0-9a-zA-Z]+)\}/g, (_, t) => {
    if (t === "v") return String(Math.round(r.value));
    if (t === "v1") return r.value.toFixed(1);
    const n = parseInt(t, 10);
    if (!Number.isNaN(n) && n >= 0 && r.text && n < r.text.length) return r.text[n];
    return "";
  });
}
function frac(w) {
  const r = state.readings[w.sensor];
  if (!r) return 0;
  let hi = w.max || r.max || 0;
  if (hi <= 0 && r.history && r.history.length) hi = Math.max(...r.history);
  if (hi <= 0) return 0;
  return Math.min(1, Math.max(0, r.value / hi));
}
function historyTail(w) {
  const r = state.readings[w.sensor];
  if (!r || !r.history) return [];
  const n = Math.max(1, w.points || 30);
  return r.history.slice(-n);
}
function seriesMax(w, pts) {
  let hi = w.max || 0;
  if (hi <= 0) {
    const r = state.readings[w.sensor];
    hi = (r && r.max) || 0;
  }
  if (hi <= 0) for (const v of pts) hi = Math.max(hi, v);
  return hi || 1;
}

// ---------- canvas drawing ----------

function draw() {
  if (!state.draft) return;
  const [W, H] = themeSize();
  const z = state.view.zoom;
  cv.width = W * z;
  cv.height = H * z;
  const bg = curScreen().background || state.draft.background || "#000000";
  ctx.fillStyle = bg;
  ctx.fillRect(0, 0, cv.width, cv.height);

  if (state.view.grid) drawGrid(W, H, z);

  for (const w of curScreen().widgets) drawWidget(w, z, bg);
  const sel = curWidget();
  if (sel) drawSelection(sel, z);
  schedulePreview();
}

// Живое превью «как видит экран»: драфт рендерится на сервере (POST
// /api/render) БЕЗ касания панели - картинка в футере обновляется сама,
// не дожидаясь «Применить» или «Примерить». Дебаунс 500 мс; запоздалые
// ответы старше последнего запроса отбрасываются (seq).
let previewSeq = 0, previewTimer = null;
function schedulePreview() {
  if (!state.draft) return;
  clearTimeout(previewTimer);
  previewTimer = setTimeout(async () => {
    const seq = ++previewSeq;
    try {
      const r = await fetch(`/api/render?screen=${state.screenIdx}`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(state.draft),
      });
      if (r.status !== 200 || seq !== previewSeq) return;
      const old = $("#previewImg").src;
      $("#previewImg").src = URL.createObjectURL(await r.blob());
      if (old.startsWith("blob:")) URL.revokeObjectURL(old);
    } catch (_) { /* панель недоступна - следующая правка повторит */ }
  }, 500);
}

// drawGrid paints the coordinate grid the widgets are oriented against:
// minor lines every step px, brighter majors every 5 steps, numeric labels
// on majors at zoom >= 2 (values in logical panel pixels).
function drawGrid(W, H, z) {
  const step = state.view.step || 10;
  ctx.lineWidth = 1;
  for (let x = 0; x <= W; x += step) {
    const major = Math.round(x / step) % 5 === 0;
    ctx.strokeStyle = major ? "rgba(140,200,255,0.35)" : "rgba(140,200,255,0.13)";
    const px = Math.round(x * z) + 0.5;
    ctx.beginPath(); ctx.moveTo(px, 0); ctx.lineTo(px, H * z); ctx.stroke();
  }
  for (let y = 0; y <= H; y += step) {
    const major = Math.round(y / step) % 5 === 0;
    ctx.strokeStyle = major ? "rgba(140,200,255,0.35)" : "rgba(140,200,255,0.13)";
    const py = Math.round(y * z) + 0.5;
    ctx.beginPath(); ctx.moveTo(0, py); ctx.lineTo(W * z, py); ctx.stroke();
  }
  if (z >= 2) {
    ctx.fillStyle = "rgba(140,200,255,0.7)";
    ctx.font = "10px monospace";
    for (let x = step * 5; x < W; x += step * 5) ctx.fillText(String(x), x * z + 2, 11);
    for (let y = step * 5; y < H; y += step * 5) ctx.fillText(String(y), 2, y * z - 2);
  }
}

function drawWidget(w, z, bg) {
  const r = w.rect;
  const x = r.x * z, y = r.y * z, ww = r.width * z, hh = r.height * z;

  // placement aid: every widget shows its bounding box
  ctx.strokeStyle = "rgba(255,255,255,0.25)";
  ctx.setLineDash([3, 3]);
  ctx.strokeRect(x + 0.5, y + 0.5, ww, hh);
  ctx.setLineDash([]);

  // fill is the bar fill colour on the device (Go drawBarChart), so it
  // must not flood the bar_chart preview; only text gets a rect backdrop.
  if (w.fill && w.type === "text") {
    ctx.fillStyle = w.fill;
    ctx.fillRect(x, y, ww, hh);
  }
  if (w.outline && (w.type === "text" || w.type === "bar_chart")) {
    ctx.strokeStyle = w.outline;
    ctx.strokeRect(x + 0.5, y + 0.5, ww, hh);
  }

  switch (w.type) {
    case "text": {
      // Mirrors the Go renderer (internal/render/widgets.go drawTextWidget):
      // size is true panel pixels, the first baseline sits at rect.top +
      // ascent (TOP anchoring, NOT vertically centred), lines advance by the
      // font's own line height, words wrap at the rect width, and everything
      // outside the rect is CLIPPED (the panel rasterises each text widget
      // on a rect-sized sub-canvas, same clip, like the original text.js).
      const bold = !!(w.font && w.font.bold);
      const size = (w.font && w.font.size ? w.font.size : 20) * z;
      ctx.font = `${bold ? "bold " : ""}${size}px "Latin Modern Sans", sans-serif`;
      ctx.fillStyle = w.color || "#ffffff";
      ctx.textBaseline = "alphabetic";
      ctx.textAlign = w.align === "center" ? "center" : w.align === "right" ? "right" : "left";
      // Latin Modern hhea metrics / unitsPerEm (1000) - the same TTF the
      // device rasterises, served from web/fonts/ via @font-face.
      const m = bold ? { asc: 1.134, lh: 1.431 } : { asc: 1.154, lh: 1.463 };
      const tx = w.align === "center" ? x + ww / 2 : w.align === "right" ? x + ww : x;
      const lines = [];
      for (const raw of fmt(w).split("\n")) {
        let cur = "";
        for (const word of raw.split(" ")) {
          const cand = cur ? cur + " " + word : word;
          if (cur && ctx.measureText(cand).width > ww) { lines.push(cur); cur = word; }
          else cur = cand;
        }
        lines.push(cur);
      }
      ctx.save();
      ctx.beginPath();
      ctx.rect(x, y, ww, hh);
      ctx.clip();
      lines.forEach((ln, i) => ctx.fillText(ln, tx, y + size * m.asc + i * size * m.lh));
      ctx.restore();
      break;
    }
    case "doughnut":
      drawRing(w, x, y, ww, hh, z, bg);
      break;
    case "doughnut_chart": {
      const d = Math.min(ww, hh);
      drawRing(w, x, y + (hh - d) / 2, d, d, z, bg);
      if (ww > d + 8 * z) drawSpark(w, x + d + 4 * z, y, ww - d - 4 * z, hh);
      break;
    }
    case "line_chart":
      drawSpark(w, x, y, ww, hh);
      break;
    case "bar_chart": {
      const col = w.fill || w.color || "#00ff00";
      if (!w.points || w.points <= 1) {
        ctx.fillStyle = col;
        ctx.fillRect(x, y, ww * frac(w), hh);
      } else {
        const pts = historyTail(w);
        const hi = seriesMax(w, pts);
        const bw = (ww - (pts.length - 1)) / Math.max(1, pts.length);
        pts.forEach((v, i) => {
          const bh = (v / hi) * hh;
          ctx.fillStyle = col;
          ctx.fillRect(x + i * (bw + 1), y + hh - bh, Math.max(1, bw), bh);
        });
      }
      break;
    }
    case "custom_bar": {
      // mirrors Go drawCustomBar: grey gradient track, the first value% of
      // the width painted with the green gradient (sensor value 0..100).
      const rd = state.readings[w.sensor];
      const v = Math.min(1, Math.max(0, ((rd && rd.value) || 0) / 100));
      const track = ctx.createLinearGradient(0, y, 0, y + hh);
      track.addColorStop(0, "#404040");
      track.addColorStop(1, "#000000");
      ctx.fillStyle = track;
      ctx.fillRect(x, y, ww, hh);
      const fillG = ctx.createLinearGradient(0, y, 0, y + hh);
      fillG.addColorStop(0, "#90EE90");
      fillG.addColorStop(1, "#006400");
      ctx.fillStyle = fillG;
      ctx.fillRect(x, y, ww * v, hh);
      break;
    }
  }
}

function drawRing(w, x, y, ww, hh, z, bg) {
  const cx = x + ww / 2, cy = y + hh / 2, r = Math.min(ww, hh) / 2;
  const thick = (w.thickness || 8) * z;
  ctx.fillStyle = w.free || bg;
  ctx.beginPath(); ctx.arc(cx, cy, r, 0, Math.PI * 2); ctx.fill();
  const f = frac(w);
  if (f > 0) {
    ctx.fillStyle = w.used || w.color || "#00ff00";
    ctx.beginPath();
    ctx.moveTo(cx, cy);
    ctx.arc(cx, cy, r, -Math.PI / 2, -Math.PI / 2 + Math.PI * 2 * f);
    ctx.closePath();
    ctx.fill();
  }
  ctx.fillStyle = bg;
  ctx.beginPath(); ctx.arc(cx, cy, Math.max(0, r - thick), 0, Math.PI * 2); ctx.fill();
}

function drawSpark(w, x, y, ww, hh) {
  const pts = historyTail(w);
  if (pts.length < 2) return;
  const hi = seriesMax(w, pts);
  const dx = ww / (pts.length - 1);
  const py = (v) => y + hh - Math.min(1, v / hi) * hh;
  if (w.area) {
    ctx.fillStyle = w.fill || w.color || "#00ff00";
    ctx.beginPath();
    ctx.moveTo(x, y + hh);
    pts.forEach((v, i) => ctx.lineTo(x + i * dx, py(v)));
    ctx.lineTo(x + (pts.length - 1) * dx, y + hh);
    ctx.closePath();
    ctx.fill();
  }
  ctx.strokeStyle = w.outline || w.color || "#00ff00";
  ctx.lineWidth = Math.max(1, (w.thickness || 1));
  ctx.beginPath();
  pts.forEach((v, i) => (i ? ctx.lineTo(x + i * dx, py(v)) : ctx.moveTo(x, py(v))));
  ctx.stroke();
  ctx.lineWidth = 1;
}

function drawSelection(w, z) {
  const r = w.rect;
  ctx.strokeStyle = "#ffffff";
  ctx.setLineDash([4, 3]);
  ctx.strokeRect(r.x * z + 0.5, r.y * z + 0.5, r.width * z, r.height * z);
  ctx.setLineDash([]);
  ctx.fillStyle = "#2f6fdd";
  for (const [hx, hy] of corners(w)) {
    ctx.fillRect(hx - 4, hy - 4, 8, 8);
  }
}
function corners(w) {
  const z = state.view.zoom, r = w.rect;
  return [
    [r.x * z, r.y * z],
    [(r.x + r.width) * z, r.y * z],
    [r.x * z, (r.y + r.height) * z],
    [(r.x + r.width) * z, (r.y + r.height) * z],
  ];
}

// ---------- pointer interaction: move / resize with grid snapping ----------

function toLogical(ev) {
  const b = cv.getBoundingClientRect();
  return [(ev.clientX - b.left) / state.view.zoom, (ev.clientY - b.top) / state.view.zoom];
}
function setCoords(lx, ly) {
  $("#coords").textContent = `${Math.round(lx)},${Math.round(ly)} px`;
}

cv.addEventListener("pointerdown", (ev) => {
  const [lx, ly] = toLogical(ev);
  setCoords(lx, ly);
  const sel = curWidget();
  if (sel) {
    const tol = 6 / state.view.zoom;
    const c = corners(sel).map(([cx, cy]) => [cx / state.view.zoom, cy / state.view.zoom]);
    const names = ["nw", "ne", "sw", "se"];
    for (let i = 0; i < 4; i++) {
      if (Math.abs(lx - c[i][0]) <= tol && Math.abs(ly - c[i][1]) <= tol) {
        state.drag = { mode: "resize", corner: names[i], orig: { ...sel.rect }, sx: lx, sy: ly };
        cv.setPointerCapture(ev.pointerId);
        return;
      }
    }
  }
  // topmost hit (reverse z order)
  const ws = curScreen().widgets;
  for (let i = ws.length - 1; i >= 0; i--) {
    const r = ws[i].rect;
    if (lx >= r.x && lx <= r.x + r.width && ly >= r.y && ly <= r.y + r.height) {
      state.selected = ws[i].id;
      state.drag = { mode: "move", orig: { ...r }, sx: lx, sy: ly };
      cv.setPointerCapture(ev.pointerId);
      buildProps();
      draw();
      return;
    }
  }
  state.selected = null;
  state.drag = null;
  buildProps();
  draw();
});

cv.addEventListener("pointermove", (ev) => {
  const [lx, ly] = toLogical(ev);
  setCoords(lx, ly);
  if (!state.drag) return;
  const w = curWidget();
  if (!w) return;
  const d = state.drag, dx = lx - d.sx, dy = ly - d.sy;
  const r = w.rect;
  if (d.mode === "move") {
    r.x = snapVal(d.orig.x + dx);
    r.y = snapVal(d.orig.y + dy);
    r.width = d.orig.width;
    r.height = d.orig.height;
  } else {
    let { x, y, width: ww, height: hh } = d.orig;
    if (d.corner === "nw") { x = snapVal(x + dx); ww = d.orig.x + d.orig.width - x; }
    if (d.corner === "ne") { ww = snapVal(x + ww + dx) - x; }
    if (d.corner === "sw") { y = snapVal(y + dy); hh = d.orig.y + d.orig.height - y; }
    if (d.corner === "se") { hh = snapVal(y + hh + dy) - y; }
    if (d.corner === "ne" || d.corner === "se") ww = snapVal(d.orig.x + d.orig.width + dx) - x;
    if (d.corner === "sw" || d.corner === "se") hh = snapVal(d.orig.y + d.orig.height + dy) - y;
    Object.assign(r, { x, y, width: ww, height: hh });
  }
  clampRect(r);
  syncRectInputs();
  draw();
});

cv.addEventListener("pointerup", () => { state.drag = null; });

document.addEventListener("keydown", (ev) => {
  const tag = (ev.target.tagName || "").toLowerCase();
  if (tag === "input" || tag === "select" || tag === "textarea") return;
  const w = curWidget();
  if (!w) return;
  const step = ev.shiftKey ? 1 : (state.view.step || 10);
  let handled = true;
  switch (ev.key) {
    case "ArrowLeft": w.rect.x -= step; break;
    case "ArrowRight": w.rect.x += step; break;
    case "ArrowUp": w.rect.y -= step; break;
    case "ArrowDown": w.rect.y += step; break;
    case "Delete": case "Backspace":
      curScreen().widgets = curScreen().widgets.filter((x) => x.id !== w.id);
      state.selected = null;
      buildProps();
      break;
    default: handled = false;
  }
  if (handled) {
    ev.preventDefault();
    clampRect(w.rect);
    syncRectInputs();
    draw();
  }
});

// ---------- property panel ----------

const props = $("#props");

function row(labelText, el) {
  const l = document.createElement("label");
  l.className = "row";
  const s = document.createElement("span");
  s.textContent = labelText;
  l.append(s, el);
  return l;
}
function txt(v, on) {
  const i = document.createElement("input");
  i.type = "text";
  i.value = v ?? "";
  i.oninput = () => on(i.value);
  return i;
}
function num(v, on, step) {
  const i = document.createElement("input");
  i.type = "number";
  i.value = v ?? 0;
  if (step) i.step = step;
  i.oninput = () => on(parseFloat(i.value) || 0);
  return i;
}
function col(v, on) {
  const i = document.createElement("input");
  i.type = "color";
  i.value = /^#[0-9a-fA-F]{6}$/.test(v || "") ? v : "#000000";
  i.oninput = () => on(i.value);
  return i;
}
function sel(opts, v, on) {
  const s = document.createElement("select");
  for (const [val, lab] of opts) {
    const o = document.createElement("option");
    o.value = val;
    o.textContent = lab;
    if (String(v) === String(val)) o.selected = true;
    s.append(o);
  }
  s.onchange = () => on(s.value);
  return s;
}
function chk(v, on) {
  const i = document.createElement("input");
  i.type = "checkbox";
  i.checked = !!v;
  i.onchange = () => on(i.checked);
  return i;
}

let rectInputs = {};

function buildProps() {
  props.textContent = "";
  const w = curWidget();
  if (!w) {
    const p = document.createElement("p");
    p.className = "hint";
    p.textContent = "Виджет не выбран. Кликните виджет на холсте или добавьте новый. Виджеты ориентируются по сетке (шаг — в линейке сверху); привязка включена.";
    props.append(p);
    rectInputs = {};
    return;
  }
  const h = document.createElement("h3");
  h.textContent = `${w.type} · id ${w.id}`;
  props.append(h);

  rectInputs = {};
  const bind = (key, v) => { rectInputs[key] = v; return v; };
  props.append(row("x", bind("x", num(w.rect.x, (v) => { w.rect.x = v; clampRect(w.rect); draw(); }))));
  props.append(row("y", bind("y", num(w.rect.y, (v) => { w.rect.y = v; clampRect(w.rect); draw(); }))));
  props.append(row("width", bind("w", num(w.rect.width, (v) => { w.rect.width = v; clampRect(w.rect); draw(); }))));
  props.append(row("height", bind("h", num(w.rect.height, (v) => { w.rect.height = v; clampRect(w.rect); draw(); }))));

  const sensorOpts = [["", "— нет —"]].concat(state.sensors.map((s) => [s, s]));
  if (w.type !== "spacer" && w.type !== "none") {
    props.append(row("сенсор", sel(sensorOpts, w.sensor, (v) => { w.sensor = v; draw(); })));
  }

  switch (w.type) {
    case "text":
      props.append(row("текст", txt(w.value, (v) => { w.value = v; draw(); })));
      props.append(row("формат", txt(w.format, (v) => { w.format = v; draw(); })));
      props.append(row("цвет", col(w.color, (v) => { w.color = v; draw(); })));
      props.append(row("выравнивание", sel([["left", "слева"], ["center", "центр"], ["right", "справа"]], w.align, (v) => { w.align = v; draw(); })));
      w.font = w.font || { family: "", size: 20, bold: false };
      props.append(row("размер шрифта", num(w.font.size, (v) => { w.font.size = v; draw(); })));
      props.append(row("полужирный", chk(w.font.bold, (v) => { w.font.bold = v; draw(); })));
      props.append(row("фон прямоугольника", col(w.fill, (v) => { w.fill = v; draw(); })));
      props.append(row("обводка", col(w.outline, (v) => { w.outline = v; draw(); })));
      break;
    case "doughnut":
    case "doughnut_chart":
      props.append(row("заполнено (цвет)", col(w.used, (v) => { w.used = v; draw(); })));
      props.append(row("фон кольца", col(w.free, (v) => { w.free = v; draw(); })));
      props.append(row("толщина", num(w.thickness, (v) => { w.thickness = v; draw(); })));
      props.append(row("макс. значение", num(w.max, (v) => { w.max = v; draw(); })));
      break;
    case "line_chart":
      props.append(row("цвет линии", col(w.color, (v) => { w.color = v; draw(); })));
      props.append(row("толщина", num(w.thickness, (v) => { w.thickness = v; draw(); })));
      props.append(row("точек истории", num(w.points, (v) => { w.points = v; draw(); })));
      props.append(row("заливка области", chk(w.area, (v) => { w.area = v; draw(); })));
      props.append(row("макс. значение", num(w.max, (v) => { w.max = v; draw(); })));
      break;
    case "bar_chart":
      props.append(row("цвет", col(w.color, (v) => { w.color = v; draw(); })));
      props.append(row("столбцов (≤1 = шкала)", num(w.points, (v) => { w.points = v; draw(); })));
      props.append(row("горизонтально", chk(w.horizontal, (v) => { w.horizontal = v; draw(); })));
      props.append(row("макс. значение", num(w.max, (v) => { w.max = v; draw(); })));
      break;
  }
}

function syncRectInputs() {
  const w = curWidget();
  if (!w || !rectInputs.x) return;
  rectInputs.x.value = w.rect.x;
  rectInputs.y.value = w.rect.y;
  rectInputs.w.value = rectInputs.w ? w.rect.width : 0;
  rectInputs.h.value = w.rect.height;
}

// ---------- palette / screens ----------

$("#palette").addEventListener("click", (ev) => {
  const type = ev.target.dataset && ev.target.dataset.add;
  if (!type) return;
  const n = curScreen().widgets.length;
  const base = {
    id: nextWidgetId(), type,
    rect: { x: 10 + (n % 5) * 10, y: 10 + (n % 5) * 10, width: 80, height: 30 },
    sensor: state.sensors[0] || "",
    format: "{v}",
    color: "#ffffff",
    align: "left",
    font: { family: "", size: 13, bold: false },
  };
  if (type === "doughnut") Object.assign(base.rect = { x: 10, y: 10, width: 60, height: 60 }, { used: "#00ff00", free: "#333333", thickness: 8, max: 100, format: "" });
  if (type === "doughnut_chart") Object.assign(base.rect = { x: 10, y: 10, width: 150, height: 60 }, { used: "#00ff00", free: "#333333", thickness: 8, max: 100, format: "" });
  if (type === "line_chart") Object.assign(base.rect = { x: 10, y: 10, width: 100, height: 40 }, { color: "#00ff00", points: 30, area: false });
  if (type === "bar_chart") Object.assign(base.rect = { x: 10, y: 10, width: 100, height: 12 }, { color: "#00ff00", points: 0, max: 100 });
  if (type === "custom_bar") base.rect = { x: 10, y: 10, width: 100, height: 8 };
  if (type === "spacer") { base.format = ""; base.sensor = ""; }
  clampRect(base.rect);
  curScreen().widgets.push(base);
  state.selected = base.id;
  buildProps();
  draw();
});

$("#delWidget").addEventListener("click", () => {
  const w = curWidget();
  if (!w) return;
  curScreen().widgets = curScreen().widgets.filter((x) => x.id !== w.id);
  state.selected = null;
  buildProps();
  draw();
});

function buildTabs() {
  const tabs = $("#tabs");
  tabs.textContent = "";
  state.draft.screens.forEach((sc, i) => {
    const b = document.createElement("button");
    b.textContent = `${sc.name || "Экран " + (i + 1)}${i === state.panelScreen ? " ●" : ""}`;
    if (!sc.enabled) b.classList.add("off");
    if (i === state.screenIdx) b.classList.add("active");
    b.onclick = () => {
      state.screenIdx = i;
      state.selected = null;
      buildTabs();
      syncScreenCtl();
      buildProps();
      draw();
    };
    tabs.append(b);
  });
}

function syncScreenCtl() {
  const sc = curScreen();
  $("#scName").value = sc.name || "";
  $("#scDur").value = sc.duration || 10000;
  $("#scEnabled").checked = !!sc.enabled;
}

$("#scName").addEventListener("input", () => { curScreen().name = $("#scName").value; buildTabs(); });
$("#scDur").addEventListener("input", () => { curScreen().duration = parseInt($("#scDur").value, 10) || 10000; });
$("#scEnabled").addEventListener("change", () => { curScreen().enabled = $("#scEnabled").checked; buildTabs(); });
$("#addScreen").addEventListener("click", () => {
  let m = 0;
  for (const sc of state.draft.screens) m = Math.max(m, sc.id);
  state.draft.screens.push({ id: m + 1, name: "Экран " + (state.draft.screens.length + 1), enabled: true, duration: 10000, widgets: [] });
  state.screenIdx = state.draft.screens.length - 1;
  state.selected = null;
  buildTabs(); syncScreenCtl(); buildProps(); draw();
});
$("#delScreen").addEventListener("click", () => {
  if (state.draft.screens.length <= 1) return;
  state.draft.screens.splice(state.screenIdx, 1);
  state.screenIdx = Math.min(state.screenIdx, state.draft.screens.length - 1);
  state.selected = null;
  buildTabs(); syncScreenCtl(); buildProps(); draw();
});

// ---------- theme controls ----------

$("#orientation").addEventListener("change", () => {
  state.draft.orientation = $("#orientation").value;
  for (const sc of state.draft.screens) for (const w of sc.widgets) clampRect(w.rect);
  draw();
});
$("#refresh").addEventListener("change", () => { state.draft.refresh = $("#refresh").value; });
$("#themeBg").addEventListener("input", () => { state.draft.background = $("#themeBg").value; draw(); });

// Theme dropdown fed by the config's theme_list — parity with the original
// GUI dropdown (GET /api/config used to carry it).
const normPath = (p) => String(p || "").replace(/\\/g, "/");
async function loadThemes() {
  const pick = $("#themePick");
  try {
    const r = await fetch("/api/themes");
    const j = await r.json();
    const opts = (j.themes || []).map((t) => [String(t.config || ""), t.name || t.config]);
    if (!opts.length) opts.push(["", "— список пуст —"]);
    pick.textContent = "";
    for (const [val, lab] of opts) {
      const o = document.createElement("option");
      o.value = val;
      o.textContent = lab;
      if (val && normPath(val) === normPath(j.theme_file)) o.selected = true;
      pick.append(o);
    }
  } catch (e) {
    /* daemon offline: keep the empty picker */
  }
}
$("#themePick").addEventListener("change", async () => {
  const file = $("#themePick").value;
  if (!file) return;
  const res = await postJSON("/api/theme", { file });
  if (res.status === 200 && res.json && res.json.ok) {
    status("Тема переключена: " + (res.json.name || res.json.file));
    await reloadDraft();
    await loadThemes();
  } else {
    status("Переключение темы: " + (res.json && res.json.error ? res.json.error : res.status));
  }
});

$("#zoom").addEventListener("input", () => { state.view.zoom = parseInt($("#zoom").value, 10) || 2; draw(); });
$("#showGrid").addEventListener("change", () => { state.view.grid = $("#showGrid").checked; draw(); });
$("#snap").addEventListener("change", () => { state.view.snap = $("#snap").checked; });
$("#step").addEventListener("change", () => { state.view.step = parseInt($("#step").value, 10) || 10; draw(); });

// ---------- server round-trips ----------

function status(msg) { $("#status").textContent = msg; }

async function postJSON(path, bodyObj) {
  const r = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(bodyObj),
  });
  return { status: r.status, json: await r.json().catch(() => null) };
}

// Примерить: валидация драфта на сервере и ПОЛНЫЙ redraw физической
// панели (app.PreviewOnScreen) + LED экрана; обратно приходит PNG экрана.
$("#preview").addEventListener("click", async () => {
  status("Отправка драфта на панель…");
  const r = await fetch(`/api/preview?screen=${state.screenIdx}`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(state.draft),
  });
  if (r.status !== 200) {
    const j = await r.json().catch(() => null);
    status("Ошибка: " + (j && j.error ? j.error : r.status));
    return;
  }
  const old = $("#previewImg").src;
  $("#previewImg").src = URL.createObjectURL(await r.blob());
  if (old.startsWith("blob:")) URL.revokeObjectURL(old);
  status("Драфт выведен на настоящий экран (5 с), затем панель вернётся к живым данным");
});

$("#apply").addEventListener("click", async () => {
  const save = $("#saveApply").checked ? "1" : "0";
  const res = await postJSON(`/api/apply?save=${save}`, state.draft);
  if (res.status === 200 && res.json && res.json.ok) {
    status(res.json.saved ? "Тема применена и сохранена в файл" : "Тема применена (без сохранения)");
  } else {
    status("Ошибка: " + (res.json && res.json.error ? res.json.error : res.status));
  }
});

async function reloadDraft() {
  const r = await fetch("/api/theme");
  state.draft = await r.json();
  normalizeDraft();
  state.screenIdx = Math.min(state.panelScreen, state.draft.screens.length - 1);
  state.selected = null;
  syncThemeCtl(); buildTabs(); syncScreenCtl(); buildProps(); draw();
}

$("#reload").addEventListener("click", async () => {
  await reloadDraft();
  status("Драфт перезагружен с панели");
});

// ---------- live screen control (the original GUI buttons) ----------

$("#nextScreen").addEventListener("click", async () => {
  const res = await postJSON("/api/next_screen", {});
  if (res.status === 200 && res.json && res.json.ok) {
    state.panelScreen = res.json.index;
    buildTabs();
    status(`Панель показывает экран ${res.json.index + 1} (id ${res.json.id})`);
  } else {
    status("Следующий экран: " + (res.json && res.json.error ? res.json.error : res.status));
  }
});

$("#pauseRot").addEventListener("change", async () => {
  const on = $("#pauseRot").checked;
  try {
    const r = await fetch("/api/pause?on=" + on, { method: "POST" });
    status(r.status === 200 ? (on ? "Ротация приостановлена" : "Ротация возобновлена") : "Пауза ротации: ошибка " + r.status);
  } catch (e) {
    status("Пауза ротации: " + e);
  }
});

$("#ledApply").addEventListener("click", async () => {
  const res = await postJSON("/api/led", {
    theme: parseInt($("#ledTheme").value, 10),
    intensity: parseInt($("#ledInt").value, 10),
    speed: parseInt($("#ledSpd").value, 10),
  });
  if (res.status === 200) {
    status("LED-лента: режим применён" + (res.json && res.json.saved ? " и сохранён в config.json" : " (в config не сохранён!)"));
  } else {
    status("LED: ошибка " + (res.json && res.json.error ? res.json.error : res.status));
  }
});

function syncThemeCtl() {
  $("#orientation").value = state.draft.orientation || "landscape";
  $("#refresh").value = state.draft.refresh || "update";
  $("#themeBg").value = /^#[0-9a-fA-F]{6}$/.test(state.draft.background || "") ? state.draft.background : "#000000";
}

// Fill defaults the Go side would apply on load, so the draft round-trips
// cleanly through config.ParseTheme without surprises.
function normalizeDraft() {
  const t = state.draft;
  t.orientation = t.orientation || "landscape";
  t.refresh = t.refresh || "update";
  t.background = t.background || "#000000";
  for (const sc of t.screens) {
    sc.duration = sc.duration || 10000;
    sc.enabled = sc.enabled === undefined ? true : sc.enabled;
    sc.widgets = sc.widgets || [];
    for (const w of sc.widgets) {
      w.type = w.type || "text";
      w.color = w.color || "#ffffff";
      w.font = w.font || { family: "", size: 20, bold: false };
      w.font.size = w.font.size || 20;
    }
  }
}

async function poll() {
  try {
    const r = await fetch("/api/poll");
    const j = await r.json();
    state.readings = j.readings || {};
    draw();
  } catch (e) {
    /* daemon offline: keep last numbers */
  }
}

async function boot() {
  const r = await fetch("/api/state");
  const j = await r.json();
  state.sensors = j.sensors || [];
  state.readings = j.readings || {};
  state.draft = j.theme;
  state.panelScreen = j.current_screen || 0;
  state.screenIdx = Math.min(state.panelScreen, state.draft.screens.length - 1);
  normalizeDraft();
  if (j.config && j.config.led) {
    const led = j.config.led;
    $("#ledTheme").value = String(led.theme || 5);
    $("#ledInt").value = String(led.intensity || 3);
    $("#ledSpd").value = String(led.speed || 3);
  }
  syncThemeCtl();
  await loadThemes();
  buildTabs();
  syncScreenCtl();
  buildProps();
  draw();
  setInterval(poll, 1000);
  status("Загружено с панели; живые значения обновляются каждую секунду");
}

boot().catch((e) => status("Не удалось загрузить состояние: " + e));

// @font-face faces load asynchronously; redraw once they are available so
// the editor canvas measures and rasterises with the same Latin Modern the
// device embeds (metrics constants in drawWidget assume it).
if (document.fonts) document.fonts.ready.then(() => draw());
