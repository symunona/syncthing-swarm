# syncthing-swarm

Central dashboard for many syncthing nodes. Folders = rows, machines = columns.
One table. See sync %, state, errors, version across whole fleet. No more ssh
+ port-forward + tab soup.

Nothing existing do this (see vault research). Build on syncthing official REST
API. Nodes reachable over Tailscale.

## Tech stack (caveman)

- **Backend** Go. One binary `swarmd`. Read `swarm.yaml` cred store. Poll every
  node REST API concurrent. Merge into one matrix. Serve JSON + web.
- **Frontend** Vite + SolidJS + Tailwind v4. Build to static. Go embed into
  binary (`-tags embedweb`). Single file deploy.
- **Transport** Tailscale. Hub hit each node `http://<tailnet-ip>:8384` with
  its `X-API-Key`. No port-forward.
- **Cred store** `swarm.yaml`. Plaintext. Gitignored. You edit, add node.
- **State** in-memory snapshot. Poll loop every N sec. No DB (MVP).

## Layout

```
cmd/swarmd/          main. flag -config, signal shutdown
internal/config/     load+validate swarm.yaml
internal/stclient/   one node REST client (version, config, db/status, errors)
internal/aggregate/  fan-out all nodes -> Snapshot{devices,folders,cells}
internal/server/     poll loop + /api/matrix + serve embedded web
internal/webui/      go embed of built frontend (dist)
web/                 vite+solid+tailwind source
```

## REST endpoints hit per node

- `/rest/system/version` — version column
- `/rest/system/status` — device id
- `/rest/system/error` — device-level errors
- `/rest/config` — folders + share topology
- `/rest/db/status?folder=` — per-folder state, need/global bytes -> completion
- `/rest/folder/errors?folder=` — per-folder pull errors

## Run

```bash
cp swarm.example.yaml swarm.yaml     # fill in nodes + apikeys
go run ./cmd/swarmd                   # API on :8888, no UI
# dev UI (hot reload, proxies /api):
pnpm --dir web install
pnpm --dir web run dev                # vite :5173

# single binary (embed UI):
pnpm --dir web run build
go build -tags embedweb -o swarmd ./cmd/swarmd
./swarmd                              # full dashboard on listen addr
```

`make build` do the two-step. `make test` run go tests.

## Get a node apikey

Syncthing GUI -> Actions -> Settings -> API Key. Or `grep <apikey>` in that
node config.xml.

## Sharing (share / unshare folders)

Set a `root:` per node in `swarm.yaml` (base dir for new shared folders) and mark
the local node `local: true`. Then:

- **UI:** toggle **share mode** (top right) -> matrix cells become checkboxes.
  Check a box to share the local node's folder to that device (one click);
  uncheck to stop (confirms first, keeps files).
- **CLI** (`stc`, same swarm.yaml + logic):
  ```bash
  stc share   <folder> <target> [-path DIR] [-from NODE]
  stc unshare <folder> <target>            [-from NODE]   # no confirm
  # or: just share dropx taskbot   /   just unshare dropx taskbot
  ```
  `<folder>` is an id or label; new folder lands at `<target root>/<label>`
  (or `-path`). **Unshare never deletes files on disk.**

Backend: `POST /api/share` / `POST /api/unshare` (guarded writes, separate from
the read-only relay). Shared logic in `internal/sharing`.

## Disk usage

Syncthing exposes no host disk stats, so swarmd runs `df` per node on a 60s
ticker — locally for the hub node, over `ssh` for the rest. Set `ssh:` per node
in `swarm.yaml` (ssh destination + opts, e.g. `ssh: -p 2222 taskbot`); it reports
the filesystem holding that node's `root`. Shown as a bar in each matrix column
header and the settings cards (`GET /api/disk`). No ssh set on a remote node →
"disk n/a".

**It never falls back to `/`.** It used to, and that made the bar lie in exactly
the case it exists for: when an external drive dies, `df <root>` fails, the
fallback reports the boot media, and you get a healthy green bar for a drive that
is gone. A missing root is now reported, not papered over.

Set **`mount:`** on any node whose root lives on a separate drive (e.g.
`mount: /media/hdd`). A dying drive usually leaves its mountpoint behind as an
empty dir on the SD card, so `df` still resolves — just to the wrong filesystem.
`mount:` is what lets swarmd tell those apart and show **⚠ DRIVE MISSING**.

The other half of that alarm: syncthing's own folder-level error (`folder marker
missing`) now reaches the matrix — hover a red cell, or open the folder dock.
The `.stfolder` marker lives on the folder's own disk, so its absence means the
drive is not mounted, and syncthing stops the folder instead of propagating the
missing files as deletions.

## Roadmap (phases)

1. **MVP** read-only matrix. ✅
2. **Actions** one-click share/unshare ✅ (+ relay, detail dock, settings).
   TODO: pause/rescan, approve pending devices.
3. **st-cli** `stc share/unshare` ✅. TODO: connect/test node, pull logs, add node.
4. **Wizard** Tailscale+SSH -> apt install syncthing -> generate config ->
   auto-register into swarm + sharing graph.

Full spec in vault: `50-59 pet projects and hobbies/55 syncthing-swarm/`.
