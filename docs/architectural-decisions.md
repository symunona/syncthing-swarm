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

## 2026-08-09 — the syncthing gate is a mounted drive, not an empty plan

**Context.** `cmdBootstrap` used to re-probe after applying and return early if any
step remained. Skipped and failed were indistinguishable, so declining a step you
never wanted — an SD card wrongly offered as a data drive — permanently prevented
the syncthing install. That is the bug that made a real fiona run install nothing.

**Decision.** The hardening ledger no longer gates the syncthing stage. Unmet steps
print a warning and the stage proceeds. The one hard requirement is a mounted data
drive, enforced by `layoutFor` against a fresh probe.

**Why.** Every other hardening step is advisory: hd-idle, inotify limits and ufw all
improve a node without being preconditions for syncthing running correctly. A missing
data drive is different in kind — without it the folders would land on the boot media,
which is the failure the whole layout exists to prevent.

**Known gap.** `cmdBootstrap` takes a concrete `*provision.SSH` and calls `os.Exit`,
so this gate has no unit test; its regression guard greps the source for the old
condition. The mechanism underneath it (a skipped step blocks only its dependents)
is properly tested in `TestRunBlocksOnlyDependents`, and the end-to-end proof is
provisioning a real box. Making the wizard unit-testable means injecting the ssh
transport — worth doing, but not inside this change.

## 2026-08-09 — the pinned syncthing release is a floor

**Context.** `SyncthingRelease` pins the version new nodes install, and the
install `Check` asserted equality. But tarball installs deliberately keep
syncthing's auto-upgrade enabled, so a healthy node moves ahead of the pin on
its own — fiona came up on 2.1.2 with 2.1.3 released and a 12-hour upgrade
interval.

**Decision.** The pin is a floor. The Check compares with `sort -V` and accepts
any installed version `>=` the constant.

**Why.** Equality plus auto-upgrade is a loop: the node upgrades itself, the
check reads false, the wizard reinstalls the older pin over a newer working
binary, and auto-upgrade undoes it — every run, forever. A floor expresses what
was actually meant: never provision a node older than this.

**Cost.** A version comparison in shell instead of a grep. Bump the constant when
new nodes should start current; existing nodes need no help.
