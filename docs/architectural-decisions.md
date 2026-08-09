# Architectural decisions

Rulings made while building. Each entry: what was decided, why, and what it
cost. Newest last.

## 2026-08-09 — an fstabbed-but-unmounted drive gets its own step

**Context.** Task 1 of the idempotent-bootstrap plan narrowed
`UnmountedDrives()` so an SD card (`TRAN=mmc HOTPLUG=1`) and any partition
already named in `/etc/fstab` stop being offered as drives to adopt. The task
review pointed out the hole this opened: a partition that is in fstab but not
mounted right now is then in neither `Drives()` (not mounted) nor
`UnmountedDrives()` (already configured), so the wizard reports "no external
drive attached — attach the drive first" about a disk that is plugged in and
fully configured. A drive that failed to mount this boot is the most common way
one of these boxes breaks.

**Decision.** Keep the narrowed filter and add `Probe.ConfiguredUnmounted()`
plus a `mount-configured:<mountpoint>` step that runs `mount <mp>` and nothing
else — no fstab edit, because the entry already exists.

**Why not the alternatives.** Dropping the fstab guard would restore the old
behaviour but keeps conflating two different situations ("a new disk to adopt"
and "a configured disk that did not come up"), and the wizard's whole direction
is toward naming the actual state of the box. Reporting the case as a finding
and leaving the fix to a human contradicts the goal that started this work:
bootstrap should recover, not narrate.

**Cost.** One helper, one step, two tests, folded into Task 2.

## 2026-08-09 — syncthing steps do not declare a cross-stage dependency

**Context.** The design doc listed "syncthing binary needs the mount step" in
the `Needs` table. The syncthing stage is planned by `PlanSyncthing`, which
cannot see the hardening ledger, and step IDs for mounts are per-mountpoint.

**Decision.** No cross-stage `Needs` edge. The requirement is enforced more
strongly a step earlier: `layoutFor` runs against a fresh probe and returns an
error when no data drive is mounted, so the syncthing stage cannot be planned at
all without one.

**Cost.** None. The `Needs` graph stays within a single stage, which is also what
keeps it sparse enough not to re-create the deadlock this work removes.

## 2026-08-09 — ssh exit 255 is a transport failure, never a predicate answer

**Context.** `SSHExecutor.Satisfied` runs a step's `Check` over ssh and maps the
exit status to a boolean: zero means the box is already in the wanted state,
non-zero means it is not. But ssh reports its OWN failures — unreachable host,
dropped connection, auth failure — as exit status 255, which arrives through
`exec.Cmd` looking exactly like a predicate that answered "no".

**Decision.** Only exit codes 0–254 count as the predicate answering. 255 is
surfaced as an error, and the step is recorded failed with "could not reach the
box".

**Why.** The two ways of being wrong are not symmetric. Reading a dead
connection as "not satisfied" makes the wizard offer to re-apply finished work
on a box it can no longer reach — the same class of failure this whole plan
exists to remove. Reading a genuine 255 from a remote command as "cannot verify"
merely asks the user to look. Every `Check` in this codebase is `test`, `grep`,
`findmnt`, `command -v` or `systemctl is-active`; none of them exit 255.

**Cost.** One exit-code comparison, and two tests for the transport-error paths
that the original test fake could not reach at all.
