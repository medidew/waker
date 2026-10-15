"use strict";

const DEFAULT_POLL_MS = 4000; // used until the first /api/status tells us poll_ms
const FAST_POLL_MS = 2000;    // short-cadence polling right after user actions
const FAST_POLL_WINDOW_MS = 40000; // max duration of fast-polling after an action
const LOAD_TIMEOUT_MS = 35000;     // max time the UI waits for a model load request
let pollMs = DEFAULT_POLL_MS;
const WAKE_TIMEOUT_MS = 120000; // must match wakeserver wakeTimeout + staleWindow

let token = localStorage.getItem("waker.token") || "";
let desktopOnline = false;
let lastStatus = null;
let wakeDeadline = 0; // epoch ms when the current wake attempt times out locally

const $ = (sel) => document.querySelector(sel);
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

/* ---------- token gate ---------- */

function showGate() { $("#token-gate").classList.remove("hidden"); }
$("#token-btn").addEventListener("click", saveToken);
$("#token-input").addEventListener("keydown", (e) => { if (e.key === "Enter") saveToken(); });

function saveToken() {
  token = $("#token-input").value.trim();
  localStorage.setItem("waker.token", token);
  $("#gate-msg").classList.add("hidden");
  $("#token-gate").classList.add("hidden");
  poll(true);
}

/* ---------- API ---------- */

async function api(path, opts = {}) {
  const ctrl = new AbortController();
  if (opts.timeoutMs > 0) setTimeout(() => ctrl.abort(), opts.timeoutMs); // aborting after completion is a no-op
  let res;
  try {
    res = await fetch("/api" + path, {
      ...opts,
      signal: ctrl.signal,
      headers: { "Authorization": "Bearer " + token, "Content-Type": "application/json", ...(opts.headers || {}) },
    });
  } catch (e) {
    if (ctrl.signal.aborted) { const t = new Error("request timed out"); t.name = "TimeoutError"; throw t; }
    throw e;
  }
  if (res.status === 401) { showGate(); $("#gate-msg").classList.remove("hidden"); throw new Error("unauthorized"); }
  return res;
}

function setConnError(detail) {
  const pill = $("#presence");
  pill.classList.remove("online");
  pill.classList.add("offline");
  $("#desktop-name").textContent = detail === "auth" ? "token rejected" : "unreachable";
  $("#wake-subtitle").textContent = detail === "auth"
    ? "The server rejected the access token — re-enter it in the dialog."
    : "Cannot reach the waker API (" + detail + "). Check that wakeserver is running.";
}

/* ---------- polling & rendering ---------- */

async function poll() {
  let st;
  try {
    const res = await api("/status");
    if (!res.ok) { setConnError("http " + res.status); return; }
    st = await res.json();
  } catch (e) {
    setConnError(e && e.message === "unauthorized" ? "auth" : String((e && e.message) || e));
    return;
  }
  lastStatus = st;
  if (st.poll_ms > 0) pollMs = Math.max(FAST_POLL_MS, Number(st.poll_ms));

  renderPresence(st);
  renderLinks(st.links || []);
  renderSSH(st.desktop || {});

  if (st.desktop.online) {
    desktopOnline = true;
    wakeDeadline = 0;
    try {
      const dres = await api("/desktop/api/status");
      if (dres.ok) {
        const d = await dres.json();
        renderSystem(d.system || {});
        renderServices(d.services || []);
        $("#system-card").classList.remove("hidden");
        $("#services-card").classList.remove("hidden");
        refreshModels();
      }
    } catch (e) { /* agent briefly unreachable; retry next poll */ }
  } else {
    desktopOnline = false;
    hideDesktopPanels();
  }
  updateWakeUI(st);
}

function renderPresence(st) {
  const online = !!st.desktop.online;
  const pill = $("#presence");
  pill.classList.toggle("online", online);
  pill.classList.toggle("offline", !online);
  $("#desktop-name").textContent = (st.desktop.hostname || "desktop") + (online ? " · online" : " · offline");
}

function updateWakeUI(st) {
  const btn = $("#wake-btn"), timerEl = $("#wake-timer"), sub = $("#wake-subtitle");
  if (desktopOnline) {
    btn.disabled = true;
    btn.textContent = "● Online";
    sub.textContent = "Desktop is awake.";
    timerEl.classList.add("hidden");
    return;
  }
  const now = Date.now();
  const waking = (st.waking && st.waking.active) || wakeDeadline > now;
  if (waking) {
    btn.disabled = true;
    btn.textContent = "Waking…";
    sub.textContent = "Waiting for the desktop to join the tailnet — can take up to ~2 minutes.";
    timerEl.classList.remove("hidden");
    tickTimer();
  } else {
    btn.disabled = false;
    btn.textContent = "Wake desktop";
    sub.textContent = "Sends a Wake-on-LAN packet and waits for it to come online.";
    timerEl.classList.add("hidden");
  }
}

function tickTimer() {
  if (!lastStatus) return;
  const startedAt = lastStatus.waking && lastStatus.waking.started_at
    ? new Date(lastStatus.waking.started_at).getTime()
    : wakeDeadline - WAKE_TIMEOUT_MS;
  const elapsed = Math.max(0, Math.floor((Date.now() - startedAt) / 1000));
  $("#wake-timer").textContent = fmtElapsed(elapsed);
}

function fmtElapsed(s) { return Math.floor(s / 60) + ":" + String(s % 60).padStart(2, "0"); }

function fmtUptime(sec) {
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

function renderSystem(s) {
  const ramPct = s.mem_total_mb ? Math.min(100, (100 * s.mem_used_mb) / s.mem_total_mb) : 0;
  let html = `
    <div class="stat"><span class="k">Uptime</span><span class="v">${fmtUptime(s.uptime_sec || 0)}</span></div>
    <div class="stat"><span class="k">Load (1/5/15)</span><span class="v">${(s.load1||0).toFixed(2)} / ${(s.load5||0).toFixed(2)} / ${(s.load15||0).toFixed(2)}</span></div>
    <div class="stat"><span class="k">Memory</span><span class="bar"><div style="width:${ramPct}%"></div></span><span class="v">${((s.mem_used_mb||0)/1024).toFixed(1)} / ${((s.mem_total_mb||0)/1024).toFixed(1)} GB</span></div>`;
  for (const g of s.gpus || []) {
    html += `<div class="stat"><span class="k">GPU ${g.index} · ${esc(g.name)}</span><span class="v">${g.util_pct}% · ${((g.mem_used_mb||0)/1024).toFixed(1)}/${((g.mem_total_mb||0)/1024).toFixed(0)} GB · ${g.temp_c}°C</span></div>`;
  }
  $("#sys-stats").innerHTML = html;
}

const STATE_LABEL = { running: "Running", starting: "Starting…", stopping: "Stopping…", stopped: "Stopped", failed: "Failed", unknown: "Unknown" };

function renderServices(services) {
  const el = $("#services");
  el.innerHTML = "";
  for (const svc of services) {
    const card = document.createElement("div");
    card.className = "service state-" + svc.state;
    card.innerHTML = `
      <div class="service-head">
        <span class="sname">${esc(svc.name)}</span>
        <span class="badge state-${svc.state}">${STATE_LABEL[svc.state] || svc.state}</span>
      </div>
      ${svc.hint ? `<p class="hint">${esc(svc.hint)}</p>` : ""}`;
    const btn = document.createElement("button");
    btn.className = "btn small";
    if (svc.can_start) { btn.textContent = "Start"; btn.onclick = () => svcAction(svc.id, "start", card); }
    else if (svc.can_stop) { btn.textContent = "Stop"; btn.onclick = () => svcAction(svc.id, "stop", card); }
    else { btn.disabled = true; btn.textContent = STATE_LABEL[svc.state] || ""; }
    card.appendChild(btn);
    el.appendChild(card);
  }
}

async function svcAction(id, action, card) {
  const btn = card.querySelector("button");
  btn.disabled = true;
  try {
    await api(`/desktop/api/services/${encodeURIComponent(id)}/${action}`, { method: "POST" });
  } catch (e) { /* surfaced on next poll */ }
  fastPollUntil = Date.now() + FAST_POLL_WINDOW_MS; // track starting/stopping/failed transitions quickly
  poll();
}

let lastModelKey = null; // fingerprint of the rendered list; skip re-render when unchanged

function modelFingerprint(models) {
  return models.map((m) => m.id + ":" + (m.state === "loaded" ? 1 : 0)).join("|");
}

async function refreshModels() {
  let models = [];
  try {
    const res = await api("/desktop/api/models");
    if (!res.ok) {
      setModelMsg((await res.json()).error || "model list unavailable");
      $("#model-card").classList.remove("hidden");
      return;
    }
    models = (await res.json()).models || [];
  } catch (e) { return; }
  $("#model-card").classList.remove("hidden");

  const key = modelFingerprint(models);
  if (key === lastModelKey) return; // list unchanged: keep options and the user's selection
  lastModelKey = key;

  const sel = $("#model-select");
  const prev = sel.value;
  sel.innerHTML = "";
  for (const m of models) {
    const opt = document.createElement("option");
    opt.value = m.id;
    let label = m.id + (m.quantization ? ` (${m.quantization})` : "");
    if (m.state === "loaded") label += " — loaded";
    opt.textContent = label;
    sel.appendChild(opt);
  }
  if (models.some((m) => m.id === prev)) sel.value = prev; // preserve selection across real changes
  const loaded = models.find((m) => m.state === "loaded");
  $("#loaded-chip").textContent = loaded ? `Loaded: ${loaded.id}` : "No model loaded";
}

function renderLinks(links) {
  const el = $("#links");
  el.innerHTML = "";
  for (const l of links) {
    const a = document.createElement("a");
    a.href = l.url;
    a.target = "_blank";
    a.rel = "noopener";
    a.textContent = l.name;
    el.appendChild(a);
  }
}

function renderSSH(d) {
  const line = $("#ssh-line");
  if (!d.hostname || !d.ip) { line.classList.add("hidden"); return; }
  const user = d.ssh_user ? d.ssh_user + "@" : "";
  line.innerHTML = `ssh ${esc(user)}${esc(d.hostname)} <span class="muted">(${esc(d.ip)})</span>`;
  const btn = document.createElement("button");
  btn.className = "copy-btn";
  btn.textContent = "copy";
  btn.onclick = () => {
    navigator.clipboard.writeText(`ssh ${user}${d.hostname}`).then(() => {
      btn.textContent = "copied!";
      setTimeout(() => (btn.textContent = "copy"), 1200);
    });
  };
  line.appendChild(btn);
  line.classList.remove("hidden");
}

function hideDesktopPanels() {
  for (const id of ["system-card", "services-card", "model-card"]) $(`#${id}`).classList.add("hidden");
}

/* ---------- actions ---------- */

$("#wake-btn").addEventListener("click", async () => {
  try {
    const res = await api("/wake", { method: "POST" });
    if (!res.ok) { alert(((await res.json()).error || "wake failed")); return; }
    wakeDeadline = Date.now() + WAKE_TIMEOUT_MS;
    poll();
  } catch (e) { /* auth gate or network */ }
});

$("#load-btn").addEventListener("click", async () => {
  const model = $("#model-select").value;
  if (!model) return;
  setModelMsg(`Loading ${model}…`);
  fastPollUntil = Date.now() + FAST_POLL_WINDOW_MS; // track load progress at a quick cadence
  try {
    const res = await api("/desktop/api/models/load", { method: "POST", body: JSON.stringify({ model }), timeoutMs: LOAD_TIMEOUT_MS });
    if (!res.ok) setModelMsg((await res.json()).error || "load failed");
    else setModelMsg(`Loading ${model} — give it a moment, then check back.`);
  } catch (e) {
    // The agent keeps the load going after we stop waiting; polling shows it once done.
    if (e.name === "TimeoutError") setModelMsg("Still loading in the background — check back in a moment.");
  }
  setTimeout(refreshModels, 3000);
});

$("#unload-btn").addEventListener("click", async () => {
  setModelMsg("Unloading…");
  fastPollUntil = Date.now() + FAST_POLL_WINDOW_MS;
  try {
    const res = await api("/desktop/api/models/unload", { method: "POST" });
    if (!res.ok) setModelMsg((await res.json()).error || "unload failed");
    else setModelMsg("Unload requested.");
  } catch (e) {}
  setTimeout(refreshModels, 2000);
});

function setModelMsg(msg) { $("#model-msg").textContent = msg; if (msg) setTimeout(() => { if ($("#model-msg").textContent === msg) $("#model-msg").textContent = ""; }, 8000); }

/* ---------- boot ---------- */

let pollTimer = null;
let fastPollUntil = 0; // epoch ms until which we poll at FAST_POLL_MS

function currentInterval() {
  const now = Date.now();
  if (now < fastPollUntil) return FAST_POLL_MS;
  const waking = !desktopOnline && ((lastStatus && lastStatus.waking && lastStatus.waking.active) || wakeDeadline > now);
  return waking ? FAST_POLL_MS : pollMs;
}

function schedulePoll() {
  clearTimeout(pollTimer);
  pollTimer = setTimeout(async () => { await poll(); schedulePoll(); }, currentInterval());
}

if (!token) showGate();
schedulePoll();
setInterval(tickTimer, 1000); // keep the wake timer ticking between polls

if ("serviceWorker" in navigator) {
  // Works over https and over http on private/tailnet IPs (secure contexts).
  navigator.serviceWorker.register("/sw.js").catch(() => {});
}
