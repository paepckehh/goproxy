"use strict";

const state = { version: "", stats: null, store: null, modules: [] };
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

  init() {
    this.canvas = $("chart3d");
    this.ctx = this.canvas.getContext("2d");
    const fit = () => {
      const dpr = window.devicePixelRatio || 1;
      const r = this.canvas.getBoundingClientRect();
      this.canvas.width = Math.max(50, r.width * dpr);
      this.canvas.height = Math.max(50, r.height * dpr);
      this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };
    new ResizeObserver(fit).observe(this.canvas);
    fit();
    const loop = () => { this.draw(); requestAnimationFrame(loop); };
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
    const w = this.canvas.getBoundingClientRect().width;
    const h = this.canvas.getBoundingClientRect().height;
    const fov = 700;
    const scale = fov / (fov + Z + 260);
    return { x: w / 2 + X * scale, y: h * 0.78 - y * scale, scale, z: Z };
  },

  draw() {
    const ctx = this.ctx;
    const w = this.canvas.getBoundingClientRect().width;
    const h = this.canvas.getBoundingClientRect().height;
    ctx.clearRect(0, 0, w, h);
    this.rot += 0.0042;

    const n = this.data.length;
    if (n === 0) {
      ctx.fillStyle = "#7c8aa5";
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
      ctx.fillStyle = "#e2e8f0";
      ctx.font = "600 11px ui-monospace, monospace";
      ctx.textAlign = "center";
      ctx.fillText(fmtBytes(b.value), labelY.x, labelY.y);

      // Module name on the floor.
      const name = this.project(b.x, 0, 0);
      ctx.fillStyle = "#7c8aa5";
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
    ctx.fillStyle = "#7c8aa5";
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
  ctx.fillStyle = "#7c8aa5";
  ctx.font = "10px ui-monospace, monospace";
  ctx.textAlign = "left";
  ctx.fillText("-2m", pad.l, h - 4);
  ctx.textAlign = "center";
  ctx.fillText("-1m", w / 2, h - 4);
  ctx.textAlign = "right";
  ctx.fillText("now", w - pad.r, h - 4);
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
poll();
setInterval(poll, 2000);
