(() => {
  "use strict";

  // ------------------------------------------------------------ constants

  const MOVE_MS = 200; // cooks step one tile every 200 ms; interpolate across that
  const NAMES = {};    // filled from the snapshot (engine reads cast.json)
  const COLORS = {};
  const TILE = {
    ".": "#2f3a42", "=": "#3c4a53", "#": "#1a2228", "B": "#6a4a35",
    "W": "#4d6b78", "L": "#8c3f2c", "P": "#b08a3c", "A": "#374048",
  };
  const LANDMARKS = { walkin: "Walk-in", line: "The Line", pass: "The Pass", alley: "Alley" };
  const REASONS = {
    out_of_stock: "the walk-in is empty", not_at_walkin: "not at the walk-in",
    not_at_line: "not at the line", not_at_pass: "not at the pass",
    not_enough_prep: "not enough prep", not_on_menu: "that is not on the menu",
    dish_missing: "doesn't have that dish", no_ticket_for_dish: "no ticket for that dish",
    item_missing: "doesn't have that", target_not_nearby: "too far away to hand it over",
    blocked: "the path is blocked", no_path: "no way through",
    unknown_station: "no such station", unknown_action: "unknown action",
  };

  const $ = (id) => document.getElementById(id);
  const canvas = $("map");
  const ctx = canvas.getContext("2d");

  // ------------------------------------------------------------ state

  let world = null;
  let staticLayer = null;
  let staticKey = "";
  let tileSize = 16;
  let selected = "";
  let unread = 0;
  const agents = new Map();   // id -> { fx, fy, tx, ty, t0, data }
  const threads = {};         // id -> [{ side, text }]
  const orders = {};          // id -> current standing order

  // ------------------------------------------------------------ snapshots

  function currentPos(a, now) {
    const t = Math.min(1, (now - a.t0) / MOVE_MS);
    return { x: a.fx + (a.tx - a.fx) * t, y: a.fy + (a.ty - a.fy) * t };
  }

  function applySnapshot(s) {
    world = s;
    $("mapEmpty").hidden = true;
    $("tick").textContent = `Tick ${s.tick.toLocaleString()}`;
    if (s.restaurant) document.title = $("restaurant").textContent = s.restaurant;
    if (s.clock) $("clock").textContent = `${s.clock}${s.phase ? ", " + s.phase : ""}`;

    const now = performance.now();
    for (const a of s.agents) {
      NAMES[a.id] = a.name || a.id;
      COLORS[a.id] = a.color || "#dddddd";
      const prev = agents.get(a.id);
      if (!prev) {
        agents.set(a.id, { fx: a.x, fy: a.y, tx: a.x, ty: a.y, t0: now, data: a });
      } else {
        const p = currentPos(prev, now);
        Object.assign(prev, { fx: p.x, fy: p.y, tx: a.x, ty: a.y, t0: now, data: a });
      }
    }
    if (!selected && s.agents.length) selected = s.agents[0].id;

    const key = `${s.width}x${s.height}:${s.tiles.join("")}`;
    if (key !== staticKey) { staticKey = key; resize(); }
    updateService(s);
    renderCrew();
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
        if (c === "B") {
          g.fillStyle = "#8a6549";
          g.fillRect(x * T, y * T, T, Math.max(1, T * 0.18));
        } else if (c === "L") {
          g.fillStyle = "#f0b04a";
          g.beginPath(); g.arc(x * T + T / 2, y * T + T / 2, T * 0.22, 0, Math.PI * 2); g.fill();
        } else if (c === "P") {
          g.fillStyle = "#f2d78a";
          g.fillRect(x * T + T * 0.15, y * T + T * 0.1, T * 0.7, T * 0.16);
        } else if ((c === "." || /\d/.test(c)) && shade(x, y) === 0) {
          g.fillStyle = "#354049";
          g.fillRect(x * T + T * 0.35, y * T + T * 0.35, T * 0.3, T * 0.3);
        }
      }
    }

    // warm light under the heat lamp and over the burners
    for (const key of ["pass", "line"]) {
      const spot = world.landmarks[key];
      if (!spot) continue;
      const cx = spot[0] * T + T / 2;
      const cy = spot[1] * T + T / 2;
      const lamp = g.createRadialGradient(cx, cy, T * 0.5, cx, cy, T * 4.5);
      lamp.addColorStop(0, key === "pass" ? "rgba(242,215,138,0.20)" : "rgba(240,176,74,0.16)");
      lamp.addColorStop(1, "transparent");
      g.fillStyle = lamp;
      g.fillRect(cx - T * 4.5, cy - T * 4.5, T * 9, T * 9);
    }

    g.font = `600 ${Math.max(11, Math.round(T * 0.75))}px "Pixelify Sans", monospace`;
    g.textAlign = "center";
    for (const [name, spot] of Object.entries(world.landmarks)) {
      const label = LANDMARKS[name] || name;
      const cx = spot[0] * T + T / 2;
      const cy = spot[1] * T - T * 0.35;
      g.lineWidth = 3; g.strokeStyle = "rgba(10,14,18,0.85)"; g.strokeText(label, cx, cy);
      g.fillStyle = "#f3eee0"; g.fillText(label, cx, cy);
    }
  }

  function wrapText(text, maxWidth) {
    const words = String(text).split(/\s+/);
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
    const x = Math.min(Math.max(4, px - w / 2), world.width * T - w - 4);
    const y = Math.max(4, py - T * 0.9 - h);

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

      const glow = ctx.createRadialGradient(px, py, T * 0.2, px, py, T * 1.1);
      glow.addColorStop(0, (COLORS[id] || "#ddd") + "44");
      glow.addColorStop(1, "transparent");
      ctx.fillStyle = glow;
      ctx.fillRect(px - T * 1.1, py - T * 1.1, T * 2.2, T * 2.2);

      if (id === selected) {
        ctx.strokeStyle = "#e8a33d"; ctx.lineWidth = 2;
        ctx.beginPath(); ctx.arc(px, py, T * 0.72, 0, Math.PI * 2); ctx.stroke();
      }
      ctx.fillStyle = COLORS[id] || "#ddd";
      ctx.strokeStyle = "#11171b"; ctx.lineWidth = 2;
      ctx.beginPath(); ctx.arc(px, py, T * 0.45, 0, Math.PI * 2); ctx.fill(); ctx.stroke();

      ctx.font = `600 ${Math.max(11, Math.round(T * 0.65))}px "Pixelify Sans", monospace`;
      ctx.textAlign = "center";
      ctx.lineWidth = 3; ctx.strokeStyle = "rgba(10,14,18,0.85)";
      ctx.strokeText(NAMES[id] || id, px, py + T * 1.25);
      ctx.fillStyle = "#f3eee0";
      ctx.fillText(NAMES[id] || id, px, py + T * 1.25);
    }
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

  // ------------------------------------------------------------ crew list

  const crewEls = new Map();

  function fillOrderOptions() {
    const sel = $("orderAgent");
    const ids = [...agents.keys()];
    if (sel.options.length === ids.length + 1) return;
    sel.innerHTML = "";
    for (const id of ids) {
      const o = document.createElement("option");
      o.value = id;
      const role = agents.get(id).data.role || "";
      o.textContent = role ? `${NAMES[id]}, ${role}` : NAMES[id];
      sel.appendChild(o);
    }
    const all = document.createElement("option");
    all.value = "all";
    all.textContent = "The whole kitchen";
    sel.appendChild(all);
    if (ids.includes(selected)) sel.value = selected;
  }

  function renderCrew() {
    const list = $("villagers");
    fillOrderOptions();
    for (const id of agents.keys()) {
      const a = agents.get(id);
      let el = crewEls.get(id);
      if (!el) {
        el = document.createElement("li");
        el.className = "villager";
        el.tabIndex = 0;
        el.innerHTML = `<span class="dot"></span><span class="name"></span><span class="inv"></span><span class="doing"></span>`;
        el.querySelector(".dot").style.background = COLORS[id];
        el.querySelector(".name").textContent = `${NAMES[id]}, ${a.data.role || ""}`.replace(/, $/, "");
        el.addEventListener("click", () => select(id));
        el.addEventListener("keydown", (e) => {
          if (e.key === "Enter" || e.key === " ") { e.preventDefault(); select(id); }
        });
        list.appendChild(el);
        crewEls.set(id, el);
      }
      el.classList.toggle("selected", id === selected);
      const d = a.data;
      const carrying = Object.entries(d.inventory || {}).filter(([, n]) => n > 0)
        .map(([item, n]) => `${n} ${item}`).join(", ");
      el.querySelector(".inv").textContent = carrying ? `${carrying}, ${d.coins} tips` : `${d.coins} tips`;
      const doing = el.querySelector(".doing");
      if (orders[id]) {
        doing.textContent = `Order: ${orders[id]}`;
        doing.classList.add("has-order");
      } else {
        doing.classList.remove("has-order");
        doing.textContent = d.moving && d.destination
          ? `Heading to the ${LANDMARKS[d.destination] || d.destination}`
          : d.location ? `At the ${LANDMARKS[d.location] || d.location}` : "Crossing the kitchen";
      }
    }
  }

  function select(id) {
    selected = id;
    $("orderAgent").value = id;
    renderCrew();
    updateChatWith();
    renderThread(id);
    $("orderText").focus();
  }

  // ------------------------------------------------------------ service panel

  function updateService(s) {
    $("served").textContent = s.served ?? 0;
    $("walkouts").textContent = s.walkouts ?? 0;
    $("stock").textContent = s.stock ?? 0;

    const rail = $("rail");
    const tickets = s.tickets || [];
    rail.innerHTML = "";
    if (!tickets.length) {
      const li = document.createElement("li");
      li.className = "empty";
      li.textContent = "Rail is clear.";
      rail.appendChild(li);
    }
    for (const t of tickets) {
      const li = document.createElement("li");
      li.className = t.due_in_s <= 0 ? "ticket late" : t.due_in_s < 30 ? "ticket soon" : "ticket";
      const dish = document.createElement("span");
      dish.className = "dish";
      dish.textContent = `#${t.id} ${t.dish}`;
      const age = document.createElement("span");
      age.className = "age";
      age.textContent = t.due_in_s <= 0 ? `LATE ${-t.due_in_s}s` : `${t.waiting_s}s`;
      li.append(dish, age);
      rail.appendChild(li);
    }

    const menu = $("menu");
    if (s.menu && menu.dataset.len !== String(s.menu.length)) {
      const known = new Set((menu.dataset.known || "").split("|").filter(Boolean));
      const first = menu.dataset.len === undefined;
      menu.dataset.len = String(s.menu.length);
      menu.innerHTML = "";
      for (const dish of s.menu) {
        const chip = document.createElement("span");
        chip.className = !first && !known.has(dish) ? "chip chip-new" : "chip";
        chip.textContent = dish;
        menu.appendChild(chip);
      }
      menu.dataset.known = s.menu.join("|");
    }
  }

  // ------------------------------------------------------------ tabs, chronicle, chat

  const chatVisible = () => !$("chatSection").hidden;

  function showTab(which) {
    const chat = which === "chat";
    $("chatSection").hidden = !chat;
    $("chronicleSection").hidden = chat;
    $("tabChat").classList.toggle("active", chat);
    $("tabChronicle").classList.toggle("active", !chat);
    $("tabChat").setAttribute("aria-selected", String(chat));
    $("tabChronicle").setAttribute("aria-selected", String(!chat));
    if (chat) {
      unread = 0;
      $("chatBadge").classList.remove("on");
      renderThread(selected);
    }
  }

  function updateChatWith() {
    $("chatWith").textContent = NAMES[selected] ? `with ${NAMES[selected]}` : "";
  }

  function renderThread(id) {
    const list = $("chat");
    list.innerHTML = "";
    const thread = threads[id] || [];
    if (!thread.length) {
      const li = document.createElement("li");
      li.className = "empty";
      li.textContent = `No conversation with ${NAMES[id] || "them"} yet. Ask them something.`;
      list.appendChild(li);
      return;
    }
    for (const m of thread) {
      const li = document.createElement("li");
      li.className = m.side === "owner" ? "from-owner" : "from-cook";
      const who = document.createElement("span");
      who.className = "speaker";
      who.textContent = m.side === "owner" ? "You" : NAMES[id] || id;
      if (m.side !== "owner" && COLORS[id]) who.style.color = COLORS[id];
      const body = document.createElement("span");
      body.textContent = m.text;
      li.append(who, body);
      list.appendChild(li);
    }
    list.scrollTop = list.scrollHeight;
  }

  function addChat(side, who, text) {
    if (!who) return;
    (threads[who] = threads[who] || []).push({ side, text });
    while (threads[who].length > 40) threads[who].shift();
    if (who === selected) renderThread(who);
    if (!(who === selected && chatVisible())) {
      unread++;
      const b = $("chatBadge");
      b.textContent = unread;
      b.classList.add("on");
    }
  }

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

  function argOf(t, key) {
    try { return JSON.parse(t.args || "{}")[key] || ""; } catch { return ""; }
  }

  function onThought(t) {
    if (!t || !t.agent_id) return;
    const secs = ((t.latency_ms || 0) / 1000).toFixed(1);

    if (t.tool === "reply_to_owner") { addChat("cook", t.agent_id, t.text || "(no answer)"); return; }
    if (t.tool === "error") { addEntry("rejected", t.agent_id, t.text || "(brain offline)"); return; }
    if (t.tool === "create_dish") {
      addEntry("recipe", t.agent_id, `put ${argOf(t, "dish")} on the menu. ${t.text || ""}`);
      return;
    }
    if (t.tool === "complete_order") { delete orders[t.agent_id]; renderCrew(); }
    if (t.tool === "say") {
      addEntry("speech", t.agent_id, `says "${argOf(t, "text")}"`, `${secs}s`);
      return;
    }
    let args = "";
    try {
      args = Object.values(JSON.parse(t.args || "{}")).filter((v) => typeof v !== "object").join(", ");
    } catch { /* ignore */ }
    const tag = `${t.tool}${args ? ` ${args}` : ""}, ${secs}s${t.retrieved ? `, ${t.retrieved} memories` : ""}`;
    addEntry("", t.agent_id, t.text || "(no reasoning given)", tag);
  }

  function onEvent(e) {
    if (!e) return;
    if (e.type === "action_rejected") {
      const stale = e.staleness_ticks ? `decided ${(e.staleness_ticks / 20).toFixed(1)}s earlier` : "";
      addEntry("rejected", e.agent_id, `${e.action} rejected: ${REASONS[e.reason] || e.reason}`, stale);
    } else if (e.type === "action_result") {
      addEntry("result", e.agent_id, e.detail);
    } else if (e.type === "ticket_in") {
      addEntry("order-entry", e.agent_id, `new ticket: ${e.dish} (${e.open_tickets} on the rail)`);
    } else if (e.type === "walkout") {
      addEntry("rejected", e.agent_id, `a table walked out waiting for ${e.dish}`);
    }
  }

  function onOrder(o) {
    if (!o) return;
    if (o.kind === "chat") { addChat("owner", o.agent_id, o.text); return; }
    if (o.agent_id === "all") Object.keys(NAMES).forEach((id) => { orders[id] = o.text; });
    else orders[o.agent_id] = o.text;
    renderCrew();
    addEntry("order-entry", "Owner", `to ${NAMES[o.agent_id] || "the kitchen"}: ${o.text}`);
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
      try {
        if (msg.type === "snapshot") applySnapshot(msg.data);
        else if (msg.type === "thought") onThought(msg.data);
        else if (msg.type === "event") onEvent(msg.data);
        else if (msg.type === "order") onOrder(msg.data);
      } catch (err) {
        console.error("message handler failed", msg && msg.type, err);
      }
    };
  }

  // ------------------------------------------------------------ the form

  const form = $("orderForm");
  const text = $("orderText");
  const err = $("orderError");
  const modeOf = () => document.querySelector('input[name="mode"]:checked').value;

  document.querySelectorAll('input[name="mode"]').forEach((r) =>
    r.addEventListener("change", () => {
      const chat = modeOf() === "chat";
      $("sendBtn").textContent = chat ? "Ask" : "Send order";
      text.placeholder = chat ? "What are you working on right now?"
                              : "Drop everything and get the two oldest tickets out";
      updateChatWith();
    }));

  $("orderAgent").addEventListener("change", (e) => {
    selected = e.target.value;
    renderCrew();
    updateChatWith();
    renderThread(selected);
  });

  $("tabChronicle").addEventListener("click", () => showTab("chronicle"));
  $("tabChat").addEventListener("click", () => showTab("chat"));

  text.addEventListener("input", () => { err.textContent = ""; });
  text.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); form.requestSubmit(); }
  });

  form.addEventListener("submit", (e) => {
    e.preventDefault();
    const body = text.value.trim();
    if (!body) { err.textContent = "Write something first."; return; }
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      err.textContent = "Not connected to the gateway. Check that it's running on port 8080.";
      return;
    }
    const mode = modeOf();
    ws.send(JSON.stringify({ type: mode, agent_id: $("orderAgent").value, text: body }));
    text.value = "";
    if (mode === "chat") showTab("chat");
  });

  window.addEventListener("resize", resize);
  updateChatWith();
  connect();
  requestAnimationFrame(frame);
})();
