import { createSignal, createResource, createEffect, For, Index, Show, onCleanup, onMount, createMemo } from "solid-js";
import cytoscape from "cytoscape";
import fcose from "cytoscape-fcose";
cytoscape.use(fcose);

const MATRIX_POLL = 5000;
const TABS = ["matrix", "mesh", "share", "settings"];

async function fetchMatrix() {
  const r = await fetch("/api/matrix");
  if (!r.ok) throw new Error("HTTP " + r.status);
  return r.json();
}

// call one node's syncthing REST via the hub relay (key injected server-side)
async function relay(node, restPath, params) {
  let url = `/api/node/${encodeURIComponent(node)}/${restPath}`;
  if (params) url += "?" + new URLSearchParams(params).toString();
  const r = await fetch(url);
  if (!r.ok) throw new Error(restPath + " -> " + r.status);
  return r.json();
}

async function fetchDisk() {
  const r = await fetch("/api/disk");
  if (!r.ok) throw new Error("HTTP " + r.status);
  return r.json();
}

const pct = (n) => (n == null ? "?" : Math.floor(n) + "%");
function bytes(n) {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(n < 10 && i > 0 ? 1 : 0) + " " + u[i];
}
const speed = (bps) => (bps > 0 ? bytes(bps) + "/s" : "—");
const completion = (g, need) => (g <= 0 || need <= 0 ? 100 : Math.max(0, (g - need) / g * 100));

const diskColor = (p) => (p >= 90 ? "bg-red-500" : p >= 75 ? "bg-amber-500" : "bg-emerald-500");

// compact disk usage bar; u = {total,used,avail,pct,mount,err,missing}
//
// `missing` is loud on purpose: it means the node's share root isn't there, i.e.
// the drive is gone. This used to render as a healthy green bar, because the
// backend fell back to df / and reported the boot media instead.
function DiskBar(props) {
  return (
    <Show when={props.u} fallback={<span class="text-[10px] text-slate-600">disk —</span>}>
      <Show when={!props.u.missing} fallback={
        <span class="rounded bg-red-900/70 px-1 py-0.5 text-[10px] font-semibold text-red-200 ring-1 ring-red-500"
              title={props.u.err}>⚠ DRIVE MISSING</span>
      }>
        <Show when={!props.u.err} fallback={<span class="text-[10px] text-slate-600" title={props.u.err}>disk n/a</span>}>
          <div title={`${bytes(props.u.avail)} free of ${bytes(props.u.total)} (${props.u.pct}% used) on ${props.u.mount}`}>
            <div class="h-1.5 w-full overflow-hidden rounded bg-slate-800">
              <div class={"h-full " + diskColor(props.u.pct)} style={{ width: props.u.pct + "%" }} />
            </div>
            <div class="mt-0.5 text-[10px] text-slate-500">{props.u.pct}% · {bytes(props.u.avail)} free</div>
          </div>
        </Show>
      </Show>
    </Show>
  );
}

function cellStyle(cell, online) {
  if (!online) return { bg: "bg-slate-800/40 text-slate-600", label: "—" };
  if (!cell || !cell.present) return { bg: "bg-slate-900 text-slate-700", label: "·" };
  if (cell.errors && cell.errors.length || cell.state === "error")
    return { bg: "bg-red-900/70 text-red-200 ring-1 ring-red-500", label: "err" };
  if (cell.state === "paused") return { bg: "bg-slate-700/50 text-slate-400", label: "pause" };
  if (cell.state === "syncing") return { bg: "bg-sky-800/70 text-sky-100", label: pct(cell.completion) };
  if (cell.state === "scanning") return { bg: "bg-amber-800/60 text-amber-100", label: "scan" };
  if (cell.completion >= 99.95) return { bg: "bg-emerald-800/60 text-emerald-100", label: "100" };
  return { bg: "bg-amber-800/50 text-amber-100", label: pct(cell.completion) };
}

export default function App() {
  const [data, { refetch }] = createResource(fetchMatrix);
  const [disk, { refetch: refetchDisk }] = createResource(fetchDisk);
  const [sel, setSel] = createSignal(null); // {folder, device?, tab?}
  const [tab, setTab] = createSignal("matrix"); // matrix | mesh | share | settings
  const shareMode = () => tab() === "share";
  const [busy, setBusy] = createSignal(null); // status string
  const [confirm, setConfirm] = createSignal(null); // {folder, target}
  const t = setInterval(refetch, MATRIX_POLL);
  const td = setInterval(refetchDisk, 60000);
  onCleanup(() => { clearInterval(t); clearInterval(td); });

  async function action(kind, folder, target) {
    setBusy(`${kind === "share" ? "sharing" : "unsharing"} ${folder.label} → ${target}…`);
    try {
      const r = await fetch("/api/" + kind, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ folder: folder.id, target }),
      });
      const j = await r.json();
      if (!r.ok) throw new Error(j.error || r.status);
      setBusy(`${kind === "share" ? "shared" : "unshared"} ${folder.label} → ${target}` +
        (j.targetPath ? ` (${j.targetPath})` : ""));
      await refetch();
      setTimeout(() => setBusy(null), 4000);
    } catch (e) {
      setBusy("error: " + String(e.message || e));
    }
  }
  const doShare = (folder, target) => action("share", folder, target);
  const askUnshare = (folder, target) => setConfirm({ folder, target });

  const onMatrix = () => tab() === "matrix";
  return (
    <div class="min-h-screen">
      <div class="p-6" classList={{ "pb-[58vh]": !!sel() && onMatrix() }}>
        <header class="mb-4 flex items-baseline gap-3">
          <h1 class="text-xl font-semibold text-slate-100">syncthing swarm</h1>
          <Show when={data()}>
            <span class="text-xs text-slate-500">
              {data().devices.length} devices · {data().folders.length} folders ·
              polled {new Date(data().generatedAt).toLocaleTimeString()}
            </span>
          </Show>
          <button onClick={() => refetch()} class="ml-auto rounded bg-slate-700 px-3 py-1 text-xs hover:bg-slate-600">refresh</button>
        </header>

        {/* menu bar */}
        <nav class="mb-4 flex gap-1 border-b border-slate-800">
          <For each={TABS}>
            {(name) => (
              <button onClick={() => { setTab(name); if (name !== "matrix") setSel(null); }}
                class="-mb-px border-b-2 px-4 py-2 text-sm capitalize transition-colors"
                classList={{
                  "border-sky-400 text-sky-300 font-medium": tab() === name,
                  "border-transparent text-slate-400 hover:text-slate-200": tab() !== name,
                }}>
                {name}
              </button>
            )}
          </For>
        </nav>

        <Show when={shareMode()}>
          <div class="mb-3 rounded bg-sky-950/40 px-3 py-2 text-xs text-sky-200">
            Share mode: check a box to share <b>{data()?.source}</b>'s folder to that device (one click);
            uncheck to stop sharing (asks first, keeps files).
          </div>
        </Show>
        <Show when={busy()}>
          <div class="mb-3 rounded px-3 py-2 text-xs"
            classList={{ "bg-red-900/60 text-red-200": busy().startsWith("error"), "bg-slate-800 text-slate-300": !busy().startsWith("error") }}>
            {busy()}
          </div>
        </Show>

        <Show when={data.error}>
          <div class="rounded bg-red-900/60 p-3 text-red-200">cannot reach swarmd: {String(data.error)}</div>
        </Show>

        {/* mesh renders even before the matrix poll returns */}
        <Show when={tab() === "mesh"}>
          <Mesh data={data()} />
        </Show>

        <Show when={tab() !== "mesh"}>
          <Show when={data()} fallback={<div class="text-slate-500">loading…</div>}>
            <Show when={tab() === "settings"}>
              <Settings data={data()} disk={disk()} />
            </Show>
            <Show when={onMatrix() || shareMode()}>
              <Matrix data={data()} sel={sel()} shareMode={shareMode()} disk={disk()}
                onFolder={(f) => onMatrix() && setSel({ folder: f, tab: "files" })}
                // an errored cell opens straight to the Errors tab — that is what you
                // clicked it to find out about
                onCell={(f, d) => {
                  const cell = (data()?.cells?.[f.id] || {})[d.name];
                  const bad = cell && (cell.state === "error" || cell.errors?.length);
                  setSel({ folder: f, device: d.name, tab: bad ? "errors" : "transfers" });
                }}
                onShare={doShare} onUnshare={askUnshare} />
            </Show>
          </Show>
        </Show>
      </div>

      <Show when={sel() && onMatrix()}>
        <Dock sel={sel()} data={data()} onClose={() => setSel(null)} />
      </Show>

      <Show when={confirm()}>
        <div class="fixed inset-0 z-30 flex items-center justify-center bg-black/50" onClick={() => setConfirm(null)}>
          <div class="w-96 rounded-lg border border-slate-700 bg-[#11151f] p-5" onClick={(e) => e.stopPropagation()}>
            <div class="text-slate-100">Stop sharing <b>{confirm().folder.label}</b> to <b>{confirm().target}</b>?</div>
            <div class="mt-2 text-xs text-slate-400">Removes the folder from {confirm().target}'s config and stops syncing. Files already on disk are kept.</div>
            <div class="mt-4 flex justify-end gap-2">
              <button onClick={() => setConfirm(null)} class="rounded bg-slate-700 px-3 py-1 text-sm hover:bg-slate-600">cancel</button>
              <button onClick={() => { const c = confirm(); setConfirm(null); action("unshare", c.folder, c.target); }}
                class="rounded bg-red-700 px-3 py-1 text-sm text-white hover:bg-red-600">unshare</button>
            </div>
          </div>
        </div>
      </Show>
    </div>
  );
}

// basename of a syncthing item path (forward slashes even on windows nodes)
const baseName = (p) => (p ? p.split("/").pop() : "");

// Mesh: the swarm as a live graph. Nodes = devices (managed vs peer, online/
// error), edges = connections from /api/mesh. A Server-Sent-Events stream from
// /api/events drives the live layer: a node pulses while it's scanning/syncing,
// its label shows folder › file, incident edges light up, and errors flash red —
// each held for 10s after the last event. Clicking a node or edge opens a popup
// of the files recently seen moving through that link.
//
// Attribution note: syncthing events are per-node+folder+file, not per-edge, so
// activity is shown on the busy NODE and pulsed across its incident edges rather
// than pinned to one exact peer — the honest resolution the event data supports.
function Mesh(props) {
  let container;
  let cy, es;
  const [popup, setPopup] = createSignal(null); // {title, files:[{folder,item,at,err}]}
  const [toast, setToast] = createSignal(null);  // {msg, err}
  const [ready, setReady] = createSignal(false);
  const [err, setErr] = createSignal(null);

  const activity = new Map();   // nodeId -> recent [{folder,item,at,err}]
  const nameToId = new Map();   // managed node name -> device id
  const idToName = new Map();   // device id -> display name
  const timers = new Map();     // key -> timeout id (debounced clears)

  const folderLabel = (id) => {
    const f = props.data?.folders?.find((f) => f.id === id);
    return (f && f.label) || id || "";
  };
  const schedule = (key, ms, fn) => {
    clearTimeout(timers.get(key));
    timers.set(key, setTimeout(fn, ms));
  };
  const flashToast = (msg, isErr) => {
    setToast({ msg, err: isErr });
    schedule("toast", isErr ? 6000 : 3500, () => setToast(null));
  };

  const recentFor = (ids) => {
    const rows = [];
    for (const id of ids) for (const r of activity.get(id) || []) rows.push({ ...r, node: idToName.get(id) });
    return rows.sort((a, b) => b.at - a.at).slice(0, 30);
  };

  function handleEvent(ev) {
    const id = nameToId.get(ev.node);
    if (!id || !cy) return;
    const node = cy.getElementById(id);
    if (!node || node.empty()) return;

    const isFile = ev.type === "ItemStarted" || ev.type === "ItemFinished";
    const isErr = !!ev.error || (ev.type === "FolderErrors");
    const isActive = isFile ||
      (ev.type === "StateChanged" && ["scanning", "syncing", "sync-preparing", "scan-waiting"].includes(ev.state));

    if (isFile && ev.item) {
      const list = activity.get(id) || [];
      list.unshift({ folder: folderLabel(ev.folder), item: ev.item, at: Date.now(), err: ev.error });
      activity.set(id, list.slice(0, 25));
    }
    if (isErr) {
      node.addClass("errored");
      flashToast(`⚠ ${ev.node}: ${baseName(ev.item) || folderLabel(ev.folder)} — ${ev.error || "folder error"}`, true);
      schedule(id + ":err", 12000, () => node.removeClass("errored"));
    }
    if (isActive) {
      const label = folderLabel(ev.folder) + (ev.item ? " › " + baseName(ev.item) : "");
      node.data("disp", idToName.get(id) + "\n" + label).addClass("active");
      node.connectedEdges().addClass("active");
      schedule(id, 10000, () => {
        node.removeClass("active").data("disp", idToName.get(id));
        node.connectedEdges().removeClass("active");
      });
    }
    if (ev.type === "ItemFinished" && !ev.error) flashToast(`${ev.node} ✓ ${baseName(ev.item)}`, false);
  }

  onMount(async () => {
    let g;
    try {
      g = await fetch("/api/mesh").then((r) => { if (!r.ok) throw new Error("HTTP " + r.status); return r.json(); });
    } catch (e) { setErr(String(e.message || e)); return; }

    const els = [];
    for (const n of g.nodes) {
      idToName.set(n.id, n.name);
      if (n.managed) nameToId.set(n.name, n.id);
      els.push({ data: { id: n.id, name: n.name, disp: n.name, managed: !!n.managed, online: !!n.online, error: !!n.error } });
    }
    for (const e of g.edges) {
      els.push({ data: { id: e.a + "~" + e.b, source: e.a, target: e.b, connected: !!e.connected, type: e.type || "" } });
    }

    cy = cytoscape({
      container,
      elements: els,
      wheelSensitivity: 0.2,
      style: [
        { selector: "node", style: {
            "label": "data(disp)", "color": "#cbd5e1", "font-size": 11, "text-wrap": "wrap", "text-max-width": 140,
            "text-valign": "bottom", "text-margin-y": 4, "width": 26, "height": 26,
            "background-color": "#475569", "border-width": 2, "border-color": "#334155" } },
        { selector: "node[?managed]", style: { "width": 34, "height": 34, "background-color": "#0ea5e9", "border-color": "#0369a1" } },
        { selector: "node[?online]", style: { "border-color": "#10b981" } },
        { selector: "node[!online]", style: { "background-color": "#3f4655", "opacity": 0.7 } },
        { selector: "node.active", style: { "border-color": "#34d399", "border-width": 4, "background-color": "#059669", "color": "#ecfdf5", "font-weight": "bold" } },
        { selector: "node.errored", style: { "border-color": "#f87171", "border-width": 4, "background-color": "#b91c1c" } },
        { selector: "edge", style: {
            "width": 2, "line-color": "#334155", "curve-style": "bezier",
            "target-arrow-shape": "none" } },
        { selector: "edge[?connected]", style: { "line-color": "#64748b", "width": 2.5 } },
        { selector: "edge[!connected]", style: { "line-color": "#334155", "line-style": "dashed", "opacity": 0.5 } },
        { selector: "edge.hover", style: { "width": 6, "line-color": "#93c5fd" } },
        { selector: "edge.active", style: { "width": 5, "line-color": "#34d399" } },
      ],
      layout: { name: "fcose", animate: true, animationDuration: 500, nodeSeparation: 90, idealEdgeLength: 120 },
    });

    cy.on("mouseover", "edge", (e) => { e.target.addClass("hover"); container.style.cursor = "pointer"; });
    cy.on("mouseout", "edge", (e) => { e.target.removeClass("hover"); container.style.cursor = "default"; });
    cy.on("mouseover", "node", () => { container.style.cursor = "pointer"; });
    cy.on("mouseout", "node", () => { container.style.cursor = "default"; });
    cy.on("tap", "edge", (e) => {
      const a = e.target.data("source"), b = e.target.data("target");
      setPopup({ title: `${idToName.get(a)} ↔ ${idToName.get(b)}`, files: recentFor([a, b]) });
    });
    cy.on("tap", "node", (e) => {
      const id = e.target.id();
      setPopup({ title: idToName.get(id) || id, files: recentFor([id]) });
    });
    cy.on("tap", (e) => { if (e.target === cy) setPopup(null); }); // click background = close

    setReady(true);

    es = new EventSource("/api/events");
    es.onmessage = (m) => { try { handleEvent(JSON.parse(m.data)); } catch {} };
    es.onerror = () => {}; // EventSource auto-reconnects
  });

  onCleanup(() => {
    for (const t of timers.values()) clearTimeout(t);
    es && es.close();
    cy && cy.destroy();
  });

  return (
    <div class="relative">
      <Show when={err()}>
        <div class="rounded bg-red-900/60 p-3 text-red-200">cannot load mesh: {err()}</div>
      </Show>
      <div class="mb-2 flex flex-wrap items-center gap-3 text-[11px] text-slate-400">
        <span><span class="mr-1 inline-block h-3 w-3 rounded-full bg-sky-500 align-middle" />managed node</span>
        <span><span class="mr-1 inline-block h-3 w-3 rounded-full bg-slate-500 align-middle" />peer device</span>
        <span><span class="mr-1 inline-block h-2 w-2 rounded-full ring-2 ring-emerald-400 align-middle" />online</span>
        <span><span class="mr-1 inline-block h-1 w-4 bg-emerald-400 align-middle" />live sync</span>
        <span class="text-slate-500">click a node or edge for recently-synced files</span>
      </div>
      <div ref={container} class="h-[70vh] w-full rounded-lg border border-slate-800 bg-[#0b0e14]" />
      <Show when={!ready() && !err()}>
        <div class="absolute inset-0 flex items-center justify-center text-slate-500">building graph…</div>
      </Show>

      {/* recently-synced files popup */}
      <Show when={popup()}>
        <div class="absolute right-3 top-10 z-20 w-80 rounded-lg border border-slate-700 bg-[#11151f] p-3 shadow-xl">
          <div class="mb-2 flex items-center justify-between">
            <div class="text-sm font-medium text-slate-100">{popup().title}</div>
            <button class="text-slate-500 hover:text-slate-200" onClick={() => setPopup(null)}>✕</button>
          </div>
          <Show when={popup().files.length} fallback={<div class="text-xs text-slate-500">no sync activity seen yet on this link (since you opened the mesh).</div>}>
            <ul class="max-h-72 space-y-1 overflow-y-auto text-xs">
              <For each={popup().files}>
                {(r) => (
                  <li class="flex flex-col border-b border-slate-800 pb-1">
                    <span classList={{ "text-red-300": !!r.err, "text-slate-200": !r.err }}>{baseName(r.item)}</span>
                    <span class="text-slate-500">{r.node} · {r.folder} · {new Date(r.at).toLocaleTimeString()}</span>
                  </li>
                )}
              </For>
            </ul>
          </Show>
        </div>
      </Show>

      {/* in-page sync toast */}
      <Show when={toast()}>
        <div class="absolute bottom-3 left-1/2 z-20 -translate-x-1/2 rounded-full px-4 py-1.5 text-xs shadow-lg"
          classList={{ "bg-red-800 text-red-100": toast().err, "bg-slate-800 text-emerald-200": !toast().err }}>
          {toast().msg}
        </div>
      </Show>
    </div>
  );
}

function Matrix(props) {
  const d = () => props.data;
  const selId = () => props.sel?.folder?.id;
  return (
    <div class="overflow-x-auto rounded-lg border border-slate-800">
      <table class="border-collapse text-sm">
        <thead>
          <tr>
            <th class="sticky left-0 z-10 bg-[#0b0e14] p-2 text-left font-medium text-slate-400">folder \ device</th>
            <For each={d().devices}>
              {(dev) => (
                <th class="min-w-[120px] border-l border-slate-800 p-2 align-bottom">
                  <div class="flex items-center gap-1.5">
                    <span class={"inline-block h-2 w-2 rounded-full " + (dev.online ? "bg-emerald-400" : "bg-slate-600")}
                      title={dev.online ? "online" : "offline"} />
                    <span class="font-semibold text-slate-200">{dev.name}</span>
                    <Show when={dev.systemErrors && dev.systemErrors.length}>
                      <span class="rounded bg-red-600 px-1 text-[10px] text-white" title={dev.systemErrors.join("\n")}>{dev.systemErrors.length}!</span>
                    </Show>
                  </div>
                  <div class="mt-0.5 flex items-center gap-1.5 text-[10px] font-normal text-slate-500">
                    <span>{dev.version || "—"}</span>
                    <Show when={dev.url}>
                      <a href={dev.url} target="_blank" rel="noreferrer"
                        class="text-sky-400 hover:underline" title={"open " + dev.name + " syncthing GUI · " + dev.url}>⚙&nbsp;GUI↗</a>
                    </Show>
                  </div>
                  <div class="mt-1 w-full font-normal"><DiskBar u={props.disk?.[dev.name]} /></div>
                </th>
              )}
            </For>
          </tr>
        </thead>
        <tbody>
          <For each={d().folders}>
            {(f) => (
              <tr class="border-t border-slate-800" classList={{ "bg-slate-800/30": selId() === f.id }}>
                <td class="sticky left-0 z-10 bg-[#0b0e14] p-2">
                  <button class="text-left font-medium text-slate-200 hover:text-sky-300"
                    classList={{ "text-sky-300": selId() === f.id }}
                    onClick={() => props.onFolder(f)}>
                    {f.label}
                    <div class="text-[10px] font-normal text-slate-600">{f.id}</div>
                  </button>
                </td>
                <For each={d().devices}>
                  {(dev) => {
                    const cell = () => (d().cells[f.id] || {})[dev.name];
                    const s = () => cellStyle(cell(), dev.online);
                    const isSource = () => dev.name === d().source;
                    const sourceHas = () => !!(d().cells[f.id] || {})[d().source]?.present;
                    const shared = () => !!cell()?.present;
                    return (
                      <td class="border-l border-slate-800 p-1 text-center">
                        <Show when={props.shareMode} fallback={
                          <button class={"w-full rounded px-2 py-1.5 text-xs font-medium " + s().bg +
                            (cell() && cell().present ? " cursor-pointer hover:brightness-125" : " cursor-default")}
                            disabled={!cell() || !cell().present || !dev.online}
                            title={cell()?.errors?.length ? cell().errors.join("\n") : undefined}
                            onClick={() => props.onCell(f, dev)}>{s().label}</button>
                        }>
                          <input type="checkbox" class="h-4 w-4 accent-sky-500"
                            checked={isSource() ? sourceHas() : shared()}
                            disabled={isSource() || !sourceHas() || !dev.online}
                            title={isSource() ? d().source + " (source)"
                              : !sourceHas() ? d().source + " doesn't have this folder"
                              : !dev.online ? dev.name + " offline"
                              : shared() ? "shared — uncheck to stop" : "share to " + dev.name}
                            onChange={(e) => e.currentTarget.checked ? props.onShare(f, dev.name) : props.onUnshare(f, dev.name)} />
                        </Show>
                      </td>
                    );
                  }}
                </For>
              </tr>
            )}
          </For>
        </tbody>
      </table>
    </div>
  );
}

// ---------- bottom dock ----------

function Dock(props) {
  const folder = () => props.sel.folder;
  const [tab, setTab] = createSignal(props.sel.tab || "files");
  // devices online that have this folder present (source for browse/logs)
  const presentDevices = createMemo(() =>
    props.data.devices.filter((dev) => dev.online && (props.data.cells[folder().id] || {})[dev.name]?.present));
  const initialDev = () => {
    const want = props.sel.device;
    if (want && presentDevices().some((d) => d.name === want)) return want;
    return presentDevices()[0]?.name || props.data.devices.find((d) => d.online)?.name || "";
  };
  const [dev, setDev] = createSignal(initialDev());
  // reset source device when folder changes (or current source lost the folder)
  createEffect(() => {
    folder();
    if (!presentDevices().some((d) => d.name === dev())) setDev(presentDevices()[0]?.name || "");
  });
  // absolute path of this folder on the selected source node
  const [folderPath] = createResource(() => ({ n: dev(), f: folder().id }),
    (k) => (k.n ? relay(k.n, "rest/config/folders")
      .then((fs) => (fs.find((x) => x.id === k.f) || {}).path || "").catch(() => "") : ""));

  // how many errors this folder has on the selected node — drives the tab badge
  const errCount = () => ((props.data.cells[folder().id] || {})[dev()] || {}).errors?.length || 0;

  const Tab = (p) => (
    <button onClick={() => setTab(p.id)}
      class="border-b-2 px-3 py-1.5 text-sm"
      classList={{
        "border-sky-400 text-sky-300": tab() === p.id,
        "border-transparent text-slate-400 hover:text-slate-200": tab() !== p.id,
        "text-red-300": p.id === "errors" && errCount() > 0 && tab() !== p.id,
      }}>
      {p.label}
    </button>
  );

  return (
    <div class="fixed inset-x-0 bottom-0 z-20 h-[55vh] border-t border-slate-700 bg-[#0e1220] shadow-2xl flex flex-col">
      <div class="flex items-center gap-2 border-b border-slate-800 px-4">
        <span class="mr-2 font-semibold text-slate-100">{folder().label}</span>
        <span class="text-xs text-slate-500">{folder().id}</span>
        <div class="ml-3 flex">
          <Tab id="files" label="Files" />
          <Tab id="transfers" label="Transfers" />
          <Tab id="errors" label={errCount() ? `Errors (${errCount()})` : "Errors"} />
          <Tab id="logs" label="Logs" />
        </div>
        <div class="ml-auto flex items-center gap-2">
          <Show when={tab() !== "transfers"}>
            <label class="text-xs text-slate-500">source</label>
            <select value={dev()} onChange={(e) => setDev(e.currentTarget.value)}
              class="rounded border border-slate-700 bg-slate-800 px-2 py-1 text-xs text-slate-200">
              <Index each={presentDevices()}>{(d) => <option value={d().name}>{d().name}</option>}</Index>
            </select>
          </Show>
          <button onClick={props.onClose} class="text-slate-500 hover:text-slate-200">✕</button>
        </div>
      </div>
      <Show when={tab() !== "transfers" && folderPath()}>
        <div class="border-b border-slate-800 px-4 py-1.5 font-mono text-xs">
          <span class="text-slate-500">📂 </span>
          <span class="text-sky-300">{dev()}</span>
          <span class="text-slate-500">:</span>
          <span class="text-slate-200">{folderPath()}</span>
        </div>
      </Show>
      <div class="flex-1 overflow-auto p-4">
        <Show when={tab() === "files"}><FilesTab node={dev()} folder={folder().id} /></Show>
        <Show when={tab() === "transfers"}><TransfersTab data={props.data} folder={folder().id} /></Show>
        <Show when={tab() === "errors"}><ErrorsTab node={dev()} folder={folder().id} /></Show>
        <Show when={tab() === "logs"}><LogsTab node={dev()} folder={folder().id} /></Show>
      </div>
    </div>
  );
}

const isDir = (e) => e.type === "FILE_INFO_TYPE_DIRECTORY" || e.type === "directory";

function FilesTab(props) {
  const [root] = createResource(() => ({ n: props.node, f: props.folder }),
    (k) => relay(k.n, "rest/db/browse", { folder: k.f, levels: 1 }));
  return (
    <Show when={!root.loading} fallback={<div class="text-slate-500">loading tree…</div>}>
      <Show when={!root.error} fallback={<div class="text-red-300">{String(root.error)}</div>}>
        <ul class="font-mono text-[13px]">
          <For each={root()}>{(e) => <FileNode entry={e} node={props.node} folder={props.folder} path={e.name} depth={0} />}</For>
        </ul>
      </Show>
    </Show>
  );
}

function FileNode(props) {
  const [open, setOpen] = createSignal(false);
  const [kids] = createResource(open, () =>
    relay(props.node, "rest/db/browse", { folder: props.folder, prefix: props.path, levels: 1 }));
  const dir = isDir(props.entry);
  return (
    <li>
      <div class="flex items-center gap-2 rounded px-1 py-0.5 hover:bg-slate-800/60"
        style={{ "padding-left": props.depth * 16 + "px" }}
        onClick={() => dir && setOpen(!open())}
        classList={{ "cursor-pointer": dir }}>
        <span class="w-4 text-center text-slate-500">{dir ? (open() ? "▾" : "▸") : ""}</span>
        <span>{dir ? "📁" : "📄"}</span>
        <span class="text-slate-200">{props.entry.name}</span>
        <Show when={!dir}><span class="ml-auto text-xs text-slate-500">{bytes(props.entry.size)}</span></Show>
      </div>
      <Show when={dir && open()}>
        <Show when={!kids.loading} fallback={<div class="pl-8 text-xs text-slate-600">…</div>}>
          <ul><For each={kids()}>{(c) => (
            <FileNode entry={c} node={props.node} folder={props.folder} path={props.path + "/" + c.name} depth={props.depth + 1} />
          )}</For></ul>
        </Show>
      </Show>
    </li>
  );
}

function TransfersTab(props) {
  // live per-device status + derived download speed (needBytes delta over time)
  const [rows, setRows] = createSignal([]);
  let prev = {}; // name -> {need, t}
  // only devices that actually have this folder configured (avoids 404 spam)
  const devices = () => props.data.devices.filter(
    (d) => (props.data.cells[props.folder] || {})[d.name]?.present);

  async function tick() {
    const now = Date.now();
    const out = await Promise.all(devices().map(async (dev) => {
      const base = { name: dev.name, online: dev.online, present: false };
      if (!dev.online) return base;
      try {
        const st = await relay(dev.name, "rest/db/status", { folder: props.folder });
        const comp = completion(st.globalBytes, st.needBytes);
        const p = prev[dev.name];
        let bps = 0;
        if (p && st.needBytes < p.need) bps = (p.need - st.needBytes) / ((now - p.t) / 1000);
        prev[dev.name] = { need: st.needBytes, t: now };
        return { ...base, present: true, state: st.state, completion: comp,
          need: st.needBytes, needItems: st.needFiles || 0, global: st.globalBytes, bps };
      } catch { return base; }
    }));
    setRows(out);
  }

  createEffect(() => {
    props.folder; // re-run when folder changes
    prev = {};
    tick();
    const id = setInterval(tick, 1500);
    onCleanup(() => clearInterval(id));
  });

  const present = () => rows().filter((r) => r.present);
  const loading = () => present().filter((r) => r.completion < 99.95 || r.state === "syncing");
  const synced = () => present().filter((r) => !(r.completion < 99.95 || r.state === "syncing"));

  return (
    <div class="space-y-5">
      <div>
        <div class="mb-1 text-xs font-semibold uppercase tracking-wide text-sky-300">loading to ({loading().length})</div>
        <Show when={loading().length} fallback={<div class="text-sm text-slate-600">nothing transferring</div>}>
          <For each={loading()}>{(r) => (
            <div class="mb-2 rounded border border-slate-800 bg-slate-900/40 p-2">
              <div class="flex items-center gap-2 text-sm">
                <span class="font-medium text-slate-100">{r.name}</span>
                <span class="text-xs text-slate-500">{r.state}</span>
                <span class="ml-auto tabular-nums text-sky-300">{speed(r.bps)}</span>
              </div>
              <div class="mt-1 h-2 overflow-hidden rounded bg-slate-800">
                <div class="h-full bg-sky-500 transition-all" style={{ width: r.completion + "%" }} />
              </div>
              <div class="mt-1 flex justify-between text-[11px] text-slate-500">
                <span>{pct(r.completion)}</span>
                <span>{bytes(r.need)} · {r.needItems} items left</span>
              </div>
            </div>
          )}</For>
        </Show>
      </div>
      <div>
        <div class="mb-1 text-xs font-semibold uppercase tracking-wide text-emerald-300">present / in sync ({synced().length})</div>
        <div class="flex flex-wrap gap-2">
          <For each={synced()}>{(r) => (
            <span class="rounded bg-emerald-900/40 px-2 py-1 text-xs text-emerald-200">✓ {r.name} · {bytes(r.global)}</span>
          )}</For>
          <For each={rows().filter((r) => !r.online)}>{(r) => (
            <span class="rounded bg-slate-800/50 px-2 py-1 text-xs text-slate-500">○ {r.name} offline</span>
          )}</For>
        </div>
      </div>
    </div>
  );
}

// ---------- settings ----------

function Settings(props) {
  return (
    <div class="space-y-4">
      <div>
        <h2 class="text-lg font-semibold text-slate-100">Settings — devices & folder roots</h2>
        <p class="text-sm text-slate-500">Read-only overview of managed nodes (from swarm.yaml). Folder root = where each folder lives on that node's filesystem.</p>
      </div>
      <div class="grid gap-4 md:grid-cols-2 lg:grid-cols-3">
        <For each={props.data.devices}>{(dev) => <DeviceCard dev={dev} disk={props.disk?.[dev.name]} />}</For>
      </div>
    </div>
  );
}

function DeviceCard(props) {
  const [folders] = createResource(() => (props.dev.online ? props.dev.name : null),
    (n) => relay(n, "rest/config/folders").catch(() => []));
  return (
    <div class="rounded-lg border border-slate-800 bg-[#11151f] p-4">
      <div class="flex items-center gap-2">
        <span class={"h-2 w-2 rounded-full " + (props.dev.online ? "bg-emerald-400" : "bg-slate-600")} />
        <span class="font-semibold text-slate-100">{props.dev.name}</span>
        <span class="text-xs text-slate-500">{props.dev.version || "—"}</span>
        <Show when={props.dev.url}>
          <a href={props.dev.url} target="_blank" rel="noreferrer" class="ml-auto text-xs text-sky-400 hover:underline">⚙ open GUI ↗</a>
        </Show>
      </div>
      <div class="mt-1 break-all font-mono text-[10px] text-slate-600">{props.dev.deviceID || "—"}</div>
      <div class="mt-3">
        <div class="mb-1 text-xs font-semibold uppercase tracking-wide text-slate-400">disk</div>
        <DiskBar u={props.disk} />
      </div>
      <div class="mt-3 text-xs">
        <span class="text-slate-500">share root: </span>
        <span class="font-mono text-[11px]" classList={{ "text-slate-300": !!props.dev.root, "text-amber-400": !props.dev.root }}>
          {props.dev.root || "(unset — set `root:` in swarm.yaml to share here)"}
        </span>
      </div>
      <div class="mt-3 text-xs font-semibold uppercase tracking-wide text-slate-400">folder roots</div>
      <Show when={props.dev.online} fallback={<div class="mt-1 text-xs text-slate-600">offline</div>}>
        <ul class="mt-1 space-y-1.5">
          <For each={folders()} fallback={<li class="text-xs text-slate-600">loading…</li>}>
            {(f) => (
              <li class="text-xs">
                <span class="text-slate-300">{f.label || f.id}</span>
                <div class="break-all font-mono text-[11px] text-slate-500">{f.path}</div>
              </li>
            )}
          </For>
        </ul>
      </Show>
    </div>
  );
}

// Turn syncthing's raw error strings into something you can act on. Each entry:
// a title, what it actually means, and what to do about it.
//
// The raw message is always shown as well — this explains it, it does not hide it.
const ERROR_KINDS = [
  {
    match: /delete dir:.*directory not empty/i,
    title: "Local-only files are blocking a directory deletion",
    what:
      "Another node deleted this directory, so syncthing tried to delete it here too — but " +
      "the directory still contains files that exist ONLY on this node. Syncthing refuses to " +
      "delete files it has never seen anywhere else, so the removal fails and it retries forever.",
    why:
      "Usually this means the folder is receive-only and picked up files that were added " +
      "locally (or survived from an older copy of the folder), while the rest of the swarm " +
      "moved on and deleted the directory around them. Your files are NOT lost — they are " +
      "sitting right there, and syncthing is protecting them.",
    fix: [
      "Look at what is actually in those directories on this node (Files tab, or ssh in).",
      "If they are syncthing's own .sync-conflict-* copies (it saved this node's older version " +
        "of a file before overwriting it), they are almost always safe to delete — the swarm's " +
        "copy is the newer one. Use \"Delete conflict copies\" below.",
      "If you WANT to keep them: move them out of the folder first.",
    ],
    danger: "Both fixes DELETE files. Preview shows you exactly which ones — read that list first.",
    actions: ["clean-conflicts", "revert", "rescan"],
  },
  {
    match: /folder marker missing|marker/i,
    title: "The folder marker is missing — the drive is probably not mounted",
    what:
      "The .stfolder marker lives on the folder's own disk. Syncthing cannot find it, so it has " +
      "STOPPED this folder instead of treating every missing file as a deletion to propagate.",
    why: "Almost always an unmounted or dead drive, not a syncthing problem.",
    fix: [
      "Check the drive is mounted on that node (the disk bar in the column header will say DRIVE MISSING).",
      "Mount it, then rescan. Nothing was propagated — syncthing stopped precisely to avoid that.",
    ],
    // no delete actions here: the fix is to mount the drive, and deleting anything
    // while the folder's disk is missing is the last thing you want.
    actions: ["rescan"],
  },
  {
    match: /no space left|out of (disk )?space/i,
    title: "The disk is full",
    what: "Syncthing could not write because the filesystem holding this folder has no free space.",
    why: "",
    fix: ["Free space on that node, then rescan. The disk bar in the column header shows how full it is."],
    actions: ["rescan"],
  },
  {
    match: /permission denied|operation not permitted/i,
    title: "Permission denied",
    what: "Syncthing cannot read or write these files as the user it runs as.",
    why:
      "Usually a uid mismatch: the files on disk are owned by a different user than the one " +
      "syncthing runs as (common after moving a drive between machines, or restoring a backup).",
    fix: ["Check the file ownership on that node against the user in the syncthing@<user> unit."],
    actions: ["rescan"],
  },
];

function classifyError(msg) {
  return ERROR_KINDS.find((k) => k.match.test(msg)) || null;
}

const LOG_LEVEL = { 0: "text-slate-500", 1: "text-slate-400", 2: "text-slate-300", 3: "text-amber-300", 4: "text-red-300" };

// ErrorsTab — what is wrong with this folder on this node, and what to do.
//
// Three sources, because they are genuinely different things:
//   - the FOLDER-level error from /rest/db/status: "folder marker missing" etc.
//     This is the one that fires when a drive dies, and /rest/folder/errors stays
//     empty in that case.
//   - per-file PULL errors from /rest/folder/errors.
//   - for receive-only folders, LOCAL CHANGES (/rest/db/localchanged): files this
//     node has that the swarm does not. These are usually the CAUSE of the pull
//     errors above, so showing them turns a mystery into a diagnosis.
function ErrorsTab(props) {
  const key = () => ({ n: props.node, f: props.folder });

  const [status] = createResource(key, (k) =>
    relay(k.n, "rest/db/status", { folder: k.f }).catch(() => ({})));
  const [errs] = createResource(key, (k) =>
    relay(k.n, "rest/folder/errors", { folder: k.f }).then((r) => r.errors || []).catch(() => []));
  const [cfg] = createResource(key, (k) =>
    relay(k.n, "rest/config/folders").then((fs) => fs.find((x) => x.id === k.f) || {}).catch(() => ({})));
  const [local] = createResource(key, (k) =>
    relay(k.n, "rest/db/localchanged", { folder: k.f }).then((r) => r.files || []).catch(() => []));

  const folderErr = () => status()?.error || "";
  const anything = () => folderErr() || (errs() || []).length || (local() || []).length;

  return (
    <div class="space-y-4">
      <Show when={anything()} fallback={
        <div class="text-sm text-slate-500">No errors on {props.node}. </div>
      }>
        {/* folder-level: the folder has STOPPED */}
        <Show when={folderErr()}>
          <Explain title="This folder has stopped" raw={folderErr()} kind={classifyError(folderErr())}
            node={props.node} folder={props.folder} />
        </Show>

        {/* per-file pull errors, grouped by kind so 200 identical errors read as one problem */}
        <Show when={(errs() || []).length}>
          <For each={groupErrors(errs())}>{(g) => (
            <Explain title={g.kind?.title || "Sync error"} kind={g.kind} count={g.items.length}
              raw={g.items[0].error} items={g.items} node={props.node} folder={props.folder} />
          )}</For>
        </Show>

        {/* receive-only local additions: usually the CAUSE of the errors above */}
        <Show when={(local() || []).length}>
          <div class="rounded border border-amber-800 bg-amber-950/30 p-3">
            <div class="text-sm font-semibold text-amber-200">
              {local().length} file{local().length === 1 ? "" : "s"} exist only on {props.node}
            </div>
            <div class="mt-1 text-xs text-slate-300">
              This folder is <code>{cfg()?.type || "receive-only"}</code>, so these local files are
              never sent to the swarm. They are frequently what blocks a directory deletion above.
              They are not lost — they are only here.
            </div>
            <ul class="mt-2 max-h-40 space-y-0.5 overflow-y-auto">
              <For each={local().slice(0, 100)}>{(f) => (
                <li class="font-mono text-[11px] text-amber-100/80">{f.name}</li>
              )}</For>
            </ul>
            <Show when={local().length > 100}>
              <div class="mt-1 text-[11px] text-slate-500">…and {local().length - 100} more</div>
            </Show>
          </div>
        </Show>
      </Show>
    </div>
  );
}

// groupErrors collapses many identical-shaped errors into one explained block:
// 200 "directory not empty" lines are ONE problem, not 200.
function groupErrors(errs) {
  const groups = [];
  for (const e of errs || []) {
    const kind = classifyError(e.error);
    let g = groups.find((x) => (x.kind ? x.kind.title : x.raw) === (kind ? kind.title : e.error));
    if (!g) { g = { kind, raw: e.error, items: [] }; groups.push(g); }
    g.items.push(e);
  }
  return groups;
}

// The fixes the UI can apply, keyed by the id an ERROR_KIND lists in `actions`.
//
// destructive:true means a preview is MANDATORY — the button cannot delete
// anything until the user has been shown the exact list of files that disappear.
// A "fix it" button that silently deletes files you have never seen is not a fix,
// it is a trap.
const FIXES = {
  "clean-conflicts": {
    label: "Delete conflict copies",
    endpoint: "/api/fix/clean-conflicts",
    destructive: true,
    blurb:
      "Deletes ONLY syncthing's own .sync-conflict-* copies that exist just on this node. " +
      "These are the older versions it saved before overwriting a file — usually exactly what " +
      "is blocking the directory deletions. Nothing the swarm knows about is touched.",
  },
  revert: {
    label: "Revert ALL local changes",
    endpoint: "/api/fix/revert",
    destructive: true,
    blurb:
      "Deletes EVERY file that exists only on this node, so it matches the swarm exactly. " +
      "Blunter than the targeted fix: it removes local-only files that are not conflict copies too.",
  },
  rescan: {
    label: "Rescan folder",
    endpoint: "/api/fix/rescan",
    destructive: false,
    blurb: "Makes syncthing look at the disk again. Changes nothing on disk.",
  },
};

// FixButtons — preview, then apply. Destructive fixes always show the file list.
function FixButtons(props) {
  const [preview, setPreview] = createSignal(null); // {fix, count, files}
  const [busy, setBusy] = createSignal("");
  const [done, setDone] = createSignal("");

  async function call(fixId, dry) {
    const fix = FIXES[fixId];
    setBusy(dry ? "checking…" : "applying…");
    setDone("");
    try {
      const r = await fetch(fix.endpoint, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ node: props.node, folder: props.folder, preview: dry }),
      });
      const j = await r.json();
      if (!r.ok) throw new Error(j.error || r.status);
      if (dry) {
        setPreview({ fixId, ...j });
      } else {
        setPreview(null);
        setDone(j.deleted != null ? `done — ${j.deleted} file(s) deleted, rescanning` : "done — rescanning");
      }
    } catch (e) {
      setDone("error: " + String(e.message || e));
    } finally {
      setBusy("");
    }
  }

  return (
    <div class="mt-3 border-t border-red-900/50 pt-2">
      <div class="mb-1.5 text-xs font-semibold text-slate-300">Fix it</div>
      <div class="flex flex-wrap gap-2">
        <For each={props.actions}>{(id) => {
          const fix = FIXES[id];
          if (!fix) return null;
          return (
            <button
              disabled={!!busy()}
              onClick={() => (fix.destructive ? call(id, true) : call(id, false))}
              title={fix.blurb}
              class="rounded px-2.5 py-1 text-xs disabled:opacity-50"
              classList={{
                "bg-red-800 text-red-100 hover:bg-red-700": fix.destructive,
                "bg-slate-700 text-slate-200 hover:bg-slate-600": !fix.destructive,
              }}>
              {fix.destructive ? "⚠ " : ""}{fix.label}{fix.destructive ? "…" : ""}
            </button>
          );
        }}</For>
        <Show when={busy()}><span class="self-center text-xs text-slate-400">{busy()}</span></Show>
        <Show when={done()}>
          <span class="self-center text-xs"
            classList={{ "text-red-300": done().startsWith("error"), "text-emerald-300": !done().startsWith("error") }}>
            {done()}
          </span>
        </Show>
      </div>

      {/* the preview IS the safety mechanism: you cannot delete what you have not been shown */}
      <Show when={preview()}>
        <div class="mt-3 rounded border border-amber-700 bg-amber-950/40 p-3">
          <div class="text-sm font-semibold text-amber-100">
            {FIXES[preview().fixId].label} on {props.node}
          </div>
          <p class="mt-1 text-xs text-slate-300">{FIXES[preview().fixId].blurb}</p>

          <Show when={preview().count > 0} fallback={
            <div class="mt-2 text-xs text-slate-400">Nothing matches — there is nothing to delete.</div>
          }>
            <div class="mt-2 text-xs font-semibold text-amber-200">
              This will permanently DELETE {preview().count} file{preview().count === 1 ? "" : "s"} on {props.node}:
            </div>
            <ul class="mt-1 max-h-56 space-y-0.5 overflow-y-auto rounded bg-black/40 p-2">
              <For each={preview().files}>{(f) => (
                <li class="font-mono text-[11px] text-amber-100/90">{f}</li>
              )}</For>
            </ul>
            <div class="mt-1 text-[11px] text-slate-400">
              These exist only on {props.node} — deleting them cannot affect any other node.
              Read the list: anything here that you want, move it out of the folder first.
            </div>
          </Show>

          <div class="mt-3 flex gap-2">
            <button onClick={() => setPreview(null)}
              class="rounded bg-slate-700 px-2.5 py-1 text-xs hover:bg-slate-600">Cancel</button>
            <Show when={preview().count > 0}>
              <button disabled={!!busy()} onClick={() => call(preview().fixId, false)}
                class="rounded bg-red-700 px-2.5 py-1 text-xs text-red-50 hover:bg-red-600 disabled:opacity-50">
                Delete {preview().count} file{preview().count === 1 ? "" : "s"}
              </button>
            </Show>
          </div>
        </div>
      </Show>
    </div>
  );
}

function Explain(props) {
  return (
    <div class="rounded border border-red-800 bg-red-950/30 p-3">
      <div class="flex items-baseline gap-2">
        <span class="text-sm font-semibold text-red-200">{props.title}</span>
        <Show when={props.count > 1}>
          <span class="rounded bg-red-900/70 px-1.5 py-0.5 text-[10px] text-red-200">
            {props.count} occurrences
          </span>
        </Show>
      </div>

      <Show when={props.kind}>
        <p class="mt-2 text-xs leading-relaxed text-slate-200">{props.kind.what}</p>
        <Show when={props.kind.why}>
          <p class="mt-1.5 text-xs leading-relaxed text-slate-400">{props.kind.why}</p>
        </Show>
        <Show when={props.kind.fix?.length}>
          <div class="mt-2 text-xs font-semibold text-slate-300">What you can do</div>
          <ul class="mt-1 list-disc space-y-1 pl-5">
            <For each={props.kind.fix}>{(f) => <li class="text-xs text-slate-300">{f}</li>}</For>
          </ul>
        </Show>
        <Show when={props.kind.danger}>
          <div class="mt-2 rounded bg-amber-950/50 px-2 py-1 text-[11px] text-amber-200 ring-1 ring-amber-800">
            ⚠ {props.kind.danger}
          </div>
        </Show>
        <Show when={props.kind.actions?.length && props.node && props.folder}>
          <FixButtons node={props.node} folder={props.folder} actions={props.kind.actions} />
        </Show>
      </Show>

      {/* the raw message, always — this explains, it does not hide */}
      <details class="mt-2">
        <summary class="cursor-pointer text-[11px] text-slate-500 hover:text-slate-300">
          raw error{props.items?.length > 1 ? ` (${props.items.length} paths)` : ""}
        </summary>
        <Show when={props.items} fallback={
          <div class="mt-1 rounded bg-black/40 p-2 font-mono text-[11px] text-red-200">{props.raw}</div>
        }>
          <ul class="mt-1 max-h-48 space-y-1 overflow-y-auto">
            <For each={props.items}>{(e) => (
              <li class="rounded bg-black/40 p-2 font-mono text-[11px] text-red-200">
                <div class="text-red-300">{e.path}</div>
                <div class="text-slate-400">{e.error}</div>
              </li>
            )}</For>
          </ul>
        </Show>
      </details>
    </div>
  );
}

function LogsTab(props) {
  const [errs] = createResource(() => ({ n: props.node, f: props.folder }),
    (k) => relay(k.n, "rest/folder/errors", { folder: k.f }).then((r) => r.errors || []).catch(() => []));
  // The folder-level error, separate from the per-file pull errors above. This is
  // the one that says "folder marker missing" when the drive is gone — and
  // /rest/folder/errors stays EMPTY in that case, so without this the dock showed
  // nothing at all for a folder whose whole disk had vanished.
  const [fatal] = createResource(() => ({ n: props.node, f: props.folder }),
    (k) => relay(k.n, "rest/db/status", { folder: k.f }).then((r) => r.error || "").catch(() => ""));
  const [log, setLog] = createSignal([]);
  createEffect(() => {
    const n = props.node;
    const pull = () => relay(n, "rest/system/log").then((r) => setLog(r.messages || [])).catch(() => {});
    pull();
    const id = setInterval(pull, 4000);
    onCleanup(() => clearInterval(id));
  });
  return (
    <div class="space-y-3">
      <Show when={fatal()}>
        <div>
          <div class="mb-1 text-xs font-semibold text-red-300">folder stopped</div>
          <div class="rounded bg-red-950/80 p-2 text-xs text-red-100 ring-1 ring-red-500">{fatal()}</div>
          <Show when={fatal().includes("marker")}>
            <div class="mt-1 text-[11px] text-slate-400">
              The <code>.stfolder</code> marker lives on the folder's own disk, so this almost always
              means the drive is not mounted. Syncthing has stopped this folder rather than treat the
              missing files as deletions — nothing has been propagated to other nodes.
            </div>
          </Show>
        </div>
      </Show>
      <Show when={errs() && errs().length}>
        <div>
          <div class="mb-1 text-xs font-semibold text-red-300">file errors</div>
          <For each={errs()}>{(e) => <div class="rounded bg-red-950/60 p-2 text-xs text-red-200">{e.path}: {e.error}</div>}</For>
        </div>
      </Show>
      <div class="rounded border border-slate-800 bg-black/30 p-2 font-mono text-[12px]">
        <For each={log().slice(-200).reverse()} fallback={<div class="text-slate-600">no log lines</div>}>
          {(m) => (
            <div class={LOG_LEVEL[m.level] || "text-slate-300"}>
              <span class="text-slate-600">{new Date(m.when).toLocaleTimeString()} </span>{m.message}
            </div>
          )}
        </For>
      </div>
    </div>
  );
}
