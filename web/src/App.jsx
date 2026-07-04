import { createSignal, createResource, For, Show, onCleanup } from "solid-js";

const POLL_MS = 5000;

async function fetchMatrix() {
  const r = await fetch("/api/matrix");
  if (!r.ok) throw new Error("HTTP " + r.status);
  return r.json();
}

// color + label for a single (folder, device) cell
function cellStyle(cell, online) {
  if (!online) return { bg: "bg-slate-800/40 text-slate-600", label: "—", ring: "" };
  if (!cell || !cell.present) return { bg: "bg-slate-900 text-slate-700", label: "·", ring: "" };
  const errs = cell.errors && cell.errors.length;
  if (errs || cell.state === "error")
    return { bg: "bg-red-900/70 text-red-200", label: "err", ring: "ring-1 ring-red-500" };
  if (cell.state === "paused")
    return { bg: "bg-slate-700/50 text-slate-400", label: "pause", ring: "" };
  if (cell.state === "syncing")
    return { bg: "bg-sky-800/70 text-sky-100", label: pct(cell.completion), ring: "" };
  if (cell.state === "scanning")
    return { bg: "bg-amber-800/60 text-amber-100", label: "scan", ring: "" };
  if (cell.completion >= 99.95)
    return { bg: "bg-emerald-800/60 text-emerald-100", label: "100", ring: "" };
  return { bg: "bg-amber-800/50 text-amber-100", label: pct(cell.completion), ring: "" };
}

const pct = (n) => (n == null ? "?" : Math.floor(n) + "%");
const bytes = (n) => {
  if (!n) return "0 B";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(1) + " " + u[i];
};

export default function App() {
  const [data, { refetch }] = createResource(fetchMatrix);
  const [sel, setSel] = createSignal(null); // {folder, device, cell}

  const timer = setInterval(refetch, POLL_MS);
  onCleanup(() => clearInterval(timer));

  return (
    <div class="min-h-screen p-6">
      <header class="mb-5 flex items-baseline gap-3">
        <h1 class="text-xl font-semibold text-slate-100">syncthing swarm</h1>
        <Show when={data()}>
          <span class="text-xs text-slate-500">
            {data().devices.length} devices · {data().folders.length} folders ·
            polled {new Date(data().generatedAt).toLocaleTimeString()}
          </span>
        </Show>
        <button
          onClick={() => refetch()}
          class="ml-auto rounded bg-slate-700 px-3 py-1 text-xs hover:bg-slate-600"
        >
          refresh
        </button>
      </header>

      <Show when={data.error}>
        <div class="rounded bg-red-900/60 p-3 text-red-200">
          cannot reach swarmd: {String(data.error)}
        </div>
      </Show>

      <Show when={data()} fallback={<div class="text-slate-500">loading…</div>}>
        <Matrix data={data()} onPick={setSel} />
      </Show>

      <Show when={sel()}>
        <Drawer sel={sel()} onClose={() => setSel(null)} />
      </Show>
    </div>
  );
}

function Matrix(props) {
  const d = () => props.data;
  return (
    <div class="overflow-x-auto rounded-lg border border-slate-800">
      <table class="border-collapse text-sm">
        <thead>
          <tr>
            <th class="sticky left-0 z-10 bg-[#0b0e14] p-2 text-left font-medium text-slate-400">
              folder \ device
            </th>
            <For each={d().devices}>
              {(dev) => (
                <th class="min-w-[120px] border-l border-slate-800 p-2 align-bottom">
                  <div class="flex items-center gap-1.5">
                    <span
                      class={
                        "inline-block h-2 w-2 rounded-full " +
                        (dev.online ? "bg-emerald-400" : "bg-slate-600")
                      }
                      title={dev.online ? "online" : "offline / unreachable"}
                    />
                    <span class="font-semibold text-slate-200">{dev.name}</span>
                    <Show when={dev.systemErrors && dev.systemErrors.length}>
                      <span
                        class="rounded bg-red-600 px-1 text-[10px] text-white"
                        title={dev.systemErrors.join("\n")}
                      >
                        {dev.systemErrors.length}!
                      </span>
                    </Show>
                  </div>
                  <div class="mt-0.5 text-[10px] font-normal text-slate-500">
                    {dev.version || "—"}
                  </div>
                </th>
              )}
            </For>
          </tr>
        </thead>
        <tbody>
          <For each={d().folders}>
            {(f) => (
              <tr class="border-t border-slate-800">
                <td class="sticky left-0 z-10 bg-[#0b0e14] p-2 font-medium text-slate-300">
                  {f.label}
                  <div class="text-[10px] font-normal text-slate-600">{f.id}</div>
                </td>
                <For each={d().devices}>
                  {(dev) => {
                    const cell = () => (d().cells[f.id] || {})[dev.name];
                    const s = () => cellStyle(cell(), dev.online);
                    return (
                      <td class="border-l border-slate-800 p-1 text-center">
                        <button
                          class={
                            "w-full rounded px-2 py-1.5 text-xs font-medium " +
                            s().bg + " " + s().ring +
                            (cell() && cell().present ? " cursor-pointer hover:brightness-125" : " cursor-default")
                          }
                          disabled={!cell() || !cell().present || !dev.online}
                          onClick={() =>
                            props.onPick({ folder: f, device: dev, cell: cell() })
                          }
                        >
                          {s().label}
                        </button>
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

function Drawer(props) {
  const s = props.sel;
  const c = s.cell || {};
  return (
    <div class="fixed inset-0 z-20 flex justify-end bg-black/40" onClick={props.onClose}>
      <div
        class="h-full w-96 overflow-y-auto border-l border-slate-700 bg-[#11151f] p-5"
        onClick={(e) => e.stopPropagation()}
      >
        <div class="mb-4 flex items-start justify-between">
          <div>
            <div class="text-lg font-semibold text-slate-100">{s.folder.label}</div>
            <div class="text-xs text-slate-500">
              {s.folder.id} @ {s.device.name}
            </div>
          </div>
          <button onClick={props.onClose} class="text-slate-500 hover:text-slate-200">✕</button>
        </div>

        <dl class="space-y-2 text-sm">
          <Row k="state" v={c.state} />
          <Row k="completion" v={pct(c.completion)} />
          <Row k="need bytes" v={bytes(c.needBytes)} />
          <Row k="need items" v={c.needItems ?? 0} />
          <Row k="device id" v={s.device.deviceID ? s.device.deviceID.slice(0, 12) + "…" : "—"} />
        </dl>

        <Show when={c.errors && c.errors.length}>
          <div class="mt-4">
            <div class="mb-1 text-xs font-semibold text-red-300">folder errors</div>
            <ul class="space-y-1">
              <For each={c.errors}>
                {(e) => (
                  <li class="rounded bg-red-950/60 p-2 text-xs text-red-200">{e}</li>
                )}
              </For>
            </ul>
          </div>
        </Show>

        <Show when={s.device.systemErrors && s.device.systemErrors.length}>
          <div class="mt-4">
            <div class="mb-1 text-xs font-semibold text-red-300">device system errors</div>
            <ul class="space-y-1">
              <For each={s.device.systemErrors}>
                {(e) => (
                  <li class="rounded bg-red-950/60 p-2 text-xs text-red-200">{e}</li>
                )}
              </For>
            </ul>
          </div>
        </Show>
      </div>
    </div>
  );
}

function Row(props) {
  return (
    <div class="flex justify-between border-b border-slate-800 pb-1">
      <dt class="text-slate-500">{props.k}</dt>
      <dd class="text-slate-200">{props.v}</dd>
    </div>
  );
}
