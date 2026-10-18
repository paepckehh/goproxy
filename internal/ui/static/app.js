"use strict";

const state = { version: "", stats: null, store: null, modules: [], historyRange: "24h" };
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
  const L = s.lifetime;
  countUp("kpi-requests", L.requests, fmtNum);
  $("kpi-requests-sub").textContent =
    fmtNum(L.hits) + " hits · " + fmtNum(L.misses) + " misses · " + fmtNum(L.passes) + " passes";

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
}

function shortMod(path) {
  const parts = path.split("/");
  return parts.length > 2 ? parts.slice(-2).join("/") : path;
}

function renderModules(mods) {
  const body = $("modules-body");
  $("modules-empty").style.display = mods.length ? "none" : "block";
  $("modules-hint").textContent = mods.length ? "top " + Math.min(mods.length, 10) + " by bytes served" : "";
  const rows = mods.slice(0, 10).map((m) => {
    const used = m.lastUsed && !m.lastUsed.startsWith("0001") ? ago(m.lastUsed) : "never";
    const trust = m.verified
      ? '<span class="chip ok">verified</span>'
      : '<span class="chip warn">unverified</span>';
    return `<tr>
      <td><span class="path">${escapeHTML(m.path)}</span></td>
      <td class="r">${m.versions}</td>
      <td class="r">${fmtBytes(m.sizeBytes)}</td>
      <td class="r">${fmtNum(m.requests)}</td>
      <td class="r">${fmtBytes(m.servedBytes)}</td>
      <td class="dim">${used}</td>
      <td>${trust}</td>
    </tr>`;
  });
  body.innerHTML = rows.join("");
}

function eventChip(result) {
  switch (result) {
    case "hit": return '<span class="chip ok">hit</span>';
    case "miss": return '<span class="chip warn">miss</span>';
    case "pass": return '<span class="chip ok">pass</span>';
    case "verify-failed": return '<span class="chip bad">tampered</span>';
    case "error": return '<span class="chip bad">error</span>';
    case "upstream": return '<span class="chip bad">upstream</span>';
    default: return '<span class="chip ok">info</span>';
  }
}

function renderEvents(events) {
  const ul = $("events");
  $("events-empty").style.display = events.length ? "none" : "block";
  $("events-hint").textContent = events.length ? "newest first · " + events.length : "";
  const items = events.slice(0, 24).map((ev) => {
    const target = ev.module
      ? escapeHTML(ev.module) + (ev.version ? " " + escapeHTML(ev.version) : "")
      : "goproxy";
    const detail = ev.detail
      ? escapeHTML(ev.detail)
      : (ev.kind || "") + (ev.bytes ? " · " + fmtBytes(ev.bytes) : "");
    return `<li>
      ${eventChip(ev.result)}
      <div class="ev-main">
        <div class="ev-mod">${target}</div>
        <div class="ev-detail">${detail}</div>
      </div>
      <span class="ev-time">${ago(ev.time)}</span>
    </li>`;
  });
  ul.innerHTML = items.join("");
}

function escapeHTML(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

// ---- 3D rotating bar chart: top modules by served bandwidth ----
const chart3d = {
  canvas: null,
  ctx: null,
  rot: 0.6,
  data: [],
  w: 0,
  h: 0,

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
    const loop = () => {
      // Rotate only while the tab is visible; redraw once when it returns.
      if (!document.hidden) this.rot += 0.0042;
      this.draw();
      requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
  },

  setData(mods) {
    this.data = mods.slice(0, 8).map((m) => ({
      label: shortMod(m.path),
      value: m.servedBytes,
    }));
  },

  project(x, y, z) {
    const c = Math.cos(this.rot), s = Math.sin(this.rot);
    const X = x * c - z * s;
    const Z = x * s + z * c;
    const w = this.w, h = this.h;
    const fov = 700;
    const scale = fov / (fov + Z + 260);
    return { x: w / 2 + X * scale, y: h * 0.78 - y * scale, scale, z: Z };
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
    const spacing = 62;
    const bw = 34;
    const maxH = h * 0.55;

    // Floor grid.
    ctx.strokeStyle = "rgba(148, 163, 255, 0.10)";
    ctx.lineWidth = 1;
    for (const gx of [-4, -2, 0, 2, 4]) {
      ctx.beginPath();
      const a = this.project(gx * spacing / 2, 0, -n * spacing / 2 - 30);
      const b = this.project(gx * spacing / 2, 0, n * spacing / 2 + 30);
      ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y); ctx.stroke();
    }

    const bars = this.data.map((d, i) => {
      const height = 18 + (d.value / maxV) * maxH;
      const x = (i - (n - 1) / 2) * spacing;
      return { ...d, x, height, depth: x * Math.sin(this.rot) };
    });
    bars.sort((a, b) => a.depth - b.depth);

    const hue0 = 250, hue1 = 190;
    bars.forEach((b, rank) => {
      const t = n === 1 ? 0.5 : rank / (n - 1);
      const hue = hue0 + (hue1 - hue0) * t;
      const top = `hsl(${hue} 85% 62%)`;
      const front = `hsl(${hue} 70% 44% / 0.92)`;
      const side = `hsl(${hue} 65% 32% / 0.92)`;

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

      // Value above the bar.
      const labelY = this.project(b.x, b.height + 16, 0);
      ctx.fillStyle = "#e6ecf7";
      ctx.font = "600 11px ui-monospace, monospace";
      ctx.textAlign = "center";
      ctx.fillText(fmtBytes(b.value), labelY.x, labelY.y);

      // Module name on the floor.
      const name = this.project(b.x, 0, 0);
      ctx.fillStyle = "#8291ad";
      ctx.font = "10.5px ui-monospace, monospace";
      let text = b.label;
      while (text.length > 14 && ctx.measureText(text).width > spacing + 26) text = text.slice(1);
      ctx.fillText(text.length < b.label.length ? "…" + text : text, name.x, name.y + 16);
    });
  },
};

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
  if (s.verifyFailures > 0) {
    chips.push(`<span class="hstat"><b>${fmtNum(s.verifyFailures)}</b> refusals</span>`);
  }
  if (s.storeStart > 0 && s.storeEnd > 0) {
    const growth = s.storeEnd - s.storeStart;
    const sign = growth >= 0 ? "+" : "-";
    chips.push(
      `<span class="hstat">cache <b>${fmtBytes(s.storeEnd)}</b> (${sign}${fmtBytes(Math.abs(growth))} in range)</span>`
    );
  }
  $("history-summary").innerHTML = chips.join("");
}

// historySeq guards against stale responses: the 60s poll and range-click
// fetches can resolve out of order, and only the newest request may render.
let historySeq = 0;

async function fetchHistory() {
  const seq = ++historySeq;
  try {
    const resp = await fetch("/api/history?range=" + encodeURIComponent(state.historyRange), {
      cache: "no-store",
    });
    if (!resp.ok) throw new Error("status " + resp.status);
    const data = await resp.json();
    if (seq !== historySeq) return; // superseded by a newer request
    drawHistory(data);
    renderHistorySummary(data.summary || {});
  } catch (err) {
    // keep the last rendered chart; the next poll retries
  }
}

async function poll() {
  try {
    const resp = await fetch("/api/dashboard", { cache: "no-store" });
    if (!resp.ok) throw new Error("status " + resp.status);
    const data = await resp.json();
    state.version = data.version;
    state.stats = data.stats;
    state.store = data.store;
    state.modules = data.modules || [];
    renderHeader(data.stats);
    renderKPIs(data.stats, data.store);
    renderModules(state.modules);
    renderEvents(data.stats.events || []);
    chart3d.setData(state.modules);
    drawTraffic(data.stats.traffic);
    setConn(true);
  } catch (err) {
    setConn(false);
  }
}

function setConn(ok) {
  const pill = document.querySelector(".pill.live");
  const label = $("conn");
  if (ok) {
    pill.classList.remove("bad");
    label.textContent = "live";
  } else {
    pill.classList.add("bad");
    label.textContent = "reconnecting";
  }
}

chart3d.init();
document.querySelectorAll("#ranges button").forEach((btn) => {
  btn.addEventListener("click", () => {
    document.querySelectorAll("#ranges button").forEach((b) => b.classList.remove("active"));
    btn.classList.add("active");
    state.historyRange = btn.dataset.range;
    fetchHistory();
  });
});
poll();
fetchHistory();
setInterval(poll, 2000);
setInterval(fetchHistory, 60000);
