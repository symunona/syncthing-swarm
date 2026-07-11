# Node bootstrap wizard — design

`stc bootstrap <ssh-dest>` — provision a fresh box into the syncthing swarm:
probe it, harden it, set up its disk, install syncthing, mesh it with every
existing node, and re-adopt folders already sitting on an intact drive.

Roadmap phase 4 (see README). First target box: **rue** — Raspberry Pi 2 Model B,
armv7l, 1 GiB RAM, external USB HDD, reachable over Tailscale via an ssh alias.

## Why

Setting up a new node today is: ssh in, apt install, find the API key, edit
`swarm.yaml`, then click every device into every other device by hand. And the
common real case is worse — **the SD card died, the HDD is fine**. All the data
is still on disk; only syncthing's index (which lived on the dead card) is gone.
Re-adding those folders by hand means getting every folder ID right or syncthing
re-downloads terabytes it already has.

## Principles

- **Probe is read-only and needs no sudo.** It cannot change the box. Every
  command in it is a read.
- **Nothing is written anywhere before a gate.** Not the remote box, not
  `swarm.yaml`, not any node's syncthing config.
- **The wizard owns anything additive and reversible. It never touches the ssh
  access path.** A bug that locks you out of a headless box in another room is
  unrecoverable without a keyboard and a monitor.
- **Idempotent.** Re-running on a provisioned box reports `ok (skip)` per step.
  This makes it a "check my node" tool, not a one-shot.
- **Cheap.** The probe does no deep filesystem walks. Target: < 2s total.
- **The screen always says what's happening.** Partial results stream in; slow
  things declare their cost before they run; anything optional is skippable.

## Interfaces

CLI only. `internal/provision/` holds every stage as a plain function, so a web
wizard later is just handlers over the same package — the trick `internal/sharing`
already uses to serve both `swarmd` and `stc` without drift.

The ssh destination is a string handed straight to `ssh`, exactly like the
existing `ssh:` field in `swarm.yaml` (`internal/diskusage` does this today).
`~/.ssh/config` owns aliases, ports, keys, tailscale. The wizard never parses
hostnames and never manages keys.

One **ssh ControlMaster** socket is opened at startup and every stage rides it:
single auth, no per-command handshake (~300 ms × ~40 commands is real money on a
Pi 2). It dies with the process.

## Files

```
internal/provision/
  00-probe.sh      embedded, read-only, emits NDJSON
  00-probe.go      run over ssh, parse NDJSON -> Probe
  01-report.go     Probe -> human report + capacity estimates
  02-plan.go       Probe -> []Step  (dynamic; only what is missing)
  02-apply.go      run a Step via `ssh -t sudo`, re-probe to verify
  03-syncthing.go  install, bind GUI to tailnet IP, harvest apikey
  04-join.go       swarm.yaml append + device mesh
  05-adopt.go      .stfolder discovery -> folder adoption
  provision.go     shared types: Probe, Step, Node  (no stage number)
cmd/stc/main.go    + `bootstrap` subcommand (prompts, gates)
docs/nodes/<name>.md  measured specs + per-check timings, written by the wizard
```

Numbered filenames so the directory listing *is* the flow. Go ignores only `_`-
and `.`-prefixed files; digits and hyphens are fine.

## Stage 0 — probe

`ssh dest bash -s < 00-probe.sh`. Nothing is copied to the box; nothing is left
behind. The script emits **NDJSON, one line per check, as each completes**, so
partial results render immediately instead of blocking on a final blob.

```json
{"check":"box","state":"ok","data":{"model":"Raspberry Pi 2 Model B Rev 1.1","arch":"armv7l","mem":1073741824}}
{"check":"stfolders","state":"start","hint":"fast"}
{"check":"stfolders","state":"ok","data":[...]}
```

### What it collects

**Box identity.** `/proc/device-tree/model` (literally "Raspberry Pi 2 Model B
Rev 1.1"; absent on non-Pi → fall back to `/sys/class/dmi/id/product_name`),
`/etc/os-release`, `uname -m` (`armv7l` → 32-bit, decides the apt repo arch),
`nproc`, `MemTotal`. This is what lets the wizard work on a VPS or an x86 box.

**inotify.** `fs.inotify.max_user_watches`, `max_user_instances`. The default
8192 is far below what syncthing needs (roughly one watch per *directory* in
every folder tree). Blow the limit and syncthing silently abandons watching and
falls back to periodic scans — the exact thing the disk-aging policy is trying
to avoid.

**Disks.** `lsblk -J -O` answers "is there an external drive", "HDD or SSD"
(`rota`), "USB?" (`tran`), fstype, UUID, mountpoint — in one call. Plus
`findmnt -J` for mount options and `/etc/fstab` for whether a UUID entry exists
(does the drive come back after reboot?) and whether it has `nofail` (without
which a missing drive hangs boot).

**Spindown.** Is `hd-idle` installed/enabled? `hdparm -C` for current power
state (read-only; does not spin the disk up).

**Security** (deliberately small): `ss -ltnp` (a service bound to `0.0.0.0` is a
finding; one bound to `127.0.0.1` is not), `systemctl list-unit-files
--state=enabled`, presence + enabled state of `ufw` / `fail2ban` /
`unattended-upgrades`, and `PasswordAuthentication` / `PermitRootLogin` read out
of `/etc/ssh/sshd_config` + `sshd_config.d/*` (world-readable — no sudo needed,
and we only *report* these).

**Syncthing + tailscale.** Existing install, version, config dir. `tailscale ip
-4` (needed to bind the GUI) and whether tailscaled is up.

**Folders on the drive.** Scoped to the external drive's mountpoint only:

```sh
find "$MP" -mindepth 1 -maxdepth 3 -type d \
     \( -name lost+found -o -name .stversions -o -name '.Trash-*' \) -prune -o \
     -type d -name .stfolder -print -prune
```

A `.stfolder` at depth ≤ 3 means the folder root is at depth ≤ 2, which covers
every realistic layout (`/media/hdd/syncthing/dropx`). `-prune` on the marker
stops descent into folder contents; the depth cap bounds the walk to dozens of
directories. Sub-second, even on a spun-down disk (one spin-up, then nothing).

For each hit, read `.stfolder/syncthing-folder.txt` → **folder ID**. Modern
syncthing writes this file, which makes an intact HDD self-describing. Old
markers (pre ~v1.6) are empty dirs → fall back to matching the directory name
against known folder labels.

### No deep walks

Two things that look necessary but are full-tree walks (hours on a USB2 HDD),
and their cheap substitutes:

- inotify sizing → **`df -i`** inodes-used. Instant, and an upper bound on
  watches needed — which errs in the safe direction.
- initial-scan ETA → **`df`** used-bytes. Slightly over-counts (includes
  non-syncthing data), which errs toward over-estimating the wait.

Per-folder byte sizes are an opt-in `-du` flag.

### Benchmarks

| bench | how | cost | default |
|---|---|---|---|
| hash | `dd if=/dev/zero bs=1M count=64 \| sha256sum`, timed | ~2 s | **on** |
| disk r/w | `dd` 128 MiB `oflag=direct`, then delete | ~10 s | off |
| network | `dd bs=1M count=32 \| ssh rue 'cat >/dev/null'` | ~10 s | off |

Hash is on by default because the initial scan is **hash-bound**, not I/O-bound,
on a Cortex-A7 — it is the number that predicts the whole adoption. Disk +
network are one prompt (`run disk + network benchmark? (~20s) [y/N]`, default
**no**). **Ctrl-C during a benchmark skips only that benchmark** and continues
the wizard (SIGINT trapped on the bench context alone).

Nothing is ever installed on the box to benchmark it — no `speedtest-cli`, no
`iperf3`. The network test measures throughput through tailscale + ssh, which is
the path syncthing actually uses; internet speed is irrelevant to a node.

## Stage 1 — report

Line-oriented ANSI, not a full-screen TUI: scrollback survives, no bubbletea
dependency, degrades to plain text when piped.

```
  probing rue …
  ✓ box        Raspberry Pi 2 Model B — armv7l, 4 core, 1.0 GiB RAM         0.2s
  ✓ inotify    max_user_watches=8192  ← default, too low                    0.0s
  ✓ disks      sda  1.8T  USB  rotational(HDD)  ext4  → /media/hdd          0.3s
  ✓ fstab      UUID entry present, missing `nofail`, missing `noatime`      0.0s
  ✓ security   ufw: absent   fail2ban: absent   listening: sshd:22, …       0.4s
  ✓ hash       sha256: 31 MB/s  (Cortex-A7, no crypto ext)                  1.1s
  ✓ stfolders  3 found on /media/hdd                                        0.6s
```

Then the capacity estimate — what you can expect of this box:

```
  initial scan: 412 GiB at ~31 MB/s → est. 3h45m
                (one-time; rebuilds the index the dead SD card took.
                 Transfers nothing over the network.)
```

Rules for anything slow: **declare the cost before paying it** (each check
carries a `hint`); **show a live counter when an ETA is impossible** (a moving
number is proof of life, a spinner is not); **show a real ETA when one can be
computed**.

## Stage 2 — plan → apply

The report *derives* the step list. Only what is actually missing appears. Each
step prints its exact commands before running; you approve per group; applies go
through `ssh -t dest sudo sh -c '…'` so sudo prompts land in your terminal
(password never enters Go memory or any config file).

| step | when | what |
|---|---|---|
| `inotify` | watches < inodes-used on the drive | `/etc/sysctl.d/60-syncthing.conf`, sized from `df -i` |
| `fstab` | drive not in fstab, or missing opts | UUID entry + `nofail` + `noatime` |
| `hd-idle` | `rota=1` | install, set idle timeout (default 10 min) |
| `ufw` | absent | install; **allow ssh (actual port) + `tailscale0` FIRST**, then enable |
| `fail2ban` | absent | install, enable, default sshd jail |
| `unattended-upgrades` | absent | install, enable |
| *(report only)* | — | `PasswordAuthentication yes`, `PermitRootLogin`, anything listening on `0.0.0.0` |

**`ufw enable` requires typing the word `ufw`**, not a bare `y` — it is the one
step that can sever your access. The allow rules for the live sshd port (read
from the probe, not assumed to be 22) and for the `tailscale0` interface go in
*before* `enable`, never after.

The whole HDD-aging policy (`hd-idle` + `nofail`/`noatime` + weekly rescan) is
**skipped automatically when `rota=0`** — spindown is meaningless on flash and
`hd-idle` would be dead weight.

## Stage 3 — syncthing

- Install from **`apt.syncthing.net`**, not the distro repo (Raspberry Pi OS
  ships a years-old syncthing; it would out-drift the rest of the swarm).
- **`systemctl enable --now syncthing@symunona`** — the templated *system* unit
  from the official `.deb`. Runs as the user, owns files as the user (uid must
  match what is already on the HDD or adoption causes permission churn), and
  starts at boot on a headless box with no `loginctl enable-linger` footgun.
- **Bind the GUI to the tailnet IP only** (`tailscale ip -4`), never `0.0.0.0`.
  The dashboard reaches it over the tailnet; nothing on the LAN or a public
  interface can see the API. Biggest security win in the flow, costs nothing.
- Harvest the generated API key from `config.xml` (or `syncthing cli config gui
  apikey get`) and hand it to stage 4.
- Firewall: allow `22000/tcp+udp` and `21027/udp` **on `tailscale0` only**.

## Stage 4 — join the swarm

- Append the node to `swarm.yaml` as **text** (`name`, `url`, `apikey`, `root`,
  `ssh`). A yaml.v3 round-trip would eat the file's comments; an append does not.
- Mesh the devices: add the new device ID to **every** node in the swarm, and
  every existing node's device ID to the new node. Set explicit addresses
  (`tcp://<tailnet-ip>:22000`, taken from each node's `url` host) rather than
  relying on global discovery, which does not reliably find tailnet peers.

## Stage 5 — adopt folders

The hub already knows every folder ID + label in the fleet (it polls each node's
`/rest/config`). Cross-referencing that against what the probe found on the drive
gives three buckets:

- **known** — ID on disk matches a folder the swarm has → offer to adopt, path
  pre-filled to where it actually sits on the HDD.
- **guessed** — empty `.stfolder`, directory name looks like a known label →
  offer to adopt, but confirm the ID mapping explicitly.
- **orphan** — folder on disk that nobody in the swarm has → list it, do
  nothing. Adopting into a fleet with no other copy is a different, riskier
  operation.

Adopting a known folder means: create it on the new node with its **existing ID**
at its **existing path**, then add the new device to that folder on every swarm
node that already has it. Syncthing rescans, hashes what is on disk, finds it
already matches, and transfers ~nothing. That is the entire point.

**Per-folder prompt, default `sendreceive`**, with the tradeoff explained inline
at the prompt: receive-only means the box can never push a local mistake back out
into the swarm (a bad rescan after an SD swap, a half-mounted drive, a
permissions glitch) — the right choice for a box whose job is to hold a copy and
that you never edit on.

When receive-only is chosen, also offer `rescanIntervalS: 0` (periodic scan fully
disabled). Otherwise HDD-backed folders get **`604800` (weekly)**.

### Why weekly, and what a rescan is actually worth

- It does **not** catch bit rot. A rescan compares size + mtime, not content
  hashes. An unchanged file with a flipped bit is skipped.
- What it catches is **changes the watcher missed** — files changed while
  syncthing was down, or events dropped on inotify queue overflow.
- One walk a week is one spin-up out of the ~1000 the drive will do anyway:
  noise against the aging budget, and it keeps the drift check alive.

The `.stfolder` marker lives **on the HDD**, so if the drive is not mounted
syncthing sees a missing marker and puts the folder in an **error** state rather
than treating it as "everything was deleted". That is the real protection against
the unmounted-drive disaster; receive-only is extra armor.

## Error handling

- Probe check fails → that check emits `{"state":"err"}`, the report shows it,
  the plan degrades (a step whose precondition is unknown is *offered*, never
  auto-run).
- ssh dies mid-apply → the step is reported failed with its full log; re-running
  the wizard re-probes and picks up where reality actually is. No state file, no
  resume bookkeeping — the box *is* the state.
- Any node unreachable during the device mesh → that node is reported and
  skipped; re-run completes it. Partial mesh is not corrupt, just incomplete.

## Testing

- `00-probe.sh` parsing: golden NDJSON fixtures (a Pi 2 with an HDD, an x86 box
  with no external drive, a box with syncthing already installed) → assert the
  parsed `Probe` and the derived `[]Step`. This is where the logic lives, and it
  is all pure.
- `02-plan.go`: table test — probe state in, expected steps out. Especially: no
  `hd-idle` when `rota=0`; no steps at all on a fully provisioned box.
- `05-adopt.go`: known / guessed / orphan bucketing against a fake swarm config.
- Live: run against `rue`, record timings.

## Measurement

The wizard writes `docs/nodes/<name>.md`: box specs, wall time per probe check,
benchmark numbers. So the next box has a baseline, and so we can see which check
to cut. **Target: probe under 2 s.**

## Out of scope

- Web wizard (the package is shaped so it can be added over the same functions).
- Tailscale/ssh setup itself — assumed already done on the box.
- Editing `sshd_config`, ever.
- Adopting orphan folders into the swarm.
