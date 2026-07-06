import { createSignal, createResource, createEffect, For, Index, Show, onCleanup, createMemo } from "solid-js";

const MATRIX_POLL = 5000;

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

// compact disk usage bar; u = {total,used,avail,pct,mount,err}
function DiskBar(props) {
  return (
    <Show when={props.u} fallback={<span class="text-[10px] text-slate-600">disk —</span>}>
      <Show when={!props.u.err} fallback={<span class="text-[10px] text-slate-600" title={props.u.err}>disk n/a</span>}>
        <div title={`${bytes(props.u.avail)} free of ${bytes(props.u.total)} (${props.u.pct}% used) on ${props.u.mount}`}>
          <div class="h-1.5 w-full overflow-hidden rounded bg-slate-800">
            <div class={"h-full " + diskColor(props.u.pct)} style={{ width: props.u.pct + "%" }} />
          </div>
          <div class="mt-0.5 text-[10px] text-slate-500">{props.u.pct}% · {bytes(props.u.avail)} free</div>
        </div>
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
  const [view, setView] = createSignal("matrix"); // matrix | settings
  const [shareMode, setShareMode] = createSignal(false);
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

  return (
    <div class="min-h-screen">
      <div class="p-6" classList={{ "pb-[58vh]": !!sel() && view() === "matrix" }}>
        <header class="mb-5 flex items-baseline gap-3">
          <h1 class="text-xl font-semibold text-slate-100">syncthing swarm</h1>
          <Show when={data()}>
            <span class="text-xs text-slate-500">
              {data().devices.length} devices · {data().folders.length} folders ·
              polled {new Date(data().generatedAt).toLocaleTimeString()}
            </span>
          </Show>
          <div class="ml-auto flex items-center gap-2">
            <Show when={view() === "matrix"}>
              <button onClick={() => setShareMode(!shareMode())}
                class="rounded px-3 py-1 text-xs"
                classList={{ "bg-sky-700 text-white": shareMode(), "bg-slate-700 hover:bg-slate-600": !shareMode() }}
                title={"toggle share mode — check a box to share " + (data()?.source || "local") + "'s folder to a device"}>
                {shareMode() ? "☑ share mode" : "share mode"}
              </button>
            </Show>
            <button onClick={() => refetch()} class="rounded bg-slate-700 px-3 py-1 text-xs hover:bg-slate-600">refresh</button>
            <button onClick={() => setView(view() === "settings" ? "matrix" : "settings")} title="settings"
              class="rounded px-2 py-1 text-base leading-none hover:bg-slate-700"
              classList={{ "bg-slate-700": view() === "settings" }}>⚙</button>
          </div>
        </header>

        <Show when={shareMode() && view() === "matrix"}>
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
        <Show when={data()} fallback={<div class="text-slate-500">loading…</div>}>
          <Show when={view() === "settings"} fallback={
            <Matrix data={data()} sel={sel()} shareMode={shareMode()} disk={disk()}
              onFolder={(f) => setSel({ folder: f, tab: "files" })}
              onCell={(f, d) => setSel({ folder: f, device: d.name, tab: "transfers" })}
              onShare={doShare} onUnshare={askUnshare} />
          }>
            <Settings data={data()} disk={disk()} />
          </Show>
        </Show>
      </div>

      <Show when={sel() && view() === "matrix" && !shareMode()}>
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

  const Tab = (p) => (
    <button onClick={() => setTab(p.id)}
      class="border-b-2 px-3 py-1.5 text-sm"
      classList={{ "border-sky-400 text-sky-300": tab() === p.id, "border-transparent text-slate-400 hover:text-slate-200": tab() !== p.id }}>
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

const LOG_LEVEL = { 0: "text-slate-500", 1: "text-slate-400", 2: "text-slate-300", 3: "text-amber-300", 4: "text-red-300" };

function LogsTab(props) {
  const [errs] = createResource(() => ({ n: props.node, f: props.folder }),
    (k) => relay(k.n, "rest/folder/errors", { folder: k.f }).then((r) => r.errors || []).catch(() => []));
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
      <Show when={errs() && errs().length}>
        <div>
          <div class="mb-1 text-xs font-semibold text-red-300">folder errors</div>
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
