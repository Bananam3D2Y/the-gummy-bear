(() => {
  "use strict";

  // ------------------------------------------------------------ constants

  const SNAPSHOT_MS = 100; // engine publishes a snapshot every 100 ms
  const MOVE_MS = 200;     // agents step one tile every 200 ms; interpolate across that
  const NAMES = { miner: "Tove", blacksmith: "Brann", merchant: "Wren" };
  const COLORS = { miner: "#f0c05a", blacksmith: "#e0644f", merchant: "#7fc8d8" };
  const TILE = {
    ".": "#5e7f45", "=": "#b39b6e", "~": "#3f6f8f", "#": "#5e7f45", "^": "#6f6a60",
    "B": "#7a4f36", "M": "#4a4640", "F": "#b5562e", "K": "#c9a24a", "S": "#a89f8a",
  };
  const LANDMARKS = { mine: "Mine", forge: "Forge", market: "Market", square: "Square" };
  const REASONS = {
    mine_empty: "the mine is empty", not_at_mine: "not at the mine", not_at_forge: "not at the forge",
    not_at_market: "not at the market", not_enough_ore: "not enough ore", item_missing: "doesn't have that item",
    target_not_nearby: "too far away to hand it over", blocked: "the path is blocked", no_path: "no path there",
    unknown_location: "no such place", unknown_action: "unknown action",
  };

  const $ = (id) => document.getElementById(id);
  const canvas = $("map");
  const ctx = canvas.getContext("2d");

  // ------------------------------------------------------------ state

  let world = null;          // latest snapshot
  let staticLayer = null;    // pre-rendered terrain, redrawn only on resize
  let staticKey = "";
  let tileSize = 16;
  let selected = "blacksmith";
  const agents = new Map();  // id -> { fx, fy, tx, ty, t0, data }
  const priceHistory = [];
  let lastPriceSample = 0;

  // ------------------------------------------------------------ snapshots

  function currentPos(a, now) {
    const t = Math.min(1, (now - a.t0) / MOVE_MS);
    return { x: a.fx + (a.tx - a.fx) * t, y: a.fy + (a.ty - a.fy) * t };
  }

  function applySnapshot(s) {
    world = s;
    $("mapEmpty").hidden = true;
    $("tick").textContent = `Tick ${s.tick.toLocaleString()}, ${Math.floor(s.tick / 20)}s in`;

    const now = performance.now();
    for (const a of s.agents) {
      const prev = agents.get(a.id);
      if (!prev) {
        agents.set(a.id, { fx: a.x, fy: a.y, tx: a.x, ty: a.y, t0: now, data: a });
      } else {
        // Start the next glide from wherever the agent is drawn right now.
        const p = currentPos(prev, now);
        Object.assign(prev, { fx: p.x, fy: p.y, tx: a.x, ty: a.y, t0: now, data: a });
      }
    }

    const key = `${s.width}x${s.height}:${s.tiles.join("")}`;
    if (key !== staticKey) {
      staticKey = key;
      resize();
    }
    updateMarket(s, now);
    renderVillagers();
  }

  // ------------------------------------------------------------ map drawing

  function resize() {
    if (!world) return;
    const wrap = $("mapWrap");
    tileSize = Math.max(8, Math.floor(Math.min(wrap.clientWidth / world.width, wrap.clientHeight / world.height)));
    const dpr = window.devicePixelRatio || 1;
    const w = world.width * tileSize;
    const h = world.height * tileSize;
    canvas.width = w * dpr;
    canvas.height = h * dpr;
    canvas.style.width = `${w}px`;
    canvas.style.height = `${h}px`;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    buildStaticLayer(w, h, dpr);
  }

  // Cheap deterministic noise so the grass isn't a flat color.
  const shade = (x, y) => ((x * 73856093) ^ (y * 19349663)) % 7;

  function buildStaticLayer(w, h, dpr) {
    staticLayer = document.createElement("canvas");
    staticLayer.width = w * dpr;
    staticLayer.height = h * dpr;
    const g = staticLayer.getContext("2d");
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    const T = tileSize;

    for (let y = 0; y < world.height; y++) {
      const row = world.tiles[y];
      for (let x = 0; x < world.width; x++) {
        const c = row[x];
        g.fillStyle = TILE[c] || TILE["."];
        g.fillRect(x * T, y * T, T, T);
        if (c === "." || /\d/.test(c)) {
          if (shade(x, y) === 0) { g.fillStyle = "#678a4c"; g.fillRect(x * T + T * 0.3, y * T + T * 0.4, T * 0.15, T * 0.3); }
        } else if (c === "#") {
          g.fillStyle = "#2e4a2a";
          g.beginPath(); g.arc(x * T + T / 2, y * T + T / 2, T * 0.46, 0, Math.PI * 2); g.fill();
        } else if (c === "~" && shade(x, y) < 2) {
          g.fillStyle = "#5287a6"; g.fillRect(x * T + T * 0.2, y * T + T * 0.5, T * 0.5, Math.max(1, T * 0.08));
        } else if (c === "B") {
          g.fillStyle = "#5c3a28"; g.fillRect(x * T, y * T + T * 0.75, T, T * 0.25);
        } else if (c === "^") {
          g.fillStyle = "#57534b"; g.fillRect(x * T + T * 0.15, y * T + T * 0.2, T * 0.6, T * 0.55);
        }
      }
    }

    g.font = `600 ${Math.max(11, Math.round(T * 0.75))}px "Pixelify Sans", monospace`;
    g.textAlign = "center";
    for (const [name, [lx, ly]] of Object.entries(world.landmarks)) {
      const label = LANDMARKS[name] || name;
      const cx = lx * T + T / 2;
      const cy = ly * T - T * 0.35;
      g.lineWidth = 3; g.strokeStyle = "rgba(20,28,34,0.85)"; g.strokeText(label, cx, cy);
      g.fillStyle = "#f3eee0"; g.fillText(label, cx, cy);
    }
  }

  function wrapText(text, maxWidth) {
    const words = text.split(/\s+/);
    const lines = [];
    let line = "";
    for (const w of words) {
      const test = line ? `${line} ${w}` : w;
      if (ctx.measureText(test).width > maxWidth && line) { lines.push(line); line = w; } else { line = test; }
      if (lines.length === 3) break;
    }
    if (line && lines.length < 3) lines.push(line);
    return lines;
  }

  function drawBubble(text, px, py) {
    const T = tileSize;
    ctx.font = `${Math.max(11, Math.round(T * 0.7))}px "Pixelify Sans", monospace`;
    const lines = wrapText(text, Math.max(120, T * 9));
    const lh = Math.max(13, T * 0.85);
    const w = Math.max(...lines.map((l) => ctx.measureText(l).width)) + 14;
    const h = lines.length * lh + 10;
    const cw = world.width * T;
    let x = Math.min(Math.max(4, px - w / 2), cw - w - 4);
    let y = Math.max(4, py - T * 0.9 - h);

    ctx.fillStyle = "rgba(243,238,224,0.96)";
    ctx.beginPath(); ctx.roundRect(x, y, w, h, 5); ctx.fill();
    ctx.beginPath(); ctx.moveTo(px - 5, y + h); ctx.lineTo(px + 5, y + h); ctx.lineTo(px, y + h + 6); ctx.fill();
    ctx.fillStyle = "#1b2631"; ctx.textAlign = "left"; ctx.textBaseline = "top";
    lines.forEach((l, i) => ctx.fillText(l, x + 7, y + 5 + i * lh));
    ctx.textBaseline = "alphabetic";
  }

  function frame() {
    requestAnimationFrame(frame);
    if (!world || !staticLayer) return;
    const T = tileSize;
    const W = world.width * T;
    const H = world.height * T;
    ctx.clearRect(0, 0, W, H);
    ctx.drawImage(staticLayer, 0, 0, W, H);

    const now = performance.now();
    const drawn = [];
    for (const [id, a] of agents) {
      const p = currentPos(a, now);
      const px = p.x * T + T / 2;
      const py = p.y * T + T / 2;
      drawn.push({ a, px, py });

      if (id === selected) {
        ctx.strokeStyle = "#e8a33d"; ctx.lineWidth = 2;
        ctx.beginPath(); ctx.arc(px, py, T * 0.72, 0, Math.PI * 2); ctx.stroke();
      }
      ctx.fillStyle = COLORS[id] || "#ddd";
      ctx.strokeStyle = "#1b2631"; ctx.lineWidth = 2;
      ctx.beginPath(); ctx.arc(px, py, T * 0.45, 0, Math.PI * 2); ctx.fill(); ctx.stroke();

      ctx.font = `600 ${Math.max(11, Math.round(T * 0.65))}px "Pixelify Sans", monospace`;
      ctx.textAlign = "center";
      ctx.lineWidth = 3; ctx.strokeStyle = "rgba(20,28,34,0.85)";
      ctx.strokeText(NAMES[id] || id, px, py + T * 1.25);
      ctx.fillStyle = "#f3eee0";
      ctx.fillText(NAMES[id] || id, px, py + T * 1.25);
    }
    // Bubbles last so they sit on top of every agent.
    for (const { a, px, py } of drawn) if (a.data.bubble) drawBubble(a.data.bubble, px, py);
  }

  canvas.addEventListener("click", (e) => {
    if (!world) return;
    const r = canvas.getBoundingClientRect();
    const tx = (e.clientX - r.left) / tileSize;
    const ty = (e.clientY - r.top) / tileSize;
    let best = null;
    let bestD = 1.5;
    for (const [id, a] of agents) {
      const d = Math.hypot(a.tx + 0.5 - tx, a.ty + 0.5 - ty);
      if (d < bestD) { best = id; bestD = d; }
    }
    if (best) select(best);
  });

  // ------------------------------------------------------------ sidebar

  function select(id) {
    selected = id;
    $("orderAgent").value = id;
    renderVillagers();
    $("orderText").focus();
  }

  $("orderAgent").addEventListener("change", (e) => { selected = e.target.value; renderVillagers(); });

  const villagerEls = new Map();
  const orders = {}; // id -> last order text, cleared when the agent reports completion

  function renderVillagers() {
    const list = $("villagers");
    for (const id of ["miner", "blacksmith", "merchant"]) {
      const a = agents.get(id);
      let el = villagerEls.get(id);
      if (!el) {
        el = document.createElement("li");
        el.className = "villager";
        el.tabIndex = 0;
        el.innerHTML = `<span class="dot"></span><span class="name"></span><span class="inv"></span><span class="doing"></span>`;
        el.querySelector(".dot").style.background = COLORS[id];
        el.querySelector(".name").textContent = `${NAMES[id]}, ${id}`;
        el.addEventListener("click", () => select(id));
        el.addEventListener("keydown", (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(id); } });
        list.appendChild(el);
        villagerEls.set(id, el);
      }
      el.classList.toggle("selected", id === selected);
      if (!a) continue;
      const d = a.data;
      el.querySelector(".inv").textContent = `${d.inventory.ore} ore, ${d.inventory.tool} tools, ${d.coins} coins`;
      const doing = el.querySelector(".doing");
      if (orders[id]) {
        doing.textContent = `Order: ${orders[id]}`;
        doing.classList.add("has-order");
      } else {
        doing.classList.remove("has-order");
        doing.textContent = d.moving && d.destination ? `Walking to the ${d.destination}`
          : d.location ? `At the ${d.location}` : "On the road";
      }
    }
  }

  function updateMarket(s, now) {
    $("pTool").textContent = s.prices.tool.toFixed(1);
    $("pOre").textContent = s.prices.ore.toFixed(1);
    $("mineStock").textContent = s.mine_stock;
    if (now - lastPriceSample > 2000) {
      lastPriceSample = now;
      priceHistory.push(s.prices.tool);
      if (priceHistory.length > 90) priceHistory.shift();
      drawSpark();
    }
  }

  function drawSpark() {
    const c = $("spark");
    const dpr = window.devicePixelRatio || 1;
    const w = c.clientWidth;
    const h = 44;
    c.width = w * dpr; c.height = h * dpr;
    const g = c.getContext("2d");
    g.setTransform(dpr, 0, 0, dpr, 0, 0);
    if (priceHistory.length < 2) return;
    const min = Math.min(...priceHistory) - 0.5;
    const max = Math.max(...priceHistory) + 0.5;
    g.strokeStyle = "#e8a33d"; g.lineWidth = 2; g.beginPath();
    priceHistory.forEach((v, i) => {
      const x = (i / (priceHistory.length - 1)) * (w - 2) + 1;
      const y = h - 3 - ((v - min) / (max - min)) * (h - 6);
      i ? g.lineTo(x, y) : g.moveTo(x, y);
    });
    g.stroke();
  }

  // ------------------------------------------------------------ chronicle

  function addEntry(cls, who, what, tag) {
    const list = $("chronicle");
    const empty = list.querySelector(".empty");
    if (empty) empty.remove();
    const li = document.createElement("li");
    if (cls) li.className = cls;
    const whoEl = document.createElement("span");
    whoEl.className = "who";
    whoEl.textContent = NAMES[who] || who;
    if (COLORS[who]) whoEl.style.color = COLORS[who];
    const whatEl = document.createElement("span");
    whatEl.className = "what";
    whatEl.textContent = ` ${what}`;
    li.append(whoEl, whatEl);
    if (tag) {
      const t = document.createElement("span");
      t.className = "tag";
      t.textContent = tag;
      li.append(t);
    }
    list.prepend(li);
    while (list.children.length > 150) list.lastChild.remove();
  }

  function onThought(t) {
    if (t.tool === "error") { addEntry("rejected", t.agent_id, t.text); return; }
    if (t.tool === "complete_order") { delete orders[t.agent_id]; renderVillagers(); }
    let args = "";
    try {
      const parsed = JSON.parse(t.args || "{}");
      args = Object.values(parsed).filter((v) => typeof v !== "object").join(", ");
    } catch { /* ignore */ }
    if (t.tool === "say") {
      addEntry("speech", t.agent_id, `says "${(JSON.parse(t.args || "{}").text) || ""}"`, `${(t.latency_ms / 1000).toFixed(1)}s`);
      return;
    }
    const tag = `${t.tool}${args ? ` ${args}` : ""}, ${(t.latency_ms / 1000).toFixed(1)}s${t.retrieved ? `, ${t.retrieved} memories` : ""}`;
    addEntry("", t.agent_id, t.text || "(no reasoning given)", tag);
  }

  function onEvent(e) {
    if (e.type === "action_rejected") {
      const stale = e.staleness_ticks ? `, decided ${(e.staleness_ticks / 20).toFixed(1)}s earlier` : "";
      addEntry("rejected", e.agent_id, `${e.action} rejected: ${REASONS[e.reason] || e.reason}`, stale.slice(2));
    } else if (e.type === "action_result") {
      addEntry("result", e.agent_id, e.detail);
    } else if (e.type === "market_report") {
      addEntry("result", e.agent_id, `checked the market: tool ${e.prices.tool}, ore ${e.prices.ore}`);
    }
    // "heard" and "received" are skipped: the speaker's thought and the giver's result already show them.
  }

  function onOrder(o) {
    if (o.agent_id === "all") Object.keys(NAMES).forEach((id) => { orders[id] = o.text; });
    else orders[o.agent_id] = o.text;
    renderVillagers();
    addEntry("order-entry", "Overseer", `to ${NAMES[o.agent_id] || "everyone"}: ${o.text}`);
  }

  // ------------------------------------------------------------ websocket

  let ws = null;
  let backoff = 500;

  function connect() {
    ws = new WebSocket(`${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/ws`);
    ws.onopen = () => {
      backoff = 500;
      $("conn").textContent = "Connected";
      $("conn").className = "conn conn-up";
    };
    ws.onclose = () => {
      $("conn").textContent = "Reconnecting";
      $("conn").className = "conn conn-down";
      setTimeout(connect, backoff);
      backoff = Math.min(backoff * 2, 5000);
    };
    ws.onmessage = (m) => {
      let msg;
      try { msg = JSON.parse(m.data); } catch { return; }
      if (msg.type === "snapshot") applySnapshot(msg.data);
      else if (msg.type === "thought") onThought(msg.data);
      else if (msg.type === "event") onEvent(msg.data);
      else if (msg.type === "order") onOrder(msg.data);
    };
  }

  // ------------------------------------------------------------ order form

  const form = $("orderForm");
  const text = $("orderText");
  const err = $("orderError");

  text.addEventListener("input", () => { err.textContent = ""; });
  text.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); form.requestSubmit(); }
  });

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const body = text.value.trim();
    if (!body) { err.textContent = "Write an order first."; return; }
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      err.textContent = "Not connected to the gateway. Check that the gateway is running on port 8080.";
      return;
    }
    ws.send(JSON.stringify({ type: "order", agent_id: $("orderAgent").value, text: body }));
    text.value = "";
  });

  window.addEventListener("resize", resize);
  renderVillagers();
  connect();
  requestAnimationFrame(frame);
})();
