"use strict";

const state = { version: "", stats: null, store: null, modules: [], neighbours: [], historyRange: "24h" };
const counters = new Map(); // element id -> last numeric value

const $ = (id) => document.getElementById(id);

function fmtBytes(n) {
  if (!isFinite(n)) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + " " + units[i];
}

function fmtNum(n) {
  if (!isFinite(n)) return "—";
  if (n >= 1e9) return (n / 1e9).toFixed(1) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(1) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return String(n);
}

function fmtUptime(sec) {
  if (sec < 60) return sec + "s";
  if (sec < 3600) return Math.floor(sec / 60) + "m " + (sec % 60) + "s";
  if (sec < 86400) return Math.floor(sec / 3600) + "h " + Math.floor((sec % 3600) / 60) + "m";
  return Math.floor(sec / 86400) + "d " + Math.floor((sec % 86400) / 3600) + "h";
}

function ago(iso) {
  const sec = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (sec < 60) return Math.floor(sec) + "s ago";
  if (sec < 3600) return Math.floor(sec / 60) + "m ago";
  return Math.floor(sec / 3600) + "h ago";
}

// Animate a numeric value from its previous display to the next.
function countUp(id, target, format) {
  const el = $(id);
  if (!el) return;
  const from = counters.get(id) ?? 0;
  counters.set(id, target);
  if (from === target) { el.textContent = format(target); return; }
  const t0 = performance.now();
  const dur = 550;
  function step(t) {
    const k = Math.min(1, (t - t0) / dur);
    const e = 1 - Math.pow(1 - k, 3);
    el.textContent = format(from + (target - from) * e);
    if (k < 1) requestAnimationFrame(step);
  }
  requestAnimationFrame(step);
}

function renderHeader(s) {
  $("version").textContent = state.version;
  $("uptime").textContent = "uptime " + fmtUptime(s.uptimeSec);
  renderWarn("persist-warn", s.persistErrors, (n) => n + " stats db write errors");
  renderWarn("drop-warn", s.droppedEvents, (n) => n + " events dropped");
}

// Surface recorder trouble the backend already exposes: shown only > 0.
function renderWarn(id, n, label) {
  const el = $(id);
  if (!el) return;
  if (n > 0) {
    el.textContent = label(n);
    el.classList.add("show");
  } else {
    el.classList.remove("show");
  }
}

function renderKPIs(s, store) {
  if (!s || !store) return;
  const L = s.lifetime;
  countUp("kpi-requests", L.requests, fmtNum);
  const neighbour = L.neighbourHits > 0 ? " · " + fmtNum(L.neighbourHits) + " neighbour" : "";
  $("kpi-requests-sub").textContent =
    fmtNum(L.hits) + " hits · " + fmtNum(L.misses) + " misses · " + fmtNum(L.passes) + " passes" + neighbour;

  const denom = L.hits + L.misses;
  const ratio = denom > 0 ? L.hits / denom : 0;
  countUp("kpi-ratio", Math.round(ratio * 100), (v) => v + "%");
  const C = 97.4;
  const ring = $("ring");
  ring.style.strokeDashoffset = C * (1 - ratio);
  ring.style.stroke = ratio >= 0.8 ? "#34d399" : ratio >= 0.5 ? "#22d3ee" : "#fbbf24";
  $("kpi-ratio-sub").textContent = L.verifications > 0
    ? fmtNum(L.verifications) + " verified fetches" : "no fetches yet";

  countUp("kpi-saved", L.bytesSaved, fmtBytes);
  $("kpi-saved-sub").textContent = L.bytesSaved > 0
    ? fmtBytes(L.bytesUp) + " pulled from upstream, ever" : "savings appear on cache hits";

  countUp("kpi-down", L.bytesDown, fmtBytes);
  $("kpi-down-sub").textContent = "session " + fmtBytes(s.session.bytesDown);
  countUp("kpi-up", L.bytesUp, fmtBytes);
  $("kpi-up-sub").textContent = "session " + fmtBytes(s.session.bytesUp);

  countUp("kpi-store", store.sizeBytes, fmtBytes);
  $("kpi-store-sub").textContent =
    fmtNum(store.modules) + " modules · " + fmtNum(store.versions) + " versions · " + fmtNum(store.files) + " files";

  countUp("kpi-neighbour", L.neighbourHits, fmtNum);
  $("kpi-neighbour-sub").textContent = neighbourSub(L, state.neighbours);
}

// Neighbour caches: how often a peer had the artifact, how much bandwidth
// that saved us from pulling from the upstream proxy, and how many peers
// the health probe currently sees online.
function neighbourSub(L, peers) {
  const hits = L.neighbourHits || 0;
  const misses = L.neighbourMisses || 0;
  let text = "no neighbour lookups yet";
  if (hits > 0 || misses > 0) {
    text = fmtBytes(L.bytesNeighbour || 0) + " from neighbours";
    if (misses > 0) text += " · " + fmtNum(misses) + " fell back upstream";
  }
  if (peers && peers.length) {
    const online = peers.filter((p) => p.online).length;
    text += " · " + online + "/" + peers.length + " peers online";
  }
  return text;
}

function shortMod(path) {
  const parts = path.split("/");
  return parts.length > 2 ? parts.slice(-2).join("/") : path;
}

// One module row, built once. Updates patch the cells in place so a
// pushed cache change never reloads or re-flashes the table.
function moduleRow(m) {
  const tr = document.createElement("tr");
  tr.dataset.path = m.path;
  tr.innerHTML = `<td><span class="path"></span></td>
    <td class="r"></td>
    <td class="r"></td>
    <td class="r"></td>
    <td class="r"></td>
    <td class="dim" data-time=""></td>
    <td></td>`;
  updateModuleRow(tr, m);
  return tr;
}

function updateModuleRow(tr, m) {
  const cells = tr.children;
  cells[0].firstChild.textContent = m.path;
  cells[1].textContent = m.versions;
  cells[2].textContent = fmtBytes(m.sizeBytes);
  cells[3].textContent = fmtNum(m.requests);
  cells[4].textContent = fmtBytes(m.servedBytes);
  const used = m.lastUsed && !m.lastUsed.startsWith("0001") ? m.lastUsed : "";
  cells[5].dataset.time = used;
  cells[5].textContent = used ? ago(used) : "never";
  const chip = cells[6].firstChild;
  if (chip) {
    chip.textContent = m.verified ? "verified" : "unverified";
    chip.className = "chip " + (m.verified ? "ok" : "warn");
  } else {
    cells[6].innerHTML = m.verified
      ? '<span class="chip ok">verified</span>'
      : '<span class="chip warn">unverified</span>';
  }
}

// Reconcile the table against the pushed cache state: existing rows keep
// their nodes and only their cells change, so nothing fades or reloads.
function renderModules(mods) {
  const body = $("modules-body");
  const top = mods.slice(0, 10);
  $("modules-empty").style.display = top.length ? "none" : "block";
  $("modules-hint").textContent = top.length ? "top " + top.length + " by bytes served" : "";

  const rows = new Map();
  for (const tr of Array.from(body.children)) rows.set(tr.dataset.path, tr);

  const keep = new Set();
  top.forEach((m, i) => {
    keep.add(m.path);
    let tr = rows.get(m.path);
    if (tr) {
      updateModuleRow(tr, m);
    } else {
      tr = moduleRow(m);
      rows.set(m.path, tr);
    }
    const at = body.children[i];
    if (at !== tr) body.insertBefore(tr, at || null);
  });
  for (const [path, tr] of rows) {
    if (!keep.has(path)) tr.remove();
  }
}

function eventChip(result) {
  switch (result) {
    case "hit": return '<span class="chip ok">hit</span>';
    case "miss": return '<span class="chip warn">miss</span>';
    case "pass": return '<span class="chip ok">pass</span>';
    case "neighbour": return '<span class="chip nb">neighbour</span>';
    case "verify-failed": return '<span class="chip bad">tampered</span>';
    case "error": return '<span class="chip bad">error</span>';
    case "upstream": return '<span class="chip bad">upstream</span>';
    default: return '<span class="chip ok">info</span>';
  }
}

function eventKey(ev) {
  return [ev.time, ev.module, ev.version, ev.kind, ev.result].join("|");
}

const eventKeys = new Set();

function eventItem(ev) {
  const target = ev.module
    ? escapeHTML(ev.module) + (ev.version ? " " + escapeHTML(ev.version) : "")
    : "goproxy";
  const detail = ev.detail
    ? escapeHTML(ev.detail)
    : (ev.kind || "") + (ev.bytes ? " · " + fmtBytes(ev.bytes) : "");
  return `<li data-key="${escapeHTML(eventKey(ev))}">
      ${eventChip(ev.result)}
      <div class="ev-main">
        <div class="ev-mod">${target}</div>
        <div class="ev-detail">${detail}</div>
      </div>
      <span class="ev-time" data-time="${escapeHTML(ev.time)}">${ago(ev.time)}</span>
    </li>`;
}

const MAX_EVENTS = 24;

function updateEventsMeta(count) {
  $("events-empty").style.display = count ? "none" : "block";
  $("events-hint").textContent = count ? "newest first · pushed live · " + count : "";
}

// Full render, used for the initial seed and after an SSE reconnect.
function renderEvents(events) {
  const ul = $("events");
  const shown = events.slice(0, MAX_EVENTS);
  eventKeys.clear();
  shown.forEach((ev) => eventKeys.add(eventKey(ev)));
  ul.innerHTML = shown.map(eventItem).join("");
  updateEventsMeta(ul.children.length);
}

// Live append: one new node at the top, existing nodes stay untouched so
// the feed never reloads or loses hover/focus while nothing changes. The
// seed and the first pushed events can overlap, hence the key check.
function appendEvent(ev) {
  const key = eventKey(ev);
  if (eventKeys.has(key)) return;
  const ul = $("events");
  ul.insertAdjacentHTML("afterbegin", eventItem(ev));
  eventKeys.add(key);
  while (ul.children.length > MAX_EVENTS) {
    const last = ul.lastElementChild;
    eventKeys.delete(last.dataset.key);
    ul.removeChild(last);
  }
  updateEventsMeta(ul.children.length);
}

function escapeHTML(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

// ---- 3D rotating bar chart: top modules by served bandwidth ----
const AGENDA_MAX = 8;

// Color by rank, not by rotation depth, so the fixed side agenda always
// matches the bars.
const BAR_HUES = [252, 199, 160, 43, 350, 285, 18, 187];

function barColor(i) {
  const hue = BAR_HUES[i % BAR_HUES.length];
  return {
    top: `hsl(${hue} 90% 64%)`,
    front: `hsl(${hue} 72% 46% / 0.94)`,
    side: `hsl(${hue} 68% 34% / 0.94)`,
    swatch: `hsl(${hue} 90% 60%)`,
  };
}

const chart3d = {
  canvas: null,
  ctx: null,
  rot: 0.6,
  data: [],
  key: "",
  w: 0,
  h: 0,
  highlight: -1,

  init() {
    this.canvas = $("chart3d");
    this.ctx = this.canvas.getContext("2d");
    const fit = () => {
      const dpr = window.devicePixelRatio || 1;
      const r = this.canvas.getBoundingClientRect();
      this.w = Math.max(50, r.width);
      this.h = Math.max(50, r.height);
      this.canvas.width = this.w * dpr;
      this.canvas.height = this.h * dpr;
      this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };
    new ResizeObserver(fit).observe(this.canvas);
    fit();
    const agenda = $("chart3d-agenda");
    if (agenda) {
      agenda.addEventListener("mouseover", (e) => {
        const li = e.target.closest("li");
        this.highlight = li ? Number(li.dataset.rank) : -1;
      });
      agenda.addEventListener("mouseleave", () => { this.highlight = -1; });
    }
    const loop = () => {
      // Rotate only while the tab is visible; redraw once when it returns.
      if (!document.hidden) this.rot += 0.0042;
      this.draw();
      requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
  },

  setData(mods) {
    const data = mods.slice(0, AGENDA_MAX).map((m) => ({
      label: shortMod(m.path),
      full: m.path,
      value: m.servedBytes,
    }));
    const key = data.map((d) => d.full + ":" + d.value).join("|");
    if (key === this.key) return;
    this.key = key;
    this.data = data;
    renderAgenda(data);
  },

  project(x, y, z) {
    const c = Math.cos(this.rot), s = Math.sin(this.rot);
    const X = x * c - z * s;
    const Z = x * s + z * c;
    const w = this.w, h = this.h;
    // Clamp the depth scale so wide stages still fill their width instead
    // of collapsing the bars into a small center cluster.
    const scale = Math.min(1.02, Math.max(0.74, 900 / (900 + Z + 320)));
    return { x: w / 2 + X * scale, y: h * 0.88 - y * scale, scale, z: Z };
  },

  draw() {
    const ctx = this.ctx;
    const w = this.w, h = this.h;
    ctx.clearRect(0, 0, w, h);

    const n = this.data.length;
    if (n === 0) {
      ctx.fillStyle = "#8291ad";
      ctx.font = "13px ui-sans-serif, system-ui";
      ctx.textAlign = "center";
      ctx.fillText("no traffic yet — bars appear as modules are served", w / 2, h / 2);
      return;
    }

    const maxV = Math.max(...this.data.map((d) => d.value), 1);
    // Fill the stage: spacing and bar width grow with the canvas, so wide
    // dashboards get wide bars instead of a small centered cluster.
    const spacing = n <= 1 ? 0 : Math.min(w * 0.34, (w * 0.9) / (n - 1));
    const bw = Math.min(64, Math.max(18, spacing * 0.42));
    const maxH = h * 0.74;
    const depth = n * spacing * 0.6 + 40;

    // Floor grid.
    ctx.strokeStyle = "rgba(148, 163, 255, 0.10)";
    ctx.lineWidth = 1;
    for (const gx of [-0.42, -0.21, 0, 0.21, 0.42]) {
      ctx.beginPath();
      const a = this.project(gx * w, 0, -depth);
      const b = this.project(gx * w, 0, depth);
      ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y); ctx.stroke();
    }

    const bars = this.data.map((d, i) => {
      const height = 18 + (d.value / maxV) * maxH;
      const x = (i - (n - 1) / 2) * spacing;
      return { ...d, i, x, height, depth: x * Math.sin(this.rot) };
    });
    bars.sort((a, b) => a.depth - b.depth);

    bars.forEach((b) => {
      const { top, front, side } = barColor(b.i);

      const c = [
        this.project(b.x - bw / 2, 0, -bw / 2), this.project(b.x + bw / 2, 0, -bw / 2),
        this.project(b.x + bw / 2, 0, bw / 2), this.project(b.x - bw / 2, 0, bw / 2),
        this.project(b.x - bw / 2, b.height, -bw / 2), this.project(b.x + bw / 2, b.height, -bw / 2),
        this.project(b.x + bw / 2, b.height, bw / 2), this.project(b.x - bw / 2, b.height, bw / 2),
      ];

      // Faces: top, then the two visible sides.
      ctx.fillStyle = front;
      ctx.beginPath();
      ctx.moveTo(c[0].x, c[0].y); ctx.lineTo(c[1].x, c[1].y);
      ctx.lineTo(c[5].x, c[5].y); ctx.lineTo(c[4].x, c[4].y);
      ctx.closePath(); ctx.fill();

      ctx.fillStyle = side;
      ctx.beginPath();
      ctx.moveTo(c[1].x, c[1].y); ctx.lineTo(c[2].x, c[2].y);
      ctx.lineTo(c[6].x, c[6].y); ctx.lineTo(c[5].x, c[5].y);
      ctx.closePath(); ctx.fill();

      ctx.fillStyle = top;
      ctx.beginPath();
      ctx.moveTo(c[4].x, c[4].y); ctx.lineTo(c[5].x, c[5].y);
      ctx.lineTo(c[6].x, c[6].y); ctx.lineTo(c[7].x, c[7].y);
      ctx.closePath(); ctx.fill();

      // Hovering an agenda row outlines its bar, so the shared color alone
      // carries the link between the 3D scene and the names.
      if (this.highlight === b.i) {
        ctx.strokeStyle = "rgba(255, 255, 255, 0.9)";
        ctx.lineWidth = 1.5;
        ctx.beginPath();
        ctx.moveTo(c[4].x, c[4].y); ctx.lineTo(c[5].x, c[5].y);
        ctx.lineTo(c[6].x, c[6].y); ctx.lineTo(c[7].x, c[7].y);
        ctx.closePath();
        ctx.moveTo(c[1].x, c[1].y); ctx.lineTo(c[5].x, c[5].y);
        ctx.moveTo(c[2].x, c[2].y); ctx.lineTo(c[6].x, c[6].y);
        ctx.stroke();
      }
    });
  },
};

// Non-rotating agenda beside the 3D scene: module names stay readable
// while it turns. Each row's color chip is its index and matches its bar.
function renderAgenda(data) {
  const ul = $("chart3d-agenda");
  if (!ul) return;
  ul.innerHTML = data.map((d, i) => {
    const color = barColor(i);
    return `<li data-rank="${i}" title="${escapeHTML(d.full)}">
      <span class="ag-swatch" style="--swatch:${color.swatch}"></span>
      <span class="ag-name">${escapeHTML(d.label)}</span>
      <span class="ag-val">${fmtBytes(d.value)}</span>
    </li>`;
  }).join("");
}

// ---- Traffic area chart: hits/misses stacked, bytes as a line ----
function drawTraffic(traffic) {
  const canvas = $("chart-traffic");
  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  const r = canvas.getBoundingClientRect();
  if (canvas.width !== r.width * dpr) {
    canvas.width = r.width * dpr;
    canvas.height = r.height * dpr;
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  const w = r.width, h = r.height;
  ctx.clearRect(0, 0, w, h);

  if (!traffic || traffic.length < 2) {
    ctx.fillStyle = "#8291ad";
    ctx.font = "13px ui-sans-serif, system-ui";
    ctx.textAlign = "center";
    ctx.fillText("waiting for traffic…", w / 2, h / 2);
    return;
  }

  const pad = { l: 6, r: 6, t: 14, b: 18 };
  const iw = w - pad.l - pad.r;
  const ih = h - pad.t - pad.b;
  const maxC = Math.max(4, ...traffic.map((b) => b.hits + b.misses));
  const maxB = Math.max(1, ...traffic.map((b) => b.bytes));
  const X = (i) => pad.l + (i / (traffic.length - 1)) * iw;
  const Yc = (v) => pad.t + ih - (v / maxC) * ih;

  // Stacked hits area.
  const area = (getV, color) => {
    ctx.beginPath();
    ctx.moveTo(X(0), pad.t + ih);
    traffic.forEach((b, i) => ctx.lineTo(X(i), Yc(getV(b))));
    ctx.lineTo(X(traffic.length - 1), pad.t + ih);
    ctx.closePath();
    const grad = ctx.createLinearGradient(0, pad.t, 0, pad.t + ih);
    grad.addColorStop(0, color.replace("%A", "0.35"));
    grad.addColorStop(1, color.replace("%A", "0.02"));
    ctx.fillStyle = grad;
    ctx.fill();
  };
  area((b) => b.hits, "rgba(34, 211, 238, %A)");
  area((b) => b.hits + b.misses, "rgba(251, 191, 36, %A)");

  // Bytes per second line, own scale.
  ctx.beginPath();
  traffic.forEach((b, i) => {
    const y = pad.t + ih - (b.bytes / maxB) * ih;
    i === 0 ? ctx.moveTo(X(i), y) : ctx.lineTo(X(i), y);
  });
  ctx.strokeStyle = "rgba(167, 139, 250, 0.9)";
  ctx.lineWidth = 1.5;
  ctx.stroke();

  // Time ticks.
  ctx.fillStyle = "#8291ad";
  ctx.font = "10px ui-monospace, monospace";
  ctx.textAlign = "left";
  ctx.fillText("-2m", pad.l, h - 4);
  ctx.textAlign = "center";
  ctx.fillText("-1m", w / 2, h - 4);
  ctx.textAlign = "right";
  ctx.fillText("now", w - pad.r, h - 4);
}

// ---- Long-term history: minute buckets aggregated by the backend ----
const MONTHS = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

function fmtHistoryTick(ms, range) {
  const d = new Date(ms);
  if (range === "24h") {
    const hh = String(d.getHours()).padStart(2, "0");
    const mm = String(d.getMinutes()).padStart(2, "0");
    return hh + ":" + mm;
  }
  if (range === "7d" || range === "30d") return (d.getMonth() + 1) + "." + d.getDate() + ".";
  if (range === "all") return MONTHS[d.getMonth()] + " '" + String(d.getFullYear()).slice(2);
  return MONTHS[d.getMonth()];
}

function drawHistory(payload) {
  const canvas = $("chart-history");
  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  const r = canvas.getBoundingClientRect();
  if (canvas.width !== r.width * dpr) {
    canvas.width = r.width * dpr;
    canvas.height = r.height * dpr;
  }
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  const w = r.width, h = r.height;
  ctx.clearRect(0, 0, w, h);

  const pts = payload.points || [];
  if (pts.length < 2) {
    ctx.fillStyle = "#8291ad";
    ctx.font = "13px ui-sans-serif, system-ui";
    ctx.textAlign = "center";
    ctx.fillText(
      pts.length === 0
        ? "no history yet — it accumulates every minute and survives reboots"
        : "waiting for more data\u2026",
      w / 2, h / 2
    );
    return;
  }

  const pad = { l: 8, r: 8, t: 16, b: 20 };
  const iw = w - pad.l - pad.r;
  const ih = h - pad.t - pad.b;
  const t0 = new Date(pts[0].time).getTime();
  const t1 = new Date(pts[pts.length - 1].time).getTime();
  const tSpan = Math.max(1, t1 - t0);
  const X = (ms) => pad.l + ((ms - t0) / tSpan) * iw;
  const maxC = Math.max(1, ...pts.map((p) => p.hits + p.misses));
  const Yc = (v) => pad.t + ih - (v / maxC) * ih;

  // Stacked request areas: hits under hits+misses.
  const area = (getV, color) => {
    ctx.beginPath();
    ctx.moveTo(X(t0), pad.t + ih);
    pts.forEach((p) => ctx.lineTo(X(new Date(p.time).getTime()), Yc(getV(p))));
    ctx.lineTo(X(t1), pad.t + ih);
    ctx.closePath();
    const grad = ctx.createLinearGradient(0, pad.t, 0, pad.t + ih);
    grad.addColorStop(0, color.replace("%A", "0.35"));
    grad.addColorStop(1, color.replace("%A", "0.02"));
    ctx.fillStyle = grad;
    ctx.fill();
  };
  area((p) => p.hits, "rgba(34, 211, 238, %A)");
  area((p) => p.hits + p.misses, "rgba(251, 191, 36, %A)");

  // Lines on their own scales: bandwidth saved, store size.
  const line = (getV, color) => {
    const maxV = Math.max(1, ...pts.map(getV));
    ctx.beginPath();
    pts.forEach((p, i) => {
      const x = X(new Date(p.time).getTime());
      const y = pad.t + ih - (getV(p) / maxV) * ih;
      i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
    });
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.6;
    ctx.shadowColor = color;
    ctx.shadowBlur = 8;
    ctx.stroke();
    ctx.shadowBlur = 0;
  };
  line((p) => p.bytesSaved, "rgba(167, 139, 250, 0.95)");
  line((p) => p.storeBytes, "rgba(52, 211, 153, 0.9)");

  // Time ticks.
  ctx.fillStyle = "#8291ad";
  ctx.font = "10px ui-monospace, monospace";
  ctx.textBaseline = "alphabetic";
  for (let i = 0; i <= 3; i++) {
    const ms = t0 + (tSpan * i) / 3;
    ctx.textAlign = i === 0 ? "left" : i === 3 ? "right" : "center";
    ctx.fillText(fmtHistoryTick(ms, payload.range), X(ms), h - 5);
  }
}

function renderHistorySummary(s) {
  const chips = [];
  chips.push(`<span class="hstat"><b>${fmtNum(s.requests)}</b> requests</span>`);
  const denom = (s.hits || 0) + (s.misses || 0);
  if (denom > 0) {
    chips.push(`<span class="hstat"><b>${Math.round((100 * s.hits) / denom)}%</b> hit ratio</span>`);
  }
  chips.push(`<span class="hstat green"><b>${fmtBytes(s.bytesSaved)}</b> saved</span>`);
  chips.push(`<span class="hstat"><b>${fmtBytes(s.bytesDown)}</b> served</span>`);
  chips.push(`<span class="hstat"><b>${fmtBytes(s.bytesUp)}</b> upstream</span>`);
  if ((s.neighbourHits || 0) > 0 || (s.neighbourMisses || 0) > 0) {
    chips.push(
      `<span class="hstat"><b>${fmtNum(s.neighbourHits || 0)}</b> neighbour hits · <b>${fmtBytes(s.bytesNeighbour || 0)}</b> from peers</span>`
    );
  }
  if (s.verifyFailures > 0) {
    chips.push(`<span class="hstat"><b>${fmtNum(s.verifyFailures)}</b> refusals</span>`);
  }
  if ((s.storeStart || 0) > 0 || (s.storeEnd || 0) > 0) {
    const growth = s.storeEnd - s.storeStart;
    const sign = growth >= 0 ? "+" : "-";
    chips.push(
      `<span class="hstat">cache <b>${fmtBytes(s.storeEnd)}</b> (${sign}${fmtBytes(Math.abs(growth))} in range)</span>`
    );
  }
  $("history-summary").innerHTML = chips.join("");
}

function applyStats(s) {
  if (!s) return;
  state.stats = s;
  if (Array.isArray(s.events)) renderEvents(s.events);
  renderHeader(s);
  if (state.store) renderKPIs(s, state.store);
  drawTraffic(s.traffic);
}

function applyStore(payload) {
  if (!payload) return;
  if (payload.version) state.version = payload.version;
  if (payload.store) state.store = payload.store;
  if (Array.isArray(payload.modules)) {
    state.modules = payload.modules;
    renderModules(payload.modules);
    chart3d.setData(payload.modules);
  }
  renderNeighbours(payload.neighbours);
  if (state.stats) renderKPIs(state.stats, state.store);
}

// Neighbour caches: one row per configured peer, reconciled in place so a
// probe result never rebuilds the table. The card is hidden entirely on a
// standalone proxy with no peers.
function renderNeighbours(list) {
  state.neighbours = Array.isArray(list) ? list : [];
  const section = $("fleet");
  if (!section) return;
  section.hidden = state.neighbours.length === 0;
  if (!state.neighbours.length) return;
  const online = state.neighbours.filter((p) => p.online).length;
  $("fleet-hint").textContent =
    online + "/" + state.neighbours.length + " online · probed every 10 min";
  const body = $("fleet-body");
  const rows = new Map();
  for (const tr of body.children) rows.set(tr.dataset.url, tr);
  const seen = new Set();
  for (const peer of state.neighbours) {
    seen.add(peer.url);
    let tr = rows.get(peer.url);
    if (!tr) {
      tr = document.createElement("tr");
      tr.dataset.url = peer.url;
      tr.innerHTML = `<td><span class="path"></span></td><td></td>
        <td class="dim"></td><td class="r"></td><td class="dim" data-time=""></td>`;
      body.appendChild(tr);
    }
    updateNeighbourRow(tr, peer);
  }
  for (const [url, tr] of rows) {
    if (!seen.has(url)) tr.remove();
  }
}

function updateNeighbourRow(tr, peer) {
  const cells = tr.children;
  cells[0].firstChild.textContent = peer.url;
  let chip = cells[1].firstChild;
  if (!chip) {
    chip = document.createElement("span");
    cells[1].appendChild(chip);
  }
  chip.className = "chip " + (peer.online ? "ok" : "bad");
  chip.textContent = peer.online ? "online" : "offline";
  chip.title = peer.err || "";
  cells[2].textContent = peer.version || "—";
  cells[3].textContent = peer.online ? peer.latencyMs + " ms" : "—";
  cells[4].dataset.time = peer.checkedAt && !peer.checkedAt.startsWith("0001") ? peer.checkedAt : "";
}

function applyHistory(data) {
  if (!data) return;
  drawHistory(data);
  renderHistorySummary(data.summary || {});
}

function setConn(ok, text) {
  const pill = document.querySelector(".pill.live");
  const label = $("conn");
  pill.classList.toggle("bad", !ok);
  label.textContent = ok ? "live" : (text || "reconnecting");
}

// ---- Live dashboard: one SSE stream carries every update ----
let sse = null;

function parseSSE(msg) {
  try {
    return JSON.parse(msg.data);
  } catch (err) {
    return null;
  }
}

// Relative timestamps age only when the server pushes; the stream's tick
// keeps them fresh without a client-side timer or another request.
function refreshAges() {
  document.querySelectorAll("#events [data-time], #modules-body [data-time], #fleet-body [data-time]").forEach((el) => {
    el.textContent = el.dataset.time ? ago(el.dataset.time) : "never";
  });
}

// connectEvents opens the one stream the dashboard needs. The server seeds
// events, cache state, counters and history on connect and then pushes
// every change; the page never fetches dashboard data on its own.
function connectEvents() {
  if (!("EventSource" in window)) {
    setConn(false, "no SSE support");
    return;
  }
  if (sse) {
    sse.close();
    sse = null;
  }
  const source = new EventSource("/api/events?range=" + encodeURIComponent(state.historyRange));
  sse = source;

  source.addEventListener("events", (msg) => {
    const events = parseSSE(msg);
    if (Array.isArray(events)) renderEvents(events);
  });
  source.addEventListener("stats", (msg) => applyStats(parseSSE(msg)));
  source.addEventListener("store", (msg) => applyStore(parseSSE(msg)));
  source.addEventListener("history", (msg) => applyHistory(parseSSE(msg)));
  source.addEventListener("event", (msg) => {
    const ev = parseSSE(msg);
    if (!ev) return;
    appendEvent(ev);
    setConn(true);
  });
  source.addEventListener("tick", refreshAges);
  source.onopen = () => setConn(true);
  source.onerror = () => {
    // EventSource reconnects on its own; the pill shows the gap.
    setConn(false);
  };
}

document.querySelectorAll("#ranges button").forEach((btn) => {
  btn.addEventListener("click", () => {
    document.querySelectorAll("#ranges button").forEach((b) => b.classList.remove("active"));
    btn.classList.add("active");
    state.historyRange = btn.dataset.range;
    // A fresh stream seeds the newly selected range.
    connectEvents();
  });
});

chart3d.init();
// Every value on the page arrives over the stream: no initial fetch, no
// polling, no timer-driven updates.
connectEvents();
