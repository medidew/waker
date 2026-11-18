// waker dashboard UI — a single classic (non-module) TypeScript file,
// compiled by `npm run build` (tsc) to web/app.js and go:embedded into the
// wakeserver binary.
//
// Constraints (see README):
//   - talks ONLY to the wakeserver (same origin); the desktop agent is never
//     contacted from the browser — Go proxies every agent call;
//   - all dynamic text enters the DOM via textContent (model ids and service
//     names are untrusted agent data — never innerHTML);
//   - the plain HTML forms stay as the no-JS path; this script only intercepts
//     their submits and reroutes them to the /api/ JSON endpoints, so a
//     successful action never reloads the page.

// Payload types mirror the Go structs. pageData has no JSON tags, so the top
// level uses Go field names; the nested agent/lmstudio types use their tags.

interface GPU {
  index: number;
  name: string;
  util_pct: number;
  mem_used_mb: number;
  mem_total_mb: number;
  temp_c: number;
}

interface SysStats {
  uptime_sec: number;
  load1: number;
  load5: number;
  load15: number;
  mem_total_mb: number;
  mem_used_mb: number;
  gpus?: GPU[];
}

interface ServiceView {
  id: string;
  name: string;
  state: string;
  hint?: string;
  can_start: boolean;
  can_stop: boolean;
}

interface Model {
  id: string;
  type?: string;
  arch?: string;
  quantization?: string;
  state?: string; // "loaded" | "not-loaded"
  max_context_length?: number;
}

interface LinkRow {
  Name: string;
  Href: string;
  Service: string;
  State: string;
}

interface PageData {
  DesktopName: string;
  Online: boolean;
  Waking: boolean;
  WakeSeconds: number;
  RefreshSec: number;
  Sys: SysStats;
  StatsOK: boolean;
  Services: ServiceView[] | null;
  Models: Model[] | null;
  ModelsOK: boolean;
  Loaded: Model | null;
  AgentErr: string;
  ModelErr: string;
  Links: LinkRow[] | null;
  SSH: string;
  Flash: string; // stream frames clear this; flashes come from action responses
  ServerAge: number;
}

// ---- presentation helpers (mirror the Go template funcs in main.go) ----

function mss(sec: number): string {
  // 47 -> "0:47"
  return `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, "0")}`;
}

function uptime(sec: number): string {
  const d = Math.floor(sec / 86400);
  const h = Math.floor(sec / 3600) % 24;
  const m = Math.floor(sec / 60) % 60;
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

function gb(mb: number): string {
  return (mb / 1024).toFixed(1);
}

const STATE_LABELS: Record<string, string> = {
  running: "Running",
  starting: "Starting…",
  stopping: "Stopping…",
  stopped: "Stopped",
  failed: "Failed",
  unknown: "Unknown",
};

function stateLabel(s: string): string {
  return STATE_LABELS[s] ?? s;
}

// ---- DOM helpers ----

function $(id: string): HTMLElement {
  const el = document.getElementById(id);
  if (!el) throw new Error(`missing #${id}`);
  return el;
}

function el<K extends keyof HTMLElementTagNameMap>(tag: K, cls?: string): HTMLElementTagNameMap[K] {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  return n;
}

function text<K extends keyof HTMLElementTagNameMap>(tag: K, value: string, cls?: string): HTMLElementTagNameMap[K] {
  const n = el(tag, cls);
  n.textContent = value; // untrusted values stay text-only, never HTML
  return n;
}

// ---- renderers (each re-renders its slice in place; no navigation) ----

function renderPill(d: PageData): void {
  const pill = $("presence-pill");
  pill.className = d.Online ? "pill online" : "pill offline";
  const word = d.Online ? "online" : "offline";
  pill.replaceChildren(el("span", "dot"), text("span", `${d.DesktopName} · ${word}`));
}

function renderWake(d: PageData): void {
  const sub = $("wake-subtitle");
  if (d.Online) sub.textContent = "Desktop is awake.";
  else if (d.Waking) {
    sub.textContent = `Waiting for the desktop to join the tailnet — ${mss(d.WakeSeconds)} in, can take up to ~2 minutes.`;
  } else {
    sub.textContent = "Sends a Wake-on-LAN packet and waits for it to come online.";
  }
  const btn = $("wake-btn") as HTMLButtonElement;
  if (d.Online) {
    btn.textContent = "● Online";
    btn.disabled = true;
  } else if (d.Waking) {
    btn.textContent = "Waking…";
    btn.disabled = true;
  } else {
    btn.textContent = "Wake desktop";
    btn.disabled = false;
  }
}

function stat(label: string, value: string): HTMLDivElement {
  const n = el("div", "stat");
  n.append(text("span", label, "k"), text("span", value, "v"));
  return n;
}

function memStat(s: SysStats): HTMLDivElement {
  const n = el("div", "stat");
  const bar = el("div");
  bar.style.width = s.mem_total_mb === 0 ? "" : `${Math.floor((100 * s.mem_used_mb) / s.mem_total_mb)}%`;
  const wrap = el("span", "bar");
  wrap.appendChild(bar);
  n.append(text("span", "Memory", "k"), wrap, text("span", `${gb(s.mem_used_mb)} / ${gb(s.mem_total_mb)} GB`, "v"));
  return n;
}

function gpuStat(g: GPU): HTMLDivElement {
  return stat(`GPU ${g.index} · ${g.name}`, `${g.util_pct}% · ${gb(g.mem_used_mb)}/${gb(g.mem_total_mb)} GB · ${g.temp_c}°C`);
}

function renderSystem(d: PageData): void {
  const ok = d.Online && d.StatsOK;
  $("system-err-card").hidden = !(d.Online && d.AgentErr !== "");
  if (d.Online && d.AgentErr !== "") {
    $("agent-err-text").textContent = `${d.AgentErr} — retrying on the next refresh.`;
  }
  $("system-card").hidden = !ok;
  $("services-card").hidden = !ok;
  if (!ok) return;
  const s = d.Sys;
  $("stat-grid").replaceChildren(
    stat("Uptime", uptime(s.uptime_sec)),
    stat("Load (1/5/15)", `${s.load1.toFixed(2)} / ${s.load5.toFixed(2)} / ${s.load15.toFixed(2)}`),
    memStat(s),
    ...(s.gpus ?? []).map(gpuStat),
  );
}

let servicesSig = "";
function renderServices(d: PageData, force = false): void {
  // Rebuild only when the service set or any state changed, so a click never
  // races with a tick that would reset the buttons mid-interaction.
  const sig = JSON.stringify(d.Services ?? []);
  if (!force && sig === servicesSig) return;
  servicesSig = sig;
  $("service-list").replaceChildren(...(d.Services ?? []).map(serviceRow));
}

function serviceRow(sv: ServiceView): HTMLDivElement {
  const row = el("div", `service state-${sv.state}`);
  const head = el("div", "service-head");
  head.append(text("span", sv.name, "sname"), text("span", stateLabel(sv.state), `badge state-${sv.state}`));
  row.appendChild(head);
  if (sv.hint) row.appendChild(text("p", sv.hint, "hint"));
  const actions: Array<[string, string, boolean]> = [
    ["start", "Start", sv.can_start],
    ["stop", "Stop", sv.can_stop],
  ];
  const btnRow = el("div", "btn-row");
  for (const [action, label, enabled] of actions) {
    const form = el("form");
    form.setAttribute("method", "post");
    form.setAttribute("action", `/desktop/services/${encodeURIComponent(sv.id)}/${action}`);
    const btn = text("button", label, "btn small");
    btn.disabled = !enabled;
    form.appendChild(btn);
    btnRow.appendChild(form);
  }
  row.appendChild(btnRow);
  return row;
}

let modelsSig = "";
function renderModel(d: PageData): void {
  $("model-card").hidden = !d.Online;
  if (!d.Online) return;
  $("loaded-chip").textContent = d.Loaded ? `Loaded: ${d.Loaded.id}` : "No model loaded";
  const err = $("model-err");
  err.textContent = d.ModelErr;
  err.hidden = d.ModelErr === "";
  const models = d.Models && d.Models.length > 0 ? d.Models : null;
  $("model-forms").hidden = models === null;
  if (!models) return;
  const sig = JSON.stringify(models) + "|" + (d.Loaded ? d.Loaded.id : "");
  if (sig === modelsSig) return;
  modelsSig = sig;
  const sel = $("model-select") as HTMLSelectElement;
  const current = sel.value;
  sel.replaceChildren(...models.map((m) => {
    const o = document.createElement("option");
    o.value = m.id;
    const parts = [m.id];
    if (m.quantization) parts.push(`(${m.quantization})`);
    if (m.state === "loaded") parts.push("— loaded");
    o.textContent = parts.join(" ");
    return o;
  }));
  if (d.Loaded) sel.value = d.Loaded.id;
  else if (models.some((m) => m.id === current)) sel.value = current;
}

let linksSig = "";
function renderLinks(d: PageData): void {
  const sig = JSON.stringify(d.Links ?? []);
  if (sig !== linksSig) {
    linksSig = sig;
    $("link-list").replaceChildren(...(d.Links ?? []).map(linkRowEl));
  }
  const ssh = $("ssh");
  ssh.textContent = d.SSH;
  ssh.hidden = d.SSH === "";
}

function linkRowEl(l: LinkRow): HTMLDivElement {
  const row = el("div", l.State ? `link-row state-${l.State}` : "link-row");
  const a = document.createElement("a");
  a.href = l.Href;
  a.target = "_blank";
  a.rel = "noopener";
  a.textContent = l.Name;
  row.appendChild(a);
  if (l.State) row.appendChild(text("span", stateLabel(l.State), `badge state-${l.State}`));
  return row;
}

// ---- one-shot flash + stream indicator ----

let flashTimer = 0;
function showFlash(msg: string): void {
  const f = $("flash");
  f.textContent = msg;
  f.hidden = false;
  window.clearTimeout(flashTimer);
  flashTimer = window.setTimeout(() => {
    f.hidden = true;
  }, 8000);
}

function setStream(ok: boolean): void {
  const s = $("stream-state");
  s.hidden = ok;
  if (!ok) s.textContent = "stream disconnected";
}

// ---- actions: intercept form submits, call the /api/ twins, no navigation ----

let last: PageData | null = null;

function apiRouteFor(action: string): string | null {
  if (action === "/wake") return "/api/wake";
  if (action === "/desktop/models/load") return "/api/models/load";
  if (action === "/desktop/models/unload") return "/api/models/unload";
  if (action.startsWith("/desktop/services/")) return action.replace("/desktop/", "/api/");
  return null;
}

function onFormSubmit(ev: Event): void {
  if (!(ev.target instanceof HTMLFormElement)) return;
  const route = apiRouteFor(ev.target.getAttribute("action") ?? "");
  if (!route) return; // logout (and anything unknown) keep the classic navigation
  ev.preventDefault();
  void submitViaApi(ev.target, route);
}

async function submitViaApi(form: HTMLFormElement, route: string): Promise<void> {
  const btn = form.querySelector("button");
  if (btn) btn.disabled = true;
  try {
    let body: string | undefined;
    let headers: Record<string, string> | undefined;
    if (route === "/api/models/load") {
      const sel = form.querySelector<HTMLSelectElement>("select[name=model]");
      if (!sel || sel.value === "") {
        showFlash("no model selected");
        return;
      }
      body = JSON.stringify({ model: sel.value });
      headers = { "Content-Type": "application/json" };
    }
    const res = await fetch(route, { method: "POST", credentials: "same-origin", body, headers });
    if (!res.ok) {
      let msg = `request failed (${res.status})`;
      try {
        const j = (await res.json()) as { error?: string };
        if (j.error) msg = j.error;
      } catch {
        // non-JSON error body; keep the status message
      }
      showFlash(msg);
    }
  } catch (err) {
    showFlash(err instanceof Error ? err.message : "network error");
  } finally {
    // The next stream frame renders the true state; meanwhile a failed action
    // must never leave its button stuck disabled.
    if (btn) btn.disabled = false;
    if (last) renderServices(last);
  }
}

// ---- wiring ----

function render(d: PageData): void {
  renderPill(d);
  renderWake(d);
  renderSystem(d);
  renderServices(d);
  renderModel(d);
  renderLinks(d);
}

function init(): void {
  const es = new EventSource("/api/stream");
  es.addEventListener("status", (ev) => {
    if (!(ev instanceof MessageEvent)) return;
    try {
      const d = JSON.parse(ev.data) as PageData;
      last = d;
      render(d);
    } catch {
      // ignore a malformed frame; the next one renders
    }
  });
  es.addEventListener("open", () => setStream(true));
  es.addEventListener("error", () => setStream(false));
  document.addEventListener("submit", onFormSubmit);
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", init);
} else {
  init();
}
