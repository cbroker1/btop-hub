// btop hub page: one canvas, one pane per machine, redrawn once a second.
'use strict';
(() => {

// ---- Alert thresholds: the only place to change them -------------------------
const ALERT = {
  swapPct: 80,   // swap or page file in use, percent
  memPct: 90,    // memory in use, percent
  diskPct: 90,   // system volume full, percent
  gpuTempC: 85,  // GPU temperature, degrees C
};
const OFFLINE_MS = 5000; // a pane is offline after this long without a sample

// ---- Text size: this much larger than the design's 8 x 20 px cell -----------------------
// The readouts keep their columns and the graphs give up the width the bigger text needs, so a
// pane keeps its size and the time axis keeps its 80 columns, each a little narrower. 1.32 is
// 20% larger, then another 10%; past about 1.4 the axis labels start to collide.
const TEXT_SCALE = 1.32;

// ---- Layout: the smallest character cell at which all machines are shown together -------
// Below either limit the page shows one machine at a time (swipe, arrow keys, or the dots).
// 5 device pixels is where the font stops being legible at all; 3 CSS pixels keeps phones on
// one machine while a high-density tablet like the Z13 (175% scaling) still fits all three.
// Both apply to a character as drawn, TEXT_SCALE included.
const MIN_CELL = { devicePx: 5, cssPx: 3 };

// ---- Colours -----------------------------------------------------------------------
const C = {
  page: '#181a1a', strip: '#1d1f1f', text: '#d4d5ca', dim: '#8a91a8', rule: '#414858', tick: '#6b7388',
  track: '#2e3239', fill: '#a9b4c9', cached: '#5d6780', other: '#3d4453',
  hist: '#6f7b93', histCached: '#3f4759', alert: '#ff6a4d', alertText: '#000000',
  down: '#5fd0d0', up: '#97a6ee', guide: 'rgba(255,255,255,0.10)', wash: 'rgba(255,255,255,0.035)',
};
const STOPS = {
  load: ['#2b2a2e', '#4a2634', '#7d2836', '#b63a2e', '#de6a2b', '#f4a23c', '#ffd166', '#fff0b8'],
  down: ['#22303a', '#1f4a5c', '#1f7387', '#2aa3b0', '#5fd0d0', '#a8ecea', '#e6fbfa'],
  up: ['#2a2b40', '#3a3e75', '#4f58a8', '#6f7dd6', '#97a6ee', '#c5cdfa', '#edf0ff'],
};
// 16 levels; level k sits k/15 of the way along the stops, linear RGB between them.
function makeRamp(stops) {
  const rgb = stops.map(h => [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16)));
  const out = [];
  for (let k = 0; k < 16; k++) {
    const p = k / 15 * (rgb.length - 1);
    const i = Math.min(Math.floor(p), rgb.length - 2), f = p - i;
    out.push(rgb[i].map((v, j) => Math.round(v + (rgb[i + 1][j] - v) * f)));
  }
  return out;
}
const RAMP = {}, VEIL = {}, CAP = {};
for (const k in STOPS) {
  const r = makeRamp(STOPS[k]);
  RAMP[k] = r.map(c => `rgb(${c})`);
  VEIL[k] = r.map(c => `rgba(${c},0.16)`);
  CAP[k] = r.map(c => `rgba(${c},0.85)`);
}
// Level of a 0..1 value.
function level(v) {
  if (!(v >= 0.05)) return 0;
  if (v >= 0.9) return 15;
  return Math.min(14, 1 + Math.floor(Math.pow((v - 0.05) / 0.95, 0.7) * 14));
}

// ---- The time axis: 80 columns over the last hour, non-linear -------------------------
const TICKS = [[0, 3600], [11, 1800], [22, 900], [31, 480], [42, 240], [53, 120], [64, 60], [80, 0]];
const TICK_LABELS = ['1h', '30m', '15m', '8m', '4m', '2m', '1m', 'now'];
// Seconds ago at column edge x (0..80): geometric between ticks, linear in the last minute.
function edgeAge(x) {
  for (let i = 0; i < TICKS.length - 1; i++) {
    const [c0, t0] = TICKS[i], [c1, t1] = TICKS[i + 1];
    if (x <= c1) {
      const u = (x - c0) / (c1 - c0);
      return t1 === 0 ? t0 * (1 - u) : t0 * Math.pow(t1 / t0, u);
    }
  }
  return 0;
}
const EDGE = Array.from({ length: 81 }, (_, x) => edgeAge(x));
const GUIDE_COLS = [11, 22, 31, 42, 53, 64];
function fmtSpan(sec) {
  if (sec >= 60) return (sec / 60).toFixed(1) + ' min';
  return String(+sec.toFixed(2)) + ' s';
}
const SPAN_LEFT = fmtSpan(EDGE[0] - EDGE[1]);
const SPAN_RIGHT = fmtSpan(EDGE[79] - EDGE[80]);

// ---- Pane grid -----------------------------------------------------------------------
const COLS = 120, ROWS = 64, GUTTER = 2;
const GX = 6, GW = 80, RX = 88, REND = 119; // graph start, graph width, readout start, readout end (exclusive)

// ---- Data -------------------------------------------------------------------------------
const CAP_N = 3800;
const machines = new Map(); // id -> machine
let order = [];
// ?m=gpu-rig,z13 shows only those machines (for example on a wall display).
const ONLY_LIST = (new URLSearchParams(location.search).get('m') || '').split(',').filter(Boolean);
const ONLY = ONLY_LIST.length ? ONLY_LIST : null;
let clockOffset = 0; // hub clock minus this browser's clock, ms
const hubNow = () => Date.now() + clockOffset;

// Per machine, the last hour of samples as parallel arrays (NaN where a value is missing). The
// panes draw cpu, gu, gm, mu, mc, dr, dw, nr and nt; the detail overlay uses the rest too.
const SERIES = ['t', 'mu', 'mc', 'su', 'cu', 'dr', 'dw', 'nr', 'nt', 'cf', 'ct'];
const GPU_SERIES = ['gu', 'gm', 'gt', 'gp', 'gck', 'gmk', 'gtx', 'grx'];
function makeMachine(j) {
  const info = j.info || {};
  const T = info.threads || 0, G = (info.gpus || []).length;
  const m = {
    id: j.id, name: j.name, info, connected: !!j.connected, lastT: 0, last: null,
    T, G, n: 0, cpu: new Uint16Array(CAP_N * Math.max(T, 1)),
  };
  for (const k of SERIES) m[k] = new Float64Array(CAP_N);
  for (const k of GPU_SERIES) m[k] = Array.from({ length: G }, () => new Float64Array(CAP_N));
  return m;
}
function compact(m) {
  const cut = m.t[m.n - 1] - 3660e3;
  let k = 0;
  while (k < m.n && m.t[k] < cut) k++;
  if (k < 200) k = 200;
  for (const s of SERIES) m[s].copyWithin(0, k, m.n);
  m.cpu.copyWithin(0, k * m.T, m.n * m.T);
  for (const s of GPU_SERIES) for (const a of m[s]) a.copyWithin(0, k, m.n);
  m.n -= k;
}
const num = v => (v === undefined || v === null ? NaN : v);
function push(m, s) {
  if (m.n && s.t <= m.t[m.n - 1]) return;
  if (m.n === CAP_N) compact(m);
  const i = m.n++;
  m.t[i] = s.t;
  const c = s.c, T = m.T;
  if (c && c.length === T) for (let k = 0; k < T; k++) m.cpu[i * T + k] = c[k];
  else m.cpu.fill(0xffff, i * T, i * T + T);
  for (let g = 0; g < m.G; g++) {
    const gs = (s.g && s.g[g]) || {};
    m.gu[g][i] = gs.u != null ? gs.u / 1000 : NaN;
    m.gm[g][i] = num(gs.m); m.gt[g][i] = num(gs.t); m.gp[g][i] = num(gs.p);
    m.gck[g][i] = num(gs.ck); m.gmk[g][i] = num(gs.mk); m.gtx[g][i] = num(gs.tx); m.grx[g][i] = num(gs.rx);
  }
  m.mu[i] = num(s.mu); m.mc[i] = num(s.mc); m.su[i] = num(s.su); m.cu[i] = num(s.cu);
  m.dr[i] = num(s.dr); m.dw[i] = num(s.dw);
  m.nr[i] = num(s.nr); m.nt[i] = num(s.nt);
  m.cf[i] = num(s.cf); m.ct[i] = num(s.ct);
  m.last = s;
  m.lastT = s.t;
}

function sameShape(a, b) {
  return (a.threads || 0) === (b.threads || 0) && (a.gpus || []).length === (b.gpus || []).length;
}

// ---- Loading -------------------------------------------------------------------------------
async function loadHistory(onlyId) {
  const t0 = Date.now();
  const res = await fetch(onlyId ? '/api/history?m=' + encodeURIComponent(onlyId) : '/api/history', { cache: 'no-store' });
  if (!res.ok) throw new Error('history ' + res.status);
  const j = await res.json();
  const t1 = Date.now();
  clockOffset = j.now - (t0 + t1) / 2;
  const seen = new Set();
  for (const mj of j.machines) {
    const m = makeMachine(mj);
    for (const s of mj.samples) push(m, s);
    if (!m.lastT && mj.lastT) m.lastT = mj.lastT;
    machines.set(m.id, m);
    seen.add(m.id);
  }
  if (!onlyId) {
    for (const id of [...machines.keys()]) if (!seen.has(id)) machines.delete(id);
    setOrder(j.machines.map(m => m.id).filter(id => !ONLY || ONLY.includes(id)));
  }
  layoutDirty = true;
}

// Replace the machine list, keeping one-at-a-time mode on the machine it was showing.
function setOrder(next) {
  const i = next.indexOf(order[idx]);
  order = next;
  if (i >= 0) idx = i;
  layoutDirty = true;
}

let es = null, reloadAll = false, reloadTimer = 0;
// Reload history after 250 ms; two different requests in that window become a full reload,
// so a machine new to the page always reaches the pane list.
let reloadWhat;
function scheduleReload(id = '') {
  reloadWhat = reloadWhat === undefined || reloadWhat === id ? id : '';
  clearTimeout(reloadTimer);
  reloadTimer = setTimeout(() => {
    const what = reloadWhat;
    reloadWhat = undefined;
    loadHistory(what).then(draw).catch(() => {});
  }, 250);
}
function openStream() {
  if (es) es.close();
  es = new EventSource('/api/stream');
  es.addEventListener('hello', e => {
    const j = JSON.parse(e.data);
    clockOffset = j.now - Date.now();
    for (const id in j.connected) { const m = machines.get(id); if (m) m.connected = j.connected[id]; }
    if (reloadAll) { reloadAll = false; scheduleReload(); }
  });
  es.addEventListener('s', e => {
    const j = JSON.parse(e.data);
    const m = machines.get(j.m);
    if (!m) { scheduleReload(); return; }
    push(m, j.s);
    m.connected = true;
    const cand = j.s.t - Date.now();
    clockOffset = Math.abs(cand - clockOffset) > 1500 ? cand : clockOffset * 0.9 + cand * 0.1;
  });
  es.addEventListener('info', e => {
    const j = JSON.parse(e.data);
    const m = machines.get(j.id);
    if (!m || !sameShape(m.info, j.info)) { scheduleReload(); return; }
    m.name = j.name; m.info = j.info; m.connected = true;
  });
  es.addEventListener('status', e => { const j = JSON.parse(e.data); const m = machines.get(j.m); if (m) m.connected = j.connected; });
  es.addEventListener('reload', e => scheduleReload(JSON.parse(e.data).m));
  es.addEventListener('gone', e => { machines.delete(JSON.parse(e.data).m); setOrder(order.filter(id => machines.has(id))); });
  es.onerror = () => { reloadAll = true; };
}

// ---- Formatting ---------------------------------------------------------------------------
const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
function unitOf(v) { let u = 0; while (v >= 1024 && u < 5) { v /= 1024; u++; } return u; }
// Three significant digits, truncated like btop: 47.99 GiB -> "47.9".
function trunc3(x) {
  if (x >= 100) return String(Math.floor(x + 1e-9));
  if (x >= 10) return (Math.floor(x * 10 + 1e-9) / 10).toFixed(1);
  return (Math.floor(x * 100 + 1e-9) / 100).toFixed(2);
}
function size(v) {
  if (!isFinite(v)) return 'n/a';
  const u = unitOf(v);
  return (u ? trunc3(v / 1024 ** u) : String(Math.round(v))) + ' ' + UNITS[u];
}
// "41.9 / 122 GiB", "841 MiB / 47.9 GiB", "0 / 8.00 GiB": each number keeps three
// significant digits; the unit is written once when both share it.
function pair(used, total) {
  if (!isFinite(total)) return isFinite(used) ? size(used) : 'n/a';
  if (!isFinite(used)) return 'n/a / ' + size(total);
  const tu = unitOf(total);
  if (used > 0 && tu > 0 && used < 1024 ** tu) return size(used) + ' / ' + size(total);
  const us = used === 0 ? '0' : tu ? trunc3(used / 1024 ** tu) : String(Math.round(used));
  return us + ' / ' + size(total);
}
function rate(v) {
  if (!isFinite(v)) return 'n/a';
  const u = unitOf(v);
  if (!u) return Math.round(v) + ' B/s';
  const x = v / 1024 ** u;
  const s = x >= 100 ? x.toFixed(0) : x >= 10 ? x.toFixed(1) : x.toFixed(2);
  return String(+s) + ' ' + UNITS[u] + '/s';
}
const pct = f => (isFinite(f) ? Math.round(f * 100) + '%' : 'n/a');
const two = n => String(n).padStart(2, '0');
function clock(ms) { const d = new Date(ms); return `${two(d.getHours())}:${two(d.getMinutes())}:${two(d.getSeconds())}`; }
function uptime(sec) {
  const d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60), s = Math.floor(sec % 60);
  return d ? `up ${d}d ${two(h)}:${two(m)}` : `up ${two(h)}:${two(m)}:${two(s)}`;
}
function freq(mhz) { return mhz >= 1000 ? (mhz / 1000).toFixed(1) + ' GHz' : Math.round(mhz) + ' MHz'; }
function link(bps) { return bps >= 1e9 ? 'link ' + +(bps / 1e9).toFixed(1) + ' Gbps' : 'link ' + Math.round(bps / 1e6) + ' Mbps'; }
function shortModel(s) {
  return (s || '').replace(/\bSSD\b/i, '').replace(/\s+\d+(\.\d+)?\s*(GB|TB|G|T)\s*$/i, '').replace(/\s+/g, ' ').trim();
}
// Log scale for throughput: 1 KiB/s at the baseline, 1 GiB/s at full height.
const logFrac = v => (v > 1024 ? Math.min(1, Math.log2(v / 1024) / 20) : 0);

// ---- Aggregation: mean and peak of the 1-second samples in each column ---------------------
function stat(n) { return { sum: new Float64Array(n), max: new Float64Array(n), cnt: new Uint16Array(n) }; }
function addTo(st, col, v) { if (v === v) { st.sum[col] += v; if (st.cnt[col] === 0 || v > st.max[col]) st.max[col] = v; st.cnt[col]++; } }
function aggregate(m, now) {
  const E = new Float64Array(81);
  for (let x = 0; x <= 80; x++) E[x] = now - EDGE[x] * 1000;
  const T = m.T;
  const a = {
    cpuSum: new Float64Array(80 * T), cpuMax: new Float64Array(80 * T), cpuCnt: new Uint16Array(80),
    gu: m.gu.map(() => stat(80)), gm: m.gm.map(() => stat(80)),
    mu: stat(80), mc: stat(80), dr: stat(80), dw: stat(80), nr: stat(80), nt: stat(80),
    peakRx: NaN, peakTx: NaN, peakDr: NaN, peakDw: NaN,
  };
  let i = 0;
  while (i < m.n && m.t[i] < E[0]) i++;
  let col = 0;
  for (; i < m.n; i++) {
    const t = m.t[i];
    if (t > now + 2000) break;
    while (col < 79 && t >= E[col + 1]) col++;
    if (m.cpu[i * T] !== 0xffff && T) {
      a.cpuCnt[col]++;
      for (let k = 0; k < T; k++) {
        const v = m.cpu[i * T + k] / 1000, o = col * T + k;
        a.cpuSum[o] += v;
        if (v > a.cpuMax[o]) a.cpuMax[o] = v;
      }
    }
    for (let g = 0; g < m.G; g++) { addTo(a.gu[g], col, m.gu[g][i]); addTo(a.gm[g], col, m.gm[g][i]); }
    addTo(a.mu, col, m.mu[i]); addTo(a.mc, col, m.mc[i]); addTo(a.dr, col, m.dr[i]); addTo(a.dw, col, m.dw[i]);
    addTo(a.nr, col, m.nr[i]); addTo(a.nt, col, m.nt[i]);
    if (m.nr[i] === m.nr[i] && !(m.nr[i] <= a.peakRx)) a.peakRx = m.nr[i];
    if (m.nt[i] === m.nt[i] && !(m.nt[i] <= a.peakTx)) a.peakTx = m.nt[i];
    if (m.dr[i] === m.dr[i] && !(m.dr[i] <= a.peakDr)) a.peakDr = m.dr[i];
    if (m.dw[i] === m.dw[i] && !(m.dw[i] <= a.peakDw)) a.peakDw = m.dw[i];
  }
  return a;
}

// ---- Pane layout (rows) ----------------------------------------------------------------
function paneRows(m) {
  const L = { cpu: 4, gpus: [] };
  let r = 21;
  const G = m.G;
  if (G > 0) { L.gpus.push({ g: 0, top: r, util: 8, vram: r + 9 }); r += 12; }
  for (let g = 1; g < G && r + 4 + 6 + 8 + 6 <= 61; g++) { L.gpus.push({ g, top: r, util: 3 }); r += 4; }
  L.mem = r; r += 6;
  L.disk = r; r += 8;
  L.net = r; L.netRows = 61 - r;
  return L;
}

// ---- Canvas -----------------------------------------------------------------------------
const cv = document.getElementById('c');
const ctx = cv.getContext('2d', { alpha: false });
const VGUTTER = 1; // rows between panes stacked vertically
// Rows stretch to fill the screen's height (the design's cell is 8:20, i.e. 2.5x as tall as
// wide), up to this many times the design's cell width: 4.5 fills 16:9, 16:10 and 3:2 screens
// and most phones. Text, bars and badges keep their size, centred in a stretched row; the graphs grow.
const MAX_ROW = 4.5;
// Across, a pane is COLS units (one unit is the design's cell width): GX text cells, the GW
// graph columns, then text cells to the end. Text cells are TEXT_SCALE units; graph columns get
// what is left.
const WG = (COLS - (COLS - GW) * TEXT_SCALE) / GW;
let u = 8, wt = 8 * TEXT_SCALE, wg = 8 * WG, h = 20, fontPx = wt / 0.6, layoutDirty = true;
let cx0 = 0, cy0 = 0; // top-left corner of the panes on the canvas, unzoomed device pixels
let single = false;  // one machine at a time
let roomy = true;    // all machines fit at a readable size
let focus = null;    // id of the machine the user chose to see alone
let idx = 0;         // which machine single mode shows
let grid = { pc: 1, pr: 1 }; // panes per row, rows of panes

function layout() {
  const dpr = window.devicePixelRatio || 1;
  const vw = Math.max(1, Math.floor(window.innerWidth * dpr)), vh = Math.max(1, Math.floor(window.innerHeight * dpr));
  const n = Math.max(1, order.length);
  // Units as large as the viewport allows: the panes' width, or their rows at 2.5 units.
  const fit = (cols, rows) => Math.min(vw / cols, vh / (rows * 2.5));
  // Try every arrangement (3 across, 2+1 stacked, ...): the largest units win, and within 2%
  // the one with more panes side by side.
  let best = null;
  for (let pc = n; pc >= 1; pc--) {
    const pr = Math.ceil(n / pc);
    if (pc > 1 && Math.ceil(n / (pc - 1)) === pr) continue; // a narrower grid holds the same rows
    const cols = pc * COLS + (pc - 1) * GUTTER, rows = pr * ROWS + (pr - 1) * VGUTTER;
    const cu = fit(cols, rows);
    if (!best || cu > best.u * 1.02) best = { pc, pr, cols, rows, u: cu };
  }
  roomy = n === 1 || best.u * TEXT_SCALE >= Math.max(MIN_CELL.devicePx, MIN_CELL.cssPx * dpr);
  if (focus !== null && !order.includes(focus)) focus = null;
  single = n > 1 && (focus !== null || !roomy);
  if (focus !== null) idx = order.indexOf(focus);
  if (idx >= n) idx = 0;
  let cols = best.cols, rows = best.rows;
  grid = { pc: best.pc, pr: best.pr };
  if (single) { cols = COLS; rows = ROWS; grid = { pc: 1, pr: 1 }; }
  u = fit(cols, rows);
  wt = u * TEXT_SCALE;
  wg = u * WG;
  // Rows fill the height exactly (up to MAX_ROW units). Sizes are fractional; every edge is
  // rounded to a whole device pixel when drawn, so edges stay crisp.
  h = Math.max(2, Math.min(vh / rows, MAX_ROW * u));
  fontPx = wt / 0.6; // JetBrains Mono advances 0.6 em, so each character is exactly one text cell
  // The canvas covers the viewport, so a zoomed view can use all of it; the panes are centred.
  if (cv.width !== vw || cv.height !== vh) { cv.width = vw; cv.height = vh; }
  cv.style.width = vw / dpr + 'px';
  cv.style.height = vh / dpr + 'px';
  cx0 = Math.floor((vw - cols * u) / 2);
  cy0 = Math.floor((vh - rows * h) / 2);
  clampZoom();
  layoutDirty = false;
}

// ---- Zoom: two-finger pinch, a touchpad pinch or Ctrl + wheel -------------------------------
const ZOOM_MAX = 6;
let zs = 1, ztx = 0, zty = 0; // canvas pixel = zs * unzoomed pixel + (ztx, zty)
// Keep the zoomed view on the canvas: no zooming out past the whole view, no panning off it.
function clampZoom() {
  if (!(zs > 1.0001)) { zs = 1; ztx = 0; zty = 0; return; }
  zs = Math.min(ZOOM_MAX, zs);
  ztx = Math.min(0, Math.max(cv.width * (1 - zs), ztx));
  zty = Math.min(0, Math.max(cv.height * (1 - zs), zty));
}
// Scale by f, keeping canvas point (px, py) where it is.
function zoomAt(f, px, py) {
  const s = Math.min(ZOOM_MAX, Math.max(1, zs * f));
  ztx = px - (px - ztx) * s / zs;
  zty = py - (py - zty) * s / zs;
  zs = s;
  clampZoom();
  requestDraw();
}
// Back to the whole view; false if it was already there.
function resetZoom() {
  if (zs === 1 && !ztx && !zty) return false;
  zs = 1; ztx = 0; zty = 0;
  requestDraw();
  return true;
}
let drawQueued = false;
// Redraw on the next frame, once per frame at most: gestures move faster than the 1 s tick.
function requestDraw() {
  if (drawQueued) return;
  drawQueued = true;
  requestAnimationFrame(() => { drawQueued = false; if (running) draw(); });
}

// ---- Painter for one pane: ox is its left edge (unzoomed device pixels), oyRows its first row --
let ox = 0, oyRows = 0, curFont = '';
// Unzoomed x of column c in a pane: text cells, the graph's columns, then text cells again.
function colX(c) {
  if (c <= GX) return c * wt;
  if (c <= GX + GW) return GX * wt + (c - GX) * wg;
  return GX * wt + GW * wg + (c - GX - GW) * wt;
}
const sx = x => Math.round(zs * (cx0 + x) + ztx); // unzoomed position -> canvas pixel
const sy = y => Math.round(zs * (cy0 + y) + zty);
const X = c => sx(ox + colX(c));
const Y = r => sy((oyRows + r) * h);
// Sizes on the canvas, zoom included.
const cellPx = () => wt * zs;                            // one character
const rowPx = () => h * zs;                              // one row
const fontPxZ = () => fontPx * zs;
const lineW = () => Math.max(1, Math.round(u * zs / 8)); // 1 px at the design's size
const barH = () => Math.round(0.9 * fontPxZ());          // 12 px at the design's size
function rect(x0, y0, x1, y1, color) { ctx.fillStyle = color; ctx.fillRect(x0, y0, x1 - x0, y1 - y0); }
function cells(c0, r0, c1, r1, color) { rect(X(c0), Y(r0), X(c1), Y(r1), color); }
function font(bold) {
  const f = `${bold ? 700 : 400} ${fontPxZ()}px JBM`;
  if (f !== curFont) { ctx.font = f; curFont = f; }
}
const glyphs = s => [...s].length;
const textW = s => glyphs(s) * cellPx();
// Text starting at canvas x, centred in row `row`.
function textAt(x, row, s, color, bold) {
  if (!s) return;
  font(bold);
  ctx.fillStyle = color;
  ctx.fillText(s, Math.round(x), Y(row) + Math.round(rowPx() / 2 + 0.36 * fontPxZ())); // baseline of a centred line
}
function text(col, row, s, color, bold) { textAt(X(col), row, s, color, bold); }
// Text ending at column `end`.
function textR(end, row, s, color, bold) { if (s) textAt(X(end) - textW(s), row, s, color, bold); }
// First variant that leaves at least one blank cell after a left label of length used.
function fit(used, ...variants) {
  for (const v of variants) if (v && used + 1 + glyphs(v) <= REND - RX) return v;
  return '';
}
// A 12 px bar (at the design's size) from canvas x0 to x1 in row r; segments are [fraction, colour].
function barAt(x0, x1, r, segs, track) {
  x0 = Math.round(x0); x1 = Math.round(x1);
  const bh = barH(), y0 = Y(r) + Math.round((rowPx() - bh) / 2), y1 = y0 + bh;
  rect(x0, y0, x1, y1, track || C.track);
  let x = x0;
  for (const [f, color] of segs) {
    if (!(f > 0)) continue;
    const nx = Math.min(x1, x + Math.round(Math.min(1, f) * (x1 - x0)));
    if (nx > x) rect(x, y0, nx, y1, color);
    x = nx;
  }
}
function bar(c0, c1, r, segs, track) { barAt(X(c0), X(c1), r, segs, track); }
// Alert badge: alert colour behind black text, padded by one cell. Returns the end column.
function badge(col, r, s) {
  const n = glyphs(s) + 2, x0 = X(col), x1 = Math.round(x0 + n * cellPx());
  const rowH = Y(r + 1) - Y(r), bh = Math.min(rowH, Math.round(1.5 * fontPxZ()));
  const y0 = Y(r) + Math.round((rowH - bh) / 2);
  rect(x0, y0, x1, y0 + bh, C.alert);
  textAt(x0 + cellPx(), r, s, C.alertText, true);
  return col + n;
}
function badgeR(end, r, s) { badge(end - glyphs(s) - 2, r, s); }

// ---- Strip backgrounds, guides, wash and the no-data rule ------------------------------------
function stripBg(r0, r1, has) {
  const lw = lineW();
  for (let c = 0; c < GW; c++) {
    if (!has(c)) continue;
    cells(GX + c, r0, GX + c + 1, r1, C.strip);
    if (c >= 64) cells(GX + c, r0, GX + c + 1, r1, C.wash);
  }
  for (const c of GUIDE_COLS) if (has(c)) rect(X(GX + c), Y(r0), X(GX + c) + lw, Y(r1), C.guide);
}
// A 1 px rule on the blank side wherever data starts or stops.
function edgeRules(r0, r1, has) {
  const lw = lineW();
  for (let c = 0; c < GW - 1; c++) {
    const a = has(c), b = has(c + 1);
    if (a === b) continue;
    const x = X(GX + c + 1);
    if (b) rect(x - lw, Y(r0), x, Y(r1), C.rule);
    else rect(x, Y(r0), x + lw, Y(r1), C.rule);
  }
}

// One column of an area chart. base: the baseline, in rows; dir -1 grows up, +1 grows down.
// Edges go through Y(), so they land on the same pixels as the strip's cell boundaries.
function areaCol(c, base, dir, rows, mean, peak, key) {
  const e8 = rows * 8;
  let me = Math.max(1, Math.round(mean * e8)), pe = Math.max(me, Math.round(peak * e8));
  me = Math.min(me, e8); pe = Math.min(pe, e8);
  const x0 = X(GX + c), x1 = X(GX + c + 1);
  const band = (e0, e1, color) => {
    const ya = Y(base + dir * e0 / 8), yb = Y(base + dir * e1 / 8);
    rect(x0, Math.min(ya, yb), x1, Math.max(ya, yb), color);
  };
  const lvl = r => Math.max(2, level((r + 0.5) / rows));
  for (let r = 0; r * 8 < pe; r++) {
    const lo = r * 8, hi = lo + 8, L = lvl(r);
    if (Math.min(me, hi) > lo) band(lo, Math.min(me, hi), RAMP[key][L]);
    const v0 = Math.max(me, lo), v1 = Math.min(pe, hi);
    if (v1 > v0) band(v0, v1, VEIL[key][L]);
  }
  if (pe > me) band(pe - 1, pe, CAP[key][lvl(Math.floor((pe - 1) / 8))]);
}
function flatCol(c, base, rows, from, to, color) {
  const e8 = rows * 8;
  const e0 = Math.max(0, Math.min(e8, Math.round(from * e8))), e1 = Math.max(0, Math.min(e8, Math.round(to * e8)));
  if (e1 <= e0) return;
  rect(X(GX + c), Y(base - e1 / 8), X(GX + c + 1), Y(base - e0 / 8), color);
}
const mean = (st, c) => st.sum[c] / st.cnt[c];

// Y-axis label: right-aligned in columns 1..5, vertically centred on a position in rows,
// kept inside the strip [top, bottom).
function ylabel(s, centre, top, bottom) {
  const c = Math.min(bottom - 0.5, Math.max(top + 0.5, centre));
  textR(GX - 1, c - 0.5, s, C.dim); // labels end one cell before the graph
}

// Aggregates change once a second and with each sample; gestures redraw far more often.
function aggregated(m, now) {
  const sec = Math.floor(now / 1000);
  if (!m.agg || m.aggSec !== sec || m.aggT !== m.lastT || m.aggN !== m.n) {
    m.agg = aggregate(m, now); m.aggSec = sec; m.aggT = m.lastT; m.aggN = m.n;
  }
  return m.agg;
}

// ---- Drawing one pane -------------------------------------------------------------------------
function drawPane(m, now) {
  const info = m.info || {};
  const s = m.last || {};
  const L = paneRows(m);
  const a = aggregated(m, now);
  const offline = !m.lastT || now - m.lastT > OFFLINE_MS;

  drawHeader(m, info, s, now, offline);
  drawAxis(3, true);
  drawAxis(61, false);

  // CPU heat strip: one half-height row per logical thread.
  const T = m.T, hasCpu = c => a.cpuCnt[c] > 0;
  if (T) {
    for (let c = 0; c < GW; c++) {
      const n = a.cpuCnt[c];
      if (!n) continue;
      const x0 = X(GX + c), x1 = X(GX + c + 1);
      for (let k = 0; k < T; k++) {
        const o = c * T + k, v = (a.cpuSum[o] / n + a.cpuMax[o]) / 2;
        rect(x0, Y(4 + k * 16 / T), x1, Y(4 + (k + 1) * 16 / T), RAMP.load[level(v)]);
      }
    }
    edgeRules(4, 20, hasCpu);
    for (const k of [0, T / 4, T / 2, 3 * T / 4, T - 1].map(Math.floor)) textR(GX - 1, 4 + Math.floor(k * 16 / T), String(k), C.dim);
  }

  // GPUs
  for (const G of L.gpus) {
    const gi = G.g, gu = a.gu[gi], gm = a.gm[gi];
    const has = c => gu.cnt[c] > 0;
    const top = G.top, bottom = G.vram ? G.vram + 2 : top + G.util;
    stripBg(top, bottom, has);
    const base = top + G.util;
    for (let c = 0; c < GW; c++) if (has(c)) areaCol(c, base, -1, G.util, mean(gu, c), gu.max[c], 'load');
    if (G.vram) {
      const tot = (info.gpus[gi] || {}).memTotal;
      const vb = G.vram + 2;
      if (tot > 0) for (let c = 0; c < GW; c++) if (gm.cnt[c]) flatCol(c, vb, 2, 0, Math.max(1 / 16, mean(gm, c) / tot), C.hist);
      ylabel((info.gpus[gi] || {}).shared ? 'mem' : 'vram', G.vram + 1, G.vram, G.vram + 2); // <= 4 glyphs fit the gutter
    }
    edgeRules(top, bottom, has);
    ylabel('100', top, top, top + G.util);
    ylabel('0', top + G.util, top, top + G.util);
  }

  // Memory: used, with cached stacked above.
  const memTot = info.memTotal;
  const hasMem = c => a.mu.cnt[c] > 0;
  stripBg(L.mem, L.mem + 5, hasMem);
  if (memTot > 0) {
    const base = L.mem + 5;
    for (let c = 0; c < GW; c++) {
      if (!hasMem(c)) continue;
      const u = Math.max(1 / 40, mean(a.mu, c) / memTot);
      if (a.mc.cnt[c]) flatCol(c, base, 5, Math.round(u * 40) / 40, Math.min(1, u + mean(a.mc, c) / memTot), C.histCached);
      flatCol(c, base, 5, 0, u, C.hist);
    }
  }
  edgeRules(L.mem, L.mem + 5, hasMem);
  ylabel('100', L.mem, L.mem, L.mem + 5);
  ylabel('0', L.mem + 5, L.mem, L.mem + 5);

  // Disk I/O, as the network: reads up from the centre line, writes down, on the fixed log scale.
  const DH = 3.5, dmid = L.disk + DH;
  const hasDisk = c => a.dr.cnt[c] > 0 || a.dw.cnt[c] > 0;
  stripBg(L.disk, L.disk + 7, hasDisk);
  for (let c = 0; c < GW; c++) {
    if (a.dr.cnt[c]) areaCol(c, dmid, -1, DH, logFrac(mean(a.dr, c)), logFrac(a.dr.max[c]), 'down');
    if (a.dw.cnt[c]) areaCol(c, dmid, 1, DH, logFrac(mean(a.dw, c)), logFrac(a.dw.max[c]), 'up');
  }
  edgeRules(L.disk, L.disk + 7, hasDisk);
  ylabel('1G', L.disk, L.disk, L.disk + 7);
  ylabel('1K', dmid, L.disk, L.disk + 7);
  ylabel('1G', L.disk + 7, L.disk, L.disk + 7);

  // Network: download up from the centre line, upload down.
  const NR = L.netRows, half = NR / 2, mid = L.net + half;
  const hasNet = c => a.nr.cnt[c] > 0 || a.nt.cnt[c] > 0;
  stripBg(L.net, L.net + NR, hasNet);
  for (let c = 0; c < GW; c++) {
    if (a.nr.cnt[c]) areaCol(c, mid, -1, half, logFrac(mean(a.nr, c)), logFrac(a.nr.max[c]), 'down');
    if (a.nt.cnt[c]) areaCol(c, mid, 1, half, logFrac(mean(a.nt, c)), logFrac(a.nt.max[c]), 'up');
  }
  edgeRules(L.net, L.net + NR, hasNet);
  ylabel('1G', L.net, L.net, L.net + NR);
  ylabel('1M', mid - half / 2, L.net, L.net + NR);
  ylabel('1K', mid, L.net, L.net + NR);
  ylabel('1M', mid + half / 2, L.net, L.net + NR);
  ylabel('1G', L.net + NR, L.net, L.net + NR);

  // Caption row: the span of a column at each end of the axis, or why part of it is blank. The
  // left caption takes the longest wording that leaves a cell before the right one.
  const right = 'one column = ' + SPAN_RIGHT + ' here';
  const boot = info.boot;
  const blank = !hasCpu(0) && !hasMem(0);
  let lefts = ['one column = ' + SPAN_LEFT + ' here'];
  if (boot && now - boot < 3600e3 && blank) lefts = [`blank = before boot, ${Math.round((now - boot) / 60000)} min ago`, 'blank = before boot'];
  else if ([...Array(GW).keys()].some(c => !hasMem(c) && !hasCpu(c))) lefts = ['blank = no data (asleep or agent stopped)', 'blank = no data (asleep or stopped)', 'blank = no data'];
  const room = (colX(GX + GW) - colX(GX)) * zs - textW(right) - cellPx();
  text(GX, 62, lefts.find(v => textW(v) <= room + 0.5) || '', C.dim); // + 0.5: an exact fit survives rounding
  textR(GX + GW, 62, right, C.dim);

  // Readouts, dimmed while offline.
  ctx.globalAlpha = offline ? 0.45 : 1;
  drawCPU(m, info, s, L);
  for (const G of L.gpus) drawGPU(m, info, s, G);
  drawMem(info, s, L.mem);
  drawDisk(info, s, a, L.disk);
  drawNet(info, s, a, L.net, NR);
  ctx.globalAlpha = 1;
}

function drawHeader(m, info, s, now, offline) {
  text(1, 1, m.name, C.text, true);
  let left = X(1) + textW(m.name); // canvas x where the left group ends
  if (info.sim) { textAt(left + 2 * cellPx(), 1, 'simulated', C.dim); left += textW('  simulated'); }
  // Right group, laid out from the right edge: the uptime ends every header, so the panes line
  // up; a battery (laptops, tablets) sits before it.
  let end = X(REND);
  if (s.up != null) {
    const up = s.up + Math.max(0, (now - m.lastT) / 1000) * (offline ? 0 : 1);
    const t = uptime(up);
    textAt(end - textW(t), 1, t, C.dim);
    end -= textW(t) + 2 * cellPx();
  }
  if (info.battery && s.b != null) {
    barAt(end - 10 * cellPx(), end, 1, [[s.b / 100, C.fill]]);
    end -= 12 * cellPx();
    const icon = s.bs === 1 ? '▲' : s.bs === 2 ? '▼' : '■';
    const t = `BAT ${icon} ${s.b}%`;
    textAt(end - textW(t), 1, t, C.text);
    end -= textW(t) + 2 * cellPx();
  }
  let mid;
  if (!m.lastT) mid = 'offline, not seen since the hub started';
  else if (offline) mid = 'offline, last seen ' + (now - m.lastT > 86400e3 ? new Date(m.lastT).toLocaleDateString() + ' ' : '') + clock(m.lastT);
  else mid = clock(m.lastT);
  const space = end - left;
  if (textW(mid) + 2 * cellPx() <= space) textAt(left + (space - textW(mid)) / 2, 1, mid, C.text);
  rect(X(1), Y(2.5), X(REND), Y(2.5) + lineW(), C.rule);
}

function drawAxis(row, top) {
  const lw = lineW(), th = Math.round(rowPx() * 0.7);
  for (let i = 0; i < TICKS.length; i++) {
    const c = TICKS[i][0];
    let x = X(GX + c);
    if (c === GW) x -= lw; // the "now" tick sits on the graph's last pixel
    const y0 = top ? Y(row + 1) - th : Y(row);
    rect(x, y0, x + lw, y0 + th, C.tick);
    if (c < GW) text(GX + c + 1, row, TICK_LABELS[i], C.dim);
  }
  textR(GX + GW - 1, row, 'now', C.dim);
}

const CPU_LIST = 11; // busiest threads listed: the heat strip's 16 rows less the 4 above and 1 below
function drawCPU(m, info, s, L) {
  const r = L.cpu, T = m.T;
  const loads = s.c && s.c.length ? s.c.map(v => v / 1000) : null;
  const avg = loads ? loads.reduce((x, y) => x + y, 0) / loads.length : NaN;
  text(RX, r, 'CPU', C.text, true);
  textR(REND, r, loads ? pct(avg) : 'n/a', C.text, true);
  text(RX, r + 1, info.cpu || 'n/a', C.dim);
  textR(REND, r + 1, (info.cores ? info.cores + 'c / ' : '') + (T || '?') + 't', C.dim);
  text(RX, r + 2, 'clock ' + (s.cf != null ? freq(s.cf) : 'n/a'), C.dim);
  let right;
  if (!info.hasTemp) right = 'no temp sensor';
  else right = (s.ct != null ? Math.round(s.ct) + '°C' : 'n/a') + (s.la && s.la.length ? ' · load ' + s.la[0].toFixed(1) : '');
  textR(REND, r + 2, right, C.dim);
  text(RX, r + 3, 'busiest threads', C.dim);
  if (loads) {
    // Busiest first, as many as fit beside the heat strip (rows r+4 .. r+14), then the idle count
    // on the strip's last row.
    const top = loads.map((v, i) => [v, i]).sort((p, q) => q[0] - p[0] || p[1] - q[1]).slice(0, CPU_LIST);
    top.forEach(([v, i], k) => {
      text(RX, r + 4 + k, 'C' + i, C.text);
      bar(RX + 5, RX + 25, r + 4 + k, [[v, RAMP.load[level(v)]]]);
      textR(REND, r + 4 + k, pct(v), C.text);
    });
    text(RX, r + 4 + top.length, `${loads.filter(v => v < 0.05).length} of ${loads.length} threads under 5%`, C.dim);
  } else {
    text(RX, r + 4, 'n/a', C.dim);
  }
}

function gpuTemp(gs, end, r) {
  if (gs.t == null) { textR(end, r, 'n/a', C.dim); return; }
  const t = Math.round(gs.t) + '°C';
  if (gs.t >= ALERT.gpuTempC) badgeR(end, r, t); else textR(end, r, t, C.dim);
}

function drawGPU(m, info, s, G) {
  const gi = G.g, gin = (info.gpus || [])[gi] || {}, gs = (s.g && s.g[gi]) || {};
  const r = G.top;
  text(RX, r, m.G > 1 ? 'GPU ' + gi : 'GPU', C.text, true);
  textR(REND, r, gs.u != null ? pct(gs.u / 1000) : 'n/a', C.text, true);
  text(RX, r + 1, gin.name || 'n/a', C.dim);
  if (!G.vram) { // compact: one more GPU
    const used = glyphs(gin.name || 'n/a') + 1 + glyphs(gs.t != null ? Math.round(gs.t) + '°C' : 'n/a');
    const pw = gs.p != null ? ' · ' + Math.round(gs.p) + ' W' : '', ck = gs.ck != null ? ' · ' + gs.ck + ' MHz' : '';
    let extra = fit(used, pw + ck, pw, ck);
    if (gs.t != null && gs.t >= ALERT.gpuTempC) { badgeR(REND - glyphs(extra), r + 1, Math.round(gs.t) + '°C'); textR(REND, r + 1, extra, C.dim); }
    else textR(REND, r + 1, (gs.t != null ? Math.round(gs.t) + '°C' : 'n/a') + extra, C.dim);
    text(RX, r + 2, gin.shared ? 'gpu mem' : 'vram', C.dim);
    textR(REND, r + 2, pair(num(gs.m), gin.memTotal || NaN), C.dim);
    return;
  }
  gpuTemp(gs, REND, r + 1);
  if (gs.p != null) {
    text(RX, r + 2, gin.shared ? 'package' : 'power', C.dim); // an APU's PPT covers CPU + GPU
    textR(REND, r + 2, Math.round(gs.p) + ' W' + (gs.ps != null ? ' · P' + gs.ps : '') + (gpuFan(gs) ? ' · fan ' + gpuFan(gs) : ''), C.dim);
  }
  text(RX, r + 3, 'clock', C.dim);
  textR(REND, r + 3, gpuClocks(gs), C.dim);
  if (gs.tx != null && gs.rx != null) {
    text(RX, r + 4, 'pcie', C.dim);
    textR(REND, r + 4, fit(4, `▲ ${rate(gs.tx)} · ▼ ${rate(gs.rx)}`, `▲ ${rate(gs.tx)} ▼ ${rate(gs.rx)}`), C.dim);
  }
  const v = G.vram - 1, tot = gin.memTotal;
  text(RX, v, gin.shared ? 'GPU mem' : 'VRAM', C.text, true);
  textR(REND, v, pair(num(gs.m), tot > 0 ? tot : NaN), C.text);
  bar(RX, REND, v + 1, tot > 0 && gs.m != null ? [[gs.m / tot, C.fill]] : []);
  const inUse = tot > 0 && gs.m != null ? pct(gs.m / tot) + ' in use' : 'n/a';
  text(RX, v + 2, inUse, C.dim);
}

// Fan as a percentage where the driver gives one, else the speed (the Z13's firmware reports rpm only).
const gpuFan = gs => (gs.f != null ? gs.f + '%' : gs.fr != null ? gs.fr + ' rpm' : '');

// "core 1575 · mem 7601 MHz", or whichever of the two the GPU reports.
function gpuClocks(gs) {
  const c = gs.ck != null, k = gs.mk != null;
  if (c && k) return fit(5, `core ${gs.ck} · mem ${gs.mk} MHz`, `${gs.ck} / ${gs.mk} MHz`);
  if (c) return `core ${gs.ck} MHz`;
  if (k) return `mem ${gs.mk} MHz`;
  return 'n/a';
}

function drawMem(info, s, r) {
  const tot = info.memTotal, used = num(s.mu), cached = num(s.mc), free = num(s.mf);
  const uf = used / tot;
  let c = RX + 3;
  text(RX, r, 'MEM', C.text, true);
  if (uf * 100 >= ALERT.memPct) badge(c + 1, r, pct(uf));
  textR(REND, r, pair(used, tot > 0 ? tot : NaN), C.text);
  const cf = isFinite(cached) ? cached / tot : 0, ff = isFinite(free) ? free / tot : NaN;
  const other = isFinite(ff) ? Math.max(0, 1 - uf - cf - ff) : 0;
  bar(RX, REND, r + 1, [[uf, uf * 100 >= ALERT.memPct ? C.alert : C.fill], [cf, C.cached], [other, C.other]]);
  const a = 'used ' + pct(uf), z = isFinite(ff) ? 'free ' + pct(ff) : '';
  text(RX, r + 2, a, C.dim);
  textR(REND, r + 2, z, C.dim);
  if (isFinite(cached)) {
    const b = 'cached ' + pct(cf), l = RX + glyphs(a), rr = REND - glyphs(z);
    text(l + Math.floor((rr - l - glyphs(b)) / 2), r + 2, b, C.dim);
  }
  const label = info.swapLabel || 'Swap', st = num(s.st), su = num(s.su);
  const sf = su / st, alert = st > 0 && sf * 100 >= ALERT.swapPct;
  if (alert) { text(RX, r + 3, label, C.text, true); badge(RX + glyphs(label) + 1, r + 3, pct(sf)); }
  else text(RX, r + 3, label, C.dim);
  textR(REND, r + 3, st === 0 ? 'none' : pair(su, st), C.text);
  if (s.cu != null && s.cl != null) {
    text(RX, r + 4, 'commit', C.dim);
    textR(REND, r + 4, pair(s.cu, s.cl) + ' · ' + pct(s.cu / s.cl), C.dim);
  } else if (st > 0) {
    bar(RX, REND, r + 4, [[sf, alert ? C.alert : C.fill]]);
  }
}

function drawDisk(info, s, a, r) {
  const d = info.disk || {}, label = 'DISK ' + (d.label || '');
  const used = num(s.du), tot = num(s.dt), f = used / tot;
  const alert = f * 100 >= ALERT.diskPct;
  text(RX, r, label, C.text, true);
  if (alert) badge(RX + glyphs(label) + 1, r, pct(f));
  textR(REND, r, isFinite(f) ? `${pct(f)} of ${size(tot)}` : 'n/a', C.text);
  bar(RX, REND, r + 1, isFinite(f) ? [[f, alert ? C.alert : C.fill]] : []);
  const model = [shortModel(d.model), d.bus].filter(Boolean).join(' · ');
  text(RX, r + 2, model, C.dim);
  text(RX, r + 3, '▼', C.down); text(RX + 2, r + 3, 'read', C.text); textR(REND, r + 3, rate(num(s.dr)), C.text, true);
  text(RX, r + 4, '▲', C.up); text(RX + 2, r + 4, 'write', C.text); textR(REND, r + 4, rate(num(s.dw)), C.text, true);
  text(RX, r + 5, 'peak', C.dim);
  textR(REND, r + 5, fit(4, `▼ ${rate(a.peakDr)} · ▲ ${rate(a.peakDw)}`, `▼ ${rate(a.peakDr)} ▲ ${rate(a.peakDw)}`), C.dim);
  text(RX, r + 6, 'scale', C.dim);
  textR(REND, r + 6, 'log, bytes per second', C.dim);
}

function drawNet(info, s, a, r, rows) {
  const n = info.net || {};
  text(RX, r, 'NET', C.text, true);
  textR(REND, r, n.name || 'n/a', C.text);
  text(RX, r + 1, n.addr || '', C.dim);
  textR(REND, r + 1, n.link ? link(n.link) : n.kind || '', C.dim);
  const dl = (row) => { text(RX, row, '▼', C.down); text(RX + 2, row, 'download', C.text); textR(REND, row, rate(num(s.nr)), C.text, true); };
  const ul = (row) => { text(RX, row, '▲', C.up); text(RX + 2, row, 'upload', C.text); textR(REND, row, rate(num(s.nt)), C.text, true); };
  if (rows >= 12) {
    dl(r + 3);
    text(RX, r + 4, 'peak', C.dim); textR(REND, r + 4, rate(a.peakRx), C.dim);
    text(RX, r + 5, 'total', C.dim); textR(REND, r + 5, size(num(s.nrt)), C.dim);
    ul(r + 7);
    text(RX, r + 8, 'peak', C.dim); textR(REND, r + 8, rate(a.peakTx), C.dim);
    text(RX, r + 9, 'total', C.dim); textR(REND, r + 9, size(num(s.ntt)), C.dim);
    text(RX, r + 11, 'scale', C.dim); textR(REND, r + 11, 'log, bytes per second', C.dim);
  } else if (rows >= 8) {
    dl(r + 3);
    ul(r + 4);
    text(RX, r + 5, 'peak', C.dim); textR(REND, r + 5, fit(4, `▼ ${rate(a.peakRx)} · ▲ ${rate(a.peakTx)}`, `▼ ${rate(a.peakRx)} ▲ ${rate(a.peakTx)}`), C.dim);
    text(RX, r + 7, 'scale', C.dim); textR(REND, r + 7, 'log, bytes per second', C.dim);
  } else {
    dl(r + 2);
    ul(r + 3);
  }
}

// ---- Frame -----------------------------------------------------------------------------------
function draw() {
  if (layoutDirty) layout();
  const now = hubNow();
  ctx.fillStyle = C.page;
  ctx.fillRect(0, 0, cv.width, cv.height);
  ctx.textBaseline = 'alphabetic';
  curFont = '';
  const list = order.map(id => machines.get(id)).filter(Boolean);
  if (!list.length) {
    ox = 0; oyRows = 0;
    text(1, 1, 'btop hub', C.text, true);
    text(1, 3, 'No machines yet. Install an agent on each machine (see the README).', C.dim);
    return;
  }
  const shown = single ? [list[Math.min(idx, list.length - 1)]] : list;
  shown.forEach((m, k) => {
    ox = (k % grid.pc) * (COLS + GUTTER) * u;
    oyRows = Math.floor(k / grid.pc) * (ROWS + VGUTTER);
    if (sx(ox + COLS * u) < 0 || sx(ox) > cv.width || Y(ROWS) < 0 || Y(0) > cv.height) return; // zoomed out of view
    drawPane(m, now);
  });
  if (single) { // dots in the bottom margin row: there are more machines
    ox = 0; oyRows = 0;
    const n = list.length, x0 = sx(COLS * u / 2) - (2 * n - 1) * cellPx() / 2;
    for (let k = 0; k < n; k++) textAt(x0 + 2 * k * cellPx(), ROWS - 1, '■', k === idx ? C.text : C.rule);
  }
}

let timer = 0, running = false, generation = 0;
function tick() {
  if (!running) return;
  try { draw(); } catch (e) { console.error(e); }
  try { renderDetail(); } catch (e) { console.error(e); }
  const now = hubNow();
  timer = setTimeout(tick, 1000 - (now % 1000) + 40);
}
async function start() {
  if (running) return;
  running = true;
  const gen = ++generation;
  try { await loadHistory(); } catch (e) { console.error(e); }
  if (!running || gen !== generation) return; // hidden (or hidden and shown again) while loading
  openStream();
  tick();
}
function stop() {
  running = false;
  clearTimeout(timer);
  if (es) { es.close(); es = null; }
  touches.clear(); pinch = null; pan = null; down = null;
}

document.addEventListener('visibilitychange', () => (document.hidden ? stop() : start()));
const redraw = () => { layoutDirty = true; if (running) draw(); };
// Read-only view of the layout decisions, for checking the page from a browser console.
window.btophubState = () => ({
  single, roomy, focus, idx, grid: { ...grid }, order: [...order],
  cell: { u: +u.toFixed(3), text: +wt.toFixed(3), graph: +wg.toFixed(3), h: +h.toFixed(3) },
  origin: { x: cx0, y: cy0 }, zoom: { s: +zs.toFixed(3), x: Math.round(ztx), y: Math.round(zty) },
  detail: detail ? { id: detail.id, kind: detail.kind, g: detail.g, span: detailSpan } : null,
});
const unzoom = () => { zs = 1; ztx = 0; zty = 0; };
window.addEventListener('resize', () => { unzoom(); redraw(); renderDetail(); });

// One machine at a time: step through them, keeping a chosen machine chosen.
function step(d) {
  const n = order.length;
  if (!single || n < 2) return;
  idx = (idx + d + n) % n;
  if (focus !== null) focus = order[idx];
  unzoom();
  redraw();
}
// Show one machine alone (k >= 0), or all of them again (k < 0).
function choose(k) {
  focus = k >= 0 && k < order.length && order.length > 1 ? order[k] : null;
  if (focus !== null) idx = k;
  unzoom();
  redraw();
}
// Canvas pixel under a point in client coordinates (null if the canvas has no size).
function canvasPoint(clientX, clientY) {
  const r = cv.getBoundingClientRect();
  if (!r.width || !r.height) return null;
  return [(clientX - r.left) * cv.width / r.width, (clientY - r.top) * cv.height / r.height];
}
// Which machine and which part of its pane is under a point (client coordinates), or null:
// {m, kind, g}, kind being 'header', 'cpu', 'gpu' (g says which), 'mem', 'disk', 'net' or null.
function hitAt(clientX, clientY) {
  const p = canvasPoint(clientX, clientY);
  if (!p) return null;
  const x = (p[0] - ztx) / zs - cx0, y = (p[1] - zty) / zs - cy0; // unzoomed, from the panes' corner
  const pw = (COLS + GUTTER) * u, pcol = Math.floor(x / pw), row = Math.floor(y / h);
  if (x < 0 || row < 0 || pcol >= grid.pc || x - pcol * pw >= COLS * u) return null;
  const r = row % (ROWS + VGUTTER);
  if (r >= ROWS) return null;
  const list = order.map(id => machines.get(id)).filter(Boolean);
  const m = single ? list[Math.min(idx, list.length - 1)] : list[Math.floor(row / (ROWS + VGUTTER)) * grid.pc + pcol];
  return m ? { m, ...partAt(m, r) } : null;
}
// The part of a pane at row r: the whole width of a section, graph and readout alike.
function partAt(m, r) {
  if (r < 3) return { kind: 'header' };
  if (r < 21) return { kind: 'cpu' };
  const L = paneRows(m);
  for (const G of L.gpus) if (r >= G.top && r < G.top + (G.vram ? 12 : 4)) return { kind: 'gpu', g: G.g };
  if (r >= L.mem && r < L.mem + 6) return { kind: 'mem' };
  if (r >= L.disk && r < L.disk + 8) return { kind: 'disk' };
  if (r >= L.net && r < 62) return { kind: 'net' };
  return { kind: null };
}

// ---- Detail overlay ---------------------------------------------------------------------------
// A tap or click on part of a pane (CPU, a GPU, memory, disk, network) opens everything the agent
// reports for that device: its values now, the average and peak over the last 10 minutes or
// hour, and charts of that span at full resolution, one second per sample.
const detailEl = document.getElementById('detail');
const detailTitle = document.getElementById('detail-title');
const detailSub = document.getElementById('detail-sub');
const detailTable = document.getElementById('detail-table');
const detailThreads = document.getElementById('detail-threads');
const detailChart = document.getElementById('detail-chart');
const detailClose = document.getElementById('detail-close');
const spanButtons = [...detailEl.querySelectorAll('[data-span]')];
let detail = null;    // {id, kind, g, opened}: the device shown, or null
let detailSpan = 600; // seconds of history shown
const spanText = () => (detailSpan >= 3600 ? '1 hour' : detailSpan / 60 + ' min');

function openDetail(id, kind, g) {
  detail = { id, kind, g: g || 0, opened: performance.now() };
  detailEl.hidden = false;
  renderDetail();
  detailClose.focus({ preventScroll: true });
}
function closeDetail() {
  if (!detail) return false;
  detail = null;
  detailEl.hidden = true;
  if (detailEl.contains(document.activeElement)) document.activeElement.blur();
  return true;
}
detailClose.addEventListener('click', closeDetail);
// A tap outside the panel closes it, but not the second tap of a double tap that opened it.
detailEl.addEventListener('click', e => {
  if (e.target === detailEl && detail && performance.now() - detail.opened > 500) closeDetail();
});
for (const b of spanButtons) b.addEventListener('click', () => { detailSpan = Number(b.dataset.span); renderDetail(); });

// Index of the first sample at or after time `from`.
function firstAt(m, from) {
  let lo = 0, hi = m.n;
  while (lo < hi) { const mid = (lo + hi) >> 1; if (m.t[mid] < from) lo = mid + 1; else hi = mid; }
  return lo;
}
// Mean, peak and sum of a per-sample value from sample i0 on, missing samples skipped.
function over(m, i0, get) {
  let n = 0, sum = 0, max = -Infinity;
  for (let i = i0; i < m.n; i++) { const v = get(i); if (v === v) { n++; sum += v; if (v > max) max = v; } }
  return n ? { avg: sum / n, max, sum, n } : { avg: NaN, max: NaN, sum: NaN, n: 0 };
}
const degC = v => (isFinite(v) ? Math.round(v) + '°C' : 'n/a');
const watts = v => (isFinite(v) ? Math.round(v) + ' W' : 'n/a');
const mhz = v => (isFinite(v) ? Math.round(v) + ' MHz' : 'n/a');
const ghz = v => (isFinite(v) ? freq(v) : 'n/a');
const ofTotal = (v, total) => (isFinite(v) ? size(v) + (total > 0 ? ' · ' + pct(v / total) : '') : 'n/a');

// What each kind of device shows: table rows [label, now, avg, peak] (rows with no value at all
// are left out), chart strips, and for the CPU every thread now plus a per-thread heat map.
const DETAIL = {
  cpu(m, info, s, i0) {
    const T = m.T;
    const avgLoad = i => {
      if (!T || m.cpu[i * T] === 0xffff) return NaN;
      let a = 0;
      for (let k = 0; k < T; k++) a += m.cpu[i * T + k];
      return a / T / 1000;
    };
    const load = over(m, i0, avgLoad), fq = over(m, i0, i => m.cf[i]), tp = over(m, i0, i => m.ct[i]);
    let top = NaN, topK = -1; // the busiest any thread got
    for (let i = i0; i < m.n; i++) {
      if (!T || m.cpu[i * T] === 0xffff) continue;
      for (let k = 0; k < T; k++) { const v = m.cpu[i * T + k] / 1000; if (!(v <= top)) { top = v; topK = k; } }
    }
    const loads = s.c && s.c.length === T && T ? s.c.map(v => v / 1000) : null;
    let busy = 0;
    if (loads) loads.forEach((v, k) => { if (v > loads[busy]) busy = k; });
    const rows = [
      ['load, all threads', pct(loads ? loads.reduce((a, b) => a + b, 0) / T : NaN), pct(load.avg), pct(load.max)],
      ['busiest thread', loads ? `C${busy} ${pct(loads[busy])}` : 'n/a', '', topK >= 0 ? `C${topK} ${pct(top)}` : 'n/a'],
      ['threads under 5%', loads ? `${loads.filter(v => v < 0.05).length} of ${T}` : 'n/a', '', ''],
      ['clock', ghz(num(s.cf)), ghz(fq.avg), ghz(fq.max)],
      info.hasTemp ? ['temperature', degC(num(s.ct)), degC(tp.avg), degC(tp.max)] : ['temperature', 'no sensor', '', ''],
    ];
    if (s.la && s.la.length === 3) rows.push(['load average, 1 5 15 min', s.la.map(v => v.toFixed(2)).join('  '), '', '']);
    const strips = [{ label: 'load, all threads', scale: 'pct', ramp: 'load', get: avgLoad, fmt: pct }];
    if (fq.n) strips.push({ label: 'clock', scale: 'zero', ramp: 'load', get: i => m.cf[i], fmt: ghz });
    if (tp.n) strips.push({ label: 'temperature', scale: 'range', ramp: 'load', get: i => m.ct[i], fmt: degC });
    return {
      title: 'CPU',
      sub: [info.cpu, [info.cores ? info.cores + ' cores' : '', T ? T + ' threads' : ''].filter(Boolean).join(', ')].filter(Boolean).join(' · '),
      rows, strips, threads: loads, heat: T ? { T } : null,
    };
  },
  gpu(m, info, s, i0, g) {
    const gin = (info.gpus || [])[g] || {}, gs = (s.g && s.g[g]) || {};
    const tot = gin.memTotal > 0 ? gin.memTotal : NaN;
    const memLabel = gin.shared ? 'GPU memory used' : 'VRAM used';
    const powerLabel = gin.shared ? 'package power (CPU + GPU)' : 'power';
    const ut = over(m, i0, i => m.gu[g][i]), vm = over(m, i0, i => m.gm[g][i]), tp = over(m, i0, i => m.gt[g][i]);
    const pw = over(m, i0, i => m.gp[g][i]), ck = over(m, i0, i => m.gck[g][i]), mk = over(m, i0, i => m.gmk[g][i]);
    const tx = over(m, i0, i => m.gtx[g][i]), rx = over(m, i0, i => m.grx[g][i]);
    const rows = [
      ['utilisation', pct(gs.u != null ? gs.u / 1000 : NaN), pct(ut.avg), pct(ut.max)],
      [memLabel, ofTotal(num(gs.m), tot), ofTotal(vm.avg, tot), ofTotal(vm.max, tot)],
      ['temperature', degC(num(gs.t)), degC(tp.avg), degC(tp.max)],
      [powerLabel, watts(num(gs.p)), watts(pw.avg), watts(pw.max)],
      ['core clock', mhz(num(gs.ck)), mhz(ck.avg), mhz(ck.max)],
      ['memory clock', mhz(num(gs.mk)), mhz(mk.avg), mhz(mk.max)],
      ['performance state', gs.ps != null ? 'P' + gs.ps : 'n/a', '', ''],
      ['fan', gpuFan(gs) || 'n/a', '', ''],
      ['PCIe ▲ tx', rate(num(gs.tx)), rate(tx.avg), rate(tx.max)],
      ['PCIe ▼ rx', rate(num(gs.rx)), rate(rx.avg), rate(rx.max)],
    ];
    const strips = [{ label: 'utilisation', scale: 'pct', ramp: 'load', get: i => m.gu[g][i], fmt: pct }];
    if (vm.n) strips.push(tot > 0
      ? { label: memLabel, scale: 'pct', flat: C.hist, get: i => m.gm[g][i] / tot, fmt: pct }
      : { label: memLabel, scale: 'zero', flat: C.hist, get: i => m.gm[g][i], fmt: size });
    if (tp.n) strips.push({ label: 'temperature', scale: 'range', ramp: 'load', get: i => m.gt[g][i], fmt: degC });
    if (pw.n) strips.push({ label: powerLabel, scale: 'zero', ramp: 'load', get: i => m.gp[g][i], fmt: watts });
    if (ck.n) strips.push({ label: 'core clock', scale: 'zero', ramp: 'load', get: i => m.gck[g][i], fmt: mhz });
    if (mk.n) strips.push({ label: 'memory clock', scale: 'zero', ramp: 'load', get: i => m.gmk[g][i], fmt: mhz });
    const vendor = { nvidia: 'NVIDIA', amd: 'AMD', intel: 'Intel' }[gin.vendor] || '';
    return {
      title: (m.G > 1 ? 'GPU ' + g : 'GPU') + (gin.name ? ' · ' + gin.name : ''),
      sub: [vendor, gin.integrated ? 'integrated' : '', tot > 0 ? size(tot) + (gin.shared ? ' GPU memory (VRAM + GTT)' : ' VRAM') : ''].filter(Boolean).join(' · '),
      rows, strips,
    };
  },
  mem(m, info, s, i0) {
    const tot = info.memTotal > 0 ? info.memTotal : NaN, label = info.swapLabel || 'Swap';
    const st = num(s.st), cl = num(s.cl);
    const mu = over(m, i0, i => m.mu[i]), mc = over(m, i0, i => m.mc[i]), su = over(m, i0, i => m.su[i]), cu = over(m, i0, i => m.cu[i]);
    const rows = [
      ['used', ofTotal(num(s.mu), tot), ofTotal(mu.avg, tot), ofTotal(mu.max, tot)],
      ['cached', ofTotal(num(s.mc), tot), ofTotal(mc.avg, tot), ofTotal(mc.max, tot)],
      ['free', ofTotal(num(s.mf), tot), '', ''],
      st === 0 ? [label, 'none', '', ''] : [label, ofTotal(num(s.su), st), ofTotal(su.avg, st), ofTotal(su.max, st)],
      [label + ' size', st > 0 ? size(st) : 'n/a', '', ''],
      ['commit', ofTotal(num(s.cu), cl), ofTotal(cu.avg, cl), ofTotal(cu.max, cl)],
      ['commit limit', cl > 0 ? size(cl) : 'n/a', '', ''],
    ];
    const strips = [];
    if (tot > 0 && mu.n) strips.push({ label: 'used', scale: 'pct', flat: C.hist, get: i => m.mu[i] / tot, fmt: pct });
    if (tot > 0 && mc.n) strips.push({ label: 'cached', scale: 'pct', flat: C.cached, get: i => m.mc[i] / tot, fmt: pct });
    if (st > 0 && su.n) strips.push({ label, scale: 'pct', flat: C.hist, get: i => m.su[i] / st, fmt: pct });
    if (cl > 0 && cu.n) strips.push({ label: 'commit', scale: 'pct', flat: C.hist, get: i => m.cu[i] / cl, fmt: pct });
    return { title: 'Memory', sub: tot > 0 ? size(tot) + ' total' : '', rows, strips };
  },
  disk(m, info, s, i0) {
    const d = info.disk || {}, du = num(s.du), dt = num(s.dt);
    const dr = over(m, i0, i => m.dr[i]), dw = over(m, i0, i => m.dw[i]);
    const rows = [
      ['space used', dt > 0 && isFinite(du) ? `${size(du)} of ${size(dt)} · ${pct(du / dt)}` : 'n/a', '', ''],
      ['space free', dt > 0 && isFinite(du) ? size(dt - du) : 'n/a', '', ''],
      ['read', rate(num(s.dr)), rate(dr.avg), rate(dr.max)],
      ['write', rate(num(s.dw)), rate(dw.avg), rate(dw.max)],
      ['read in the last ' + spanText(), dr.n ? size(dr.sum) : 'n/a', '', ''],
      ['written in the last ' + spanText(), dw.n ? size(dw.sum) : 'n/a', '', ''],
    ];
    const strips = [
      { label: 'read', scale: 'log', ramp: 'down', get: i => m.dr[i], fmt: rate },
      { label: 'write', scale: 'log', ramp: 'up', get: i => m.dw[i], fmt: rate },
    ];
    const sub = [d.model, d.bus, 'space is the system volume, rates all physical disks'].filter(Boolean).join(' · ');
    return { title: 'Disk ' + (d.label || ''), sub, rows, strips };
  },
  net(m, info, s, i0) {
    const n = info.net || {};
    const nr = over(m, i0, i => m.nr[i]), nt = over(m, i0, i => m.nt[i]);
    const rows = [
      ['▼ download', rate(num(s.nr)), rate(nr.avg), rate(nr.max)],
      ['▲ upload', rate(num(s.nt)), rate(nt.avg), rate(nt.max)],
      ['received in the last ' + spanText(), nr.n ? size(nr.sum) : 'n/a', '', ''],
      ['sent in the last ' + spanText(), nt.n ? size(nt.sum) : 'n/a', '', ''],
      ['total received', size(num(s.nrt)), '', ''],
      ['total sent', size(num(s.ntt)), '', ''],
    ];
    const strips = [
      { label: 'download', scale: 'log', ramp: 'down', get: i => m.nr[i], fmt: rate },
      { label: 'upload', scale: 'log', ramp: 'up', get: i => m.nt[i], fmt: rate },
    ];
    const sub = [n.desc, n.kind, n.addr, n.link ? link(n.link).replace('link ', '') + ' link' : ''].filter(Boolean).join(' · ');
    return { title: 'Network ' + (n.name || ''), sub, rows, strips };
  },
};

function renderDetail() {
  if (!detail) return;
  const m = machines.get(detail.id);
  if (!m || (detail.kind === 'gpu' && detail.g >= m.G)) { closeDetail(); return; }
  const now = hubNow(), from = now - detailSpan * 1000, i0 = firstAt(m, from);
  const spec = DETAIL[detail.kind](m, m.info || {}, m.last || {}, i0, detail.g);
  const offline = !m.lastT || now - m.lastT > OFFLINE_MS;
  detailTitle.textContent = m.name + ' · ' + spec.title;
  detailSub.textContent = [spec.sub, offline ? (m.lastT ? 'offline, last seen ' + clock(m.lastT) : 'offline') : '']
    .filter(Boolean).join(' · ');
  for (const b of spanButtons) b.setAttribute('aria-pressed', String(Number(b.dataset.span) === detailSpan));
  const KEYS = ['', 'now', 'average', 'peak']; // each value cell says what it is, for narrow screens
  const tr = (cells, tag) => {
    const r = document.createElement('tr');
    cells.forEach((c, k) => {
      const e = document.createElement(tag);
      e.textContent = c;
      if (tag === 'td' && k && c) e.dataset.k = KEYS[k];
      r.append(e);
    });
    return r;
  };
  const rows = spec.rows.filter(r => r.slice(1).some(v => v && v !== 'n/a'));
  detailTable.replaceChildren(tr(['', 'now', 'average', 'peak'], 'th'), ...rows.map(r => tr(r, 'td')));
  detailThreads.replaceChildren(...(spec.threads || []).map((v, k) => {
    const d = document.createElement('div'), a = document.createElement('span'), bar = document.createElement('i'), b = document.createElement('b');
    a.textContent = 'C' + k;
    bar.style.setProperty('--v', String(Math.min(1, v)));
    bar.style.setProperty('--c', RAMP.load[level(v)]);
    b.textContent = pct(v);
    d.append(a, bar, b);
    return d;
  }));
  drawDetailChart(m, spec, from, now);
}

// Round up to a round number: 1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6 or 8 times a power of ten.
function niceUp(x) {
  if (!(x > 0)) return 1;
  const p = Math.pow(10, Math.floor(Math.log10(x)));
  for (const f of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8]) if (x <= f * p + 1e-9) return f * p;
  return 10 * p;
}
// The overlay's chart: the CPU heat map (one row per thread) and one strip per series, over the
// chosen span on a linear time axis. Each column is the mean (band) and peak (veil and cap) of the
// samples it covers; columns without samples stay blank.
function drawDetailChart(m, spec, from, now) {
  const dpr = window.devicePixelRatio || 1, cssW = detailChart.clientWidth;
  if (!cssW) return;
  const LABEL = 20, STRIP = 48, GAP = 8, AXIS = 20; // CSS pixels
  const blocks = [];
  if (spec.heat) blocks.push({ label: 'each thread, 0 at the top', heat: true, h: Math.min(192, Math.max(96, spec.heat.T * 4)) });
  for (const st of spec.strips) blocks.push({ label: st.label, st, h: STRIP });
  const cssH = blocks.reduce((a, b) => a + LABEL + b.h + GAP, 0) + AXIS;
  detailChart.style.height = cssH + 'px';
  const W = Math.round(cssW * dpr), H = Math.round(cssH * dpr);
  if (detailChart.width !== W || detailChart.height !== H) { detailChart.width = W; detailChart.height = H; }
  const g = detailChart.getContext('2d', { alpha: false });
  g.fillStyle = C.page;
  g.fillRect(0, 0, W, H);
  g.font = `400 ${12 * dpr}px JBM`;
  g.textBaseline = 'alphabetic';
  // Columns: one per device pixel, at most one per second; column c holds samples ci[c] .. ci[c+1].
  const ncol = Math.max(1, Math.min(W, detailSpan)), ci = new Int32Array(ncol + 1);
  for (let c = 0, i = firstAt(m, from); c <= ncol; c++) {
    const tc = from + (now - from) * c / ncol;
    while (i < m.n && m.t[i] < tc) i++;
    ci[c] = i;
  }
  ci[ncol] = m.n;
  const xAt = c => Math.round(c * W / ncol);
  const step = detailSpan > 600 ? 900 : 120; // a guide every 15 or 2 minutes
  const lw = Math.max(1, Math.round(dpr));
  let y = 0;
  for (const b of blocks) {
    const top = Math.round((y + LABEL) * dpr), bh = Math.round(b.h * dpr);
    g.fillStyle = C.dim;
    g.fillText(b.label, 0, Math.round((y + 14) * dpr));
    g.fillStyle = C.strip;
    g.fillRect(0, top, W, bh);
    let range = '';
    if (b.heat) drawHeatCols(g, m, spec.heat.T, ci, ncol, xAt, top, bh);
    else range = drawStripCols(g, b.st, ci, ncol, xAt, top, bh, lw);
    g.fillStyle = C.dim;
    if (range) g.fillText(range, W - g.measureText(range).width, Math.round((y + 14) * dpr));
    g.fillStyle = C.guide;
    for (let t = step; t < detailSpan; t += step) g.fillRect(Math.round(W * (1 - t / detailSpan)), top, lw, bh);
    y += LABEL + b.h + GAP;
  }
  g.fillStyle = C.dim;
  for (let t = 0; t <= detailSpan; t += step) {
    const s = t ? '-' + t / 60 + 'm' : 'now', tw = g.measureText(s).width;
    const x = W * (1 - t / detailSpan);
    g.fillText(s, Math.round(Math.min(W - tw, Math.max(0, x - tw / 2))), Math.round((y + 14) * dpr));
  }
}
function drawHeatCols(g, m, T, ci, ncol, xAt, top, bh) {
  for (let c = 0; c < ncol; c++) {
    const i0 = ci[c], i1 = ci[c + 1];
    if (i1 <= i0) continue;
    const x0 = xAt(c), x1 = Math.max(x0 + 1, xAt(c + 1));
    for (let k = 0; k < T; k++) {
      let n = 0, sum = 0, max = 0;
      for (let i = i0; i < i1; i++) { const v = m.cpu[i * T + k]; if (v !== 0xffff) { n++; sum += v; if (v > max) max = v; } }
      if (!n) continue;
      g.fillStyle = RAMP.load[level((sum / n + max) / 2000)];
      const y0 = top + Math.round(k * bh / T);
      g.fillRect(x0, y0, x1 - x0, top + Math.round((k + 1) * bh / T) - y0);
    }
  }
}
// Draws one strip; returns the text describing its scale.
function drawStripCols(g, st, ci, ncol, xAt, top, bh, lw) {
  let norm, range, lo = 0, hi = 1;
  if (st.scale === 'pct') { norm = v => v; range = '0 to 100%'; }
  else if (st.scale === 'log') { norm = logFrac; range = 'log, 1 KiB/s to 1 GiB/s'; }
  else { // from the data in view: from zero, or a range in tens (temperatures)
    let mn = Infinity, mx = -Infinity;
    for (let i = ci[0]; i < ci[ncol]; i++) { const v = st.get(i); if (v === v) { if (v < mn) mn = v; if (v > mx) mx = v; } }
    if (!(mx >= mn)) { mn = 0; mx = 1; }
    if (st.scale === 'zero') hi = niceUp(mx);
    else { lo = Math.floor(mn / 10) * 10; hi = Math.max(lo + 10, Math.ceil(mx / 10) * 10); }
    norm = v => (v - lo) / (hi - lo);
    range = (lo ? st.fmt(lo) : '0') + ' to ' + st.fmt(hi);
  }
  for (let c = 0; c < ncol; c++) {
    let n = 0, sum = 0, max = -Infinity;
    for (let i = ci[c]; i < ci[c + 1]; i++) { const v = st.get(i); if (v === v) { n++; sum += v; if (v > max) max = v; } }
    if (!n) continue;
    const mean = Math.max(0, Math.min(1, norm(sum / n))), peak = Math.max(mean, Math.min(1, norm(max)));
    const x0 = xAt(c), w = Math.max(1, xAt(c + 1) - x0);
    const ym = top + bh - Math.max(1, Math.round(mean * bh)), yp = top + bh - Math.round(peak * bh);
    if (st.flat) { g.fillStyle = st.flat; g.fillRect(x0, ym, w, top + bh - ym); continue; }
    const L = Math.max(2, level(mean));
    if (yp < ym) {
      g.fillStyle = VEIL[st.ramp][L]; g.fillRect(x0, yp, w, ym - yp);
      g.fillStyle = CAP[st.ramp][L]; g.fillRect(x0, yp, w, lw);
    }
    g.fillStyle = RAMP[st.ramp][L];
    g.fillRect(x0, ym, w, top + bh - ym);
  }
  return range;
}

// ---- Full screen ---------------------------------------------------------------------------
const fsButton = document.getElementById('fs');
const fsPath = fsButton.querySelector('path');
const root = document.documentElement;
const fsElement = () => document.fullscreenElement || document.webkitFullscreenElement || null;
const fsAvailable = !!(document.fullscreenEnabled || document.webkitFullscreenEnabled);
function toggleFullscreen() {
  if (fsElement()) {
    (document.exitFullscreen || document.webkitExitFullscreen).call(document);
  } else {
    const req = root.requestFullscreen || root.webkitRequestFullscreen;
    const p = req && req.call(root, { navigationUI: 'hide' });
    if (p && p.catch) p.catch(() => {});
  }
}
function fsChanged() {
  const on = !!fsElement();
  if (on) armIdle(); // the cursor hides 2.5 s after entering full screen, even with no mouse movement
  else document.body.classList.remove('idle');
  fsButton.setAttribute('aria-label', on ? 'Exit full screen' : 'Full screen');
  fsButton.title = on ? 'Exit full screen (F)' : 'Full screen (F)';
  fsPath.setAttribute('d', on ? 'M6 1v5H1M10 1v5h5M10 15v-5h5M6 15v-5H1' : 'M1 6V1h5M10 1h5v5M15 10v5h-5M6 15H1v-5');
  redraw();
}
if (!fsAvailable) document.body.classList.add('nofs');
document.addEventListener('fullscreenchange', fsChanged);
document.addEventListener('webkitfullscreenchange', fsChanged);
fsButton.addEventListener('click', e => {
  e.stopPropagation();
  toggleFullscreen();
  if (e.detail) fsButton.blur(); // a mouse click or tap; keyboard activation (detail 0) keeps focus
});
// A press (any button, or one dragged off) must not leave focus, and so a visible focus ring, on
// the auto-hiding button; Tab still focuses it for keyboard users.
fsButton.addEventListener('mousedown', e => e.preventDefault());
// Holding Enter or Space on the focused button toggles once, like holding F.
fsButton.addEventListener('keydown', e => { if (e.repeat) e.preventDefault(); });

// The button shows while the pointer moves (or after a tap) and fades when idle; in full
// screen the cursor hides too, so a wall display stays clean.
let idleTimer = 0;
// (Re)start the idle clock: the button fades and, in full screen, the cursor hides.
function armIdle() {
  clearTimeout(idleTimer);
  idleTimer = setTimeout(() => {
    document.body.classList.remove('active');
    if (fsElement()) document.body.classList.add('idle');
  }, 2500);
}
function wake() {
  document.body.classList.add('active');
  document.body.classList.remove('idle');
  armIdle();
}
window.addEventListener('pointermove', e => { if (e.pointerType === 'mouse') wake(); });
// Touch and pen wake the controls on the tap's click anywhere on the page. The browser picks the
// click's target before this runs, while the hidden button still ignores pointers, so the tap
// that reveals the button can never also press it.
let lastPointerType = '';
window.addEventListener('pointerdown', e => { lastPointerType = e.pointerType; }, true);
window.addEventListener('click', e => {
  // click is a PointerEvent only in current browsers; older ones leave pointerType undefined.
  const type = e.pointerType === undefined ? lastPointerType : e.pointerType;
  if (type && type !== 'mouse') wake();
});

window.addEventListener('keydown', e => {
  if (e.altKey || e.ctrlKey || e.metaKey) return;
  if (detail) {
    if (e.key === 'Escape') closeDetail();
    else if ((e.key === 'f' || e.key === 'F') && fsAvailable && !e.repeat) toggleFullscreen();
    else return;
    e.preventDefault();
    return;
  }
  if (e.key === 'f' || e.key === 'F') { if (fsAvailable && !e.repeat) toggleFullscreen(); }
  else if (e.key === 'ArrowRight') step(1);
  else if (e.key === 'ArrowLeft') step(-1);
  else if (e.key === 'Escape' || e.key === '0') { if (!resetZoom()) choose(-1); }
  else if (e.key === '+' || e.key === '=') zoomAt(1.25, cv.width / 2, cv.height / 2);
  else if (e.key === '-' || e.key === '_') zoomAt(0.8, cv.width / 2, cv.height / 2);
  else if (e.key >= '1' && e.key <= '9') choose(Number(e.key) - 1);
  else return;
  e.preventDefault();
});

// Click or tap a pane to see that machine alone; again to see all. Swipe sideways to step
// through machines when they are shown one at a time. On touch, a tap while the controls are
// hidden only reveals them.
// Zoom: pinch with two fingers (or on a touchpad), Ctrl + wheel, or + and -. A zoomed view pans
// with one finger, a mouse drag or the wheel; a double tap or double click, pinching back out,
// Esc or 0 returns to the whole view. While zoomed, a single tap or click changes nothing.
let down = null;            // tap or swipe in progress
const touches = new Map();  // fingers on the canvas: pointerId -> {x, y}, client pixels
let pinch = null;           // two-finger zoom in progress
let pan = null;             // one finger or the mouse dragging a zoomed view
let lastTap = { t: 0, x: 0, y: 0 }, tapTimer = 0;
const canvasPerClient = () => { const r = cv.getBoundingClientRect(); return r.width ? cv.width / r.width : 1; };
function fingers() {
  const [a, b] = [...touches.values()];
  const pa = canvasPoint(a.x, a.y) || [0, 0], pb = canvasPoint(b.x, b.y) || [0, 0];
  return { mx: (pa[0] + pb[0]) / 2, my: (pa[1] + pb[1]) / 2, d: Math.hypot(pb[0] - pa[0], pb[1] - pa[1]) };
}
function startPinch() {
  const f = fingers();
  // Remember the unzoomed point under the fingers' midpoint; it stays under them, so moving both
  // fingers pans while spreading them zooms.
  pinch = { d0: Math.max(1, f.d), s0: zs, ux: (f.mx - ztx) / zs, uy: (f.my - zty) / zs };
  down = null; pan = null;
}
function movePinch() {
  const f = fingers();
  zs = Math.min(ZOOM_MAX, Math.max(1, pinch.s0 * f.d / pinch.d0));
  ztx = f.mx - pinch.ux * zs;
  zty = f.my - pinch.uy * zs;
  clampZoom();
  requestDraw();
}
// A finger lifts: the pinch ends, and a finger still down pans from here (it never taps).
function liftTouch(e) {
  if (!touches.delete(e.pointerId) || !pinch || touches.size >= 2) return;
  pinch = null;
  if (zs < 1.05) resetZoom();
  const rest = [...touches.entries()][0];
  pan = rest && zs > 1 ? { id: rest[0], x: rest[1].x, y: rest[1].y, tx: ztx, ty: zty, moved: true } : null;
  down = null;
}
cv.addEventListener('pointerdown', e => {
  if (e.pointerType === 'touch') {
    touches.set(e.pointerId, { x: e.clientX, y: e.clientY });
    if (touches.size === 2) { startPinch(); return; }
    if (touches.size > 2) return;
  }
  // Only the primary button of the first finger, pen or mouse starts a tap, swipe or pan; a
  // second finger (two-finger tap, pinch) cancels it, and right/middle clicks are left alone.
  down = e.isPrimary && e.button === 0
    ? { id: e.pointerId, x: e.clientX, y: e.clientY, awake: document.body.classList.contains('active') }
    : null;
  pan = down && zs > 1 ? { id: e.pointerId, x: e.clientX, y: e.clientY, tx: ztx, ty: zty, moved: false } : null;
  if (pan && e.pointerType === 'mouse') cv.setPointerCapture(e.pointerId);
});
window.addEventListener('pointerdown', e => { if (!e.isPrimary || e.target !== cv) down = null; });
cv.addEventListener('pointermove', e => {
  if (touches.has(e.pointerId)) touches.set(e.pointerId, { x: e.clientX, y: e.clientY });
  if (pinch) { if (touches.size >= 2) movePinch(); return; }
  if (!pan || e.pointerId !== pan.id) return;
  const dx = e.clientX - pan.x, dy = e.clientY - pan.y;
  if (!pan.moved && Math.hypot(dx, dy) < 8) return;
  pan.moved = true;
  const k = canvasPerClient();
  ztx = pan.tx + dx * k;
  zty = pan.ty + dy * k;
  clampZoom();
  requestDraw();
});
cv.addEventListener('pointerup', e => {
  const panned = !!(pan && pan.id === e.pointerId && pan.moved);
  if (pan && pan.id === e.pointerId) pan = null;
  liftTouch(e);
  if (!down || e.pointerId !== down.id) return;
  const dx = e.clientX - down.x, dy = e.clientY - down.y, awake = down.awake;
  down = null;
  if (panned) return;
  const still = Math.abs(dx) <= 10 && Math.abs(dy) <= 10;
  if (zs > 1) {
    if (!still) return;
    // Zoomed: a double tap or double click returns to the whole view, so a single one waits a
    // moment before opening details.
    const t = performance.now(), x = e.clientX, y = e.clientY;
    if (tapTimer && t - lastTap.t < 400 && Math.hypot(x - lastTap.x, y - lastTap.y) < 30) {
      clearTimeout(tapTimer); tapTimer = 0; resetZoom(); return;
    }
    clearTimeout(tapTimer);
    lastTap = { t, x, y };
    const hit = hitAt(x, y);
    tapTimer = setTimeout(() => { tapTimer = 0; if (hit && hit.kind && hit.kind !== 'header') openDetail(hit.m.id, hit.kind, hit.g); }, 400);
    return;
  }
  if (Math.abs(dx) > 40 && Math.abs(dx) > 1.5 * Math.abs(dy)) { step(dx < 0 ? 1 : -1); return; }
  if (!still) return;
  const hit = hitAt(e.clientX, e.clientY);
  if (!hit || !hit.kind) return;
  if (hit.kind !== 'header') { openDetail(hit.m.id, hit.kind, hit.g); return; }
  // The header row shows this machine alone, or all of them again. On touch, a tap there while
  // the controls are hidden only reveals them: the full-screen button sits over its right end.
  if (e.pointerType !== 'mouse' && !awake) return;
  if (focus !== null) choose(-1);
  else if (!single) choose(order.indexOf(hit.m.id));
});
cv.addEventListener('pointercancel', e => {
  if (pan && pan.id === e.pointerId) pan = null;
  liftTouch(e);
  down = null;
});
cv.addEventListener('wheel', e => {
  const unit = e.deltaMode === 1 ? 16 : e.deltaMode === 2 ? cv.height : 1; // lines or pages to pixels
  if (e.ctrlKey) { // Ctrl + wheel, and a touchpad pinch, which browsers report the same way
    e.preventDefault();
    const p = canvasPoint(e.clientX, e.clientY);
    if (p) zoomAt(Math.exp(-Math.max(-50, Math.min(50, e.deltaY * unit)) / 100), p[0], p[1]);
  } else if (zs > 1) { // the wheel or a two-finger scroll pans a zoomed view
    e.preventDefault();
    const k = canvasPerClient();
    ztx -= e.deltaX * unit * k;
    zty -= e.deltaY * unit * k;
    clampZoom();
    requestDraw();
  }
}, { passive: false });

Promise.all([document.fonts.load('400 16px JBM'), document.fonts.load('700 16px JBM')])
  .catch(() => {})
  .then(() => { if (!document.hidden) start(); });
})();
