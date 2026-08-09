# stc bootstrap: idempotent, recoverable, per-step verified

Date: 2026-08-09
Status: approved, not yet implemented

## Problem

`stc bootstrap fiona -syncthing` was run on 2026-08-09 and installed nothing.
The probe still reports `syncthing not installed`; there is no binary, no
`~/.config/syncthing`, no unit.

Three defects combined:

1. **A false drive.** `UnmountedDrives()` (`internal/provision/provision.go:349`)
   accepts any disk with `Tran == "usb" || Hotplug`. A Raspberry Pi's SD card
   reports `TRAN=mmc HOTPLUG=1`, so fiona's leftover boot card — the box now
   boots from the SSD at `/dev/sda2` — was offered as two mount steps
   (`/mnt/bootfs`, `/mnt/rootfs`). Both are nonsense.

2. **Declining a step is permanent.** After applying, `cmdBootstrap` re-probes
   and returns early when any step remains (`cmd/stc/bootstrap.go:140`). A
   skipped step is indistinguishable from a failed one, so declining the SD
   mounts blocked the syncthing stage forever. There is no way past it.

3. **Re-provisioning a known node is refused.** `AppendNode`
   (`internal/provision/04-join.go:36`) errors when the name already exists.
   fiona is in `swarm.yaml` with values from its previous life (`mount:
   /media/hdd`, `root: /media/hdd/syncthing`, a stale apikey) while the box now
   has `/srv/data`. Even a successful install could not have joined.

The wizard is already idempotent in one direction: `Plan()` derives steps from
the probe, so satisfied work shows up in `done` rather than as a step. What is
missing is per-step verification, and a failure model that isolates damage
instead of halting the run.

## Design

### 1. Steps carry a predicate and a dependency

```go
type Step struct {
    ID, Title, Why, Warn, Confirm string
    Cmds  []string
    Check []string  // root shell predicate; exit 0 means already satisfied
    Needs []string  // step IDs that must be satisfied before this one runs
}
```

`Check` runs twice per step:

- **before** the commands — satisfied means the step is recorded `already` and
  nothing runs. This is what makes a re-run cheap and safe.
- **after** the commands — not satisfied means `failed`, even when every command
  exited 0. This catches the half-applied step, which today passes silently.

Checks are authored beside the commands they verify:

| step | check |
|---|---|
| inotify | `[ "$(cat /proc/sys/fs/inotify/max_user_watches)" -ge 204800 ]` |
| mount | `findmnt -n <mountpoint>` |
| hd-idle | `systemctl is-active --quiet hd-idle` |
| syncthing binary | `/usr/local/bin/syncthing --version \| grep -q "v2.1.2"` |
| syncthing unit | `systemctl is-active --quiet syncthing@<user>` |
| GUI bind | `grep -q "<address><tailnet-ip>:8384</address>" <config.xml>` |
| apt-update | `find /var/lib/apt/lists -maxdepth 1 -name '*Packages*' -newermt '-1 day' \| grep -q .` |

Every step that `Plan` and `PlanSyncthing` emit must set `Check`. A step without
one is a bug; a test asserts the field is non-empty for all generated steps.
`apt-update` has no end state of its own, so its check is index freshness: a
list refreshed within a day is good enough, and re-running the wizard twice in a
row no longer pays for it twice.

`Needs` is deliberately sparse — only where one step genuinely cannot work
without another:

| step | needs |
|---|---|
| hd-idle | `apt-update` (installs a package) |
| syncthing binary | the mount step for the chosen data drive, when one was planned |
| syncthing unit | syncthing binary |
| GUI bind | syncthing unit |

inotify, fstab and ufw depend on nothing and never block anything.

### 2. A runner that isolates failure

`provision.Run(ctx, ssh, steps, opts)` replaces the apply loop in
`cmd/stc/bootstrap.go`. It walks steps in order and returns a ledger — one entry
per step, with a state:

| state | meaning |
|---|---|
| `already` | `Check` passed before any command ran |
| `ok` | applied, and the post-`Check` confirmed it |
| `skipped` | the user declined at the prompt |
| `failed` | a command failed, or the post-`Check` did not confirm |
| `blocked` | some step named in `Needs` is neither `ok` nor `already` |

No global abort. A failure propagates only along `Needs`; independent steps
still run. The run ends by printing the ledger and exits non-zero if anything is
`failed`.

Recovery is re-running the command. Because state lives on the box and `Check`
reads it, a second run skips what succeeded and retries what did not. No journal
file, no resume token.

### 3. The syncthing stage gates on reality, not on an empty plan

The `len(remaining) > 0 → return` gate is replaced. The syncthing stage requires
exactly one thing: a mounted data drive, which `layoutFor` already demands and
errors without. Everything else — hd-idle, inotify, ufw — is advisory: unmet
items print as warnings above the syncthing plan and the stage proceeds.

Declining a step you do not want must never cost you the install.

### 4. mmc is not removable storage

`UnmountedDrives()` requires `Tran == "usb"`, or hotplug that is not `mmc`. It
additionally skips any partition whose UUID already appears in `/etc/fstab` —
already-configured media is not an unmounted drive awaiting adoption.

Mounted mmc partitions are unaffected: a genuinely SD-booted Pi keeps working
because its partitions are mounted and therefore never in this set.

### 5. Join becomes upsert

`AppendNode` is replaced by `UpsertNode`. When the node name is absent the
behavior is unchanged. When it is present, the wizard prints a field-by-field
diff (`url`, `apikey`, `root`, `mount`, `ssh`), asks once, and rewrites only the
changed values in place, preserving surrounding comments and every other node.
Declining leaves the file untouched and reports what would have changed.

### 6. Colour and status glyphs

`Renderer` already detects a tty (`internal/provision/01-report.go:22`) and uses
`✓ ✗ -`. A small palette extends it, in the same file, active only when the
writer is a tty and `NO_COLOR` is unset:

| glyph | colour | used for |
|---|---|---|
| `✓` | green | `ok`, `already`, satisfied probe check |
| `✗` | red | `failed` |
| `⊘` | yellow | `skipped` |
| `⋯` | dim | `blocked` |
| `⚠` | yellow | warnings, findings |
| `→` | cyan | step titles, section headers |

Commands echoed before a step stay dim; remote output keeps its `│` prefix.
Piped or redirected output is plain ASCII-plus-glyph with no escape sequences,
so the existing "degrades to sane plain text" property holds.

## Testing

- `Plan` given a probe with an unmounted `mmcblk0` emits no mount steps.
- `Plan` given an unmounted USB partition still emits its mount step.
- Every step produced by `Plan` and `PlanSyncthing` has a non-empty `Check`.
- Runner: a step whose `Check` passes up front runs zero commands.
- Runner: a failed step marks its dependents `blocked` and leaves independent
  steps `ok`.
- Runner: commands succeed but post-`Check` fails → `failed`.
- `UpsertNode` rewrites an existing node's changed fields, preserves comments,
  and leaves other nodes byte-identical.
- Colour helpers emit no escape sequences when the writer is not a tty.

## Out of scope

Folder creation and sharing stay a separate, deliberate act. Nothing here edits
the ssh access path — password auth and exposed ports remain reported, never
changed.

## Verification on real hardware

After `just deploy`, run `stc bootstrap fiona -syncthing` and confirm: no SD
mount steps offered, syncthing 2.1.2 installed, unit active, GUI bound to
`100.86.131.51:8384`, and fiona's `swarm.yaml` entry rewritten to `/srv/data`
with the new apikey.
