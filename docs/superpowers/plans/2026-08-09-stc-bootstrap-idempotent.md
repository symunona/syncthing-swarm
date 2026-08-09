# stc bootstrap: idempotent + recoverable — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `stc bootstrap` verify every step against the box, isolate failures to their dependents, and re-provision a rebuilt node without hand-editing `swarm.yaml`.

**Architecture:** Each `Step` gains a `Check` shell predicate (run before and after its commands) and a sparse `Needs` list. A new runner walks the steps and returns a ledger — `already` / `ok` / `skipped` / `failed` / `blocked` — instead of aborting the run at the first failure. The syncthing stage stops gating on "the hardening plan is empty" and gates on a mounted data drive. `AppendNode` becomes `UpsertNode`. Output gets a colour palette that switches itself off when stdout is not a tty.

**Tech Stack:** Go 1.x, stdlib only (the module's only non-stdlib dependency is `gopkg.in/yaml.v3` — do not add more). Tests are plain `go test`, run via `just test`.

## Global Constraints

- No new module dependencies. stdlib + `gopkg.in/yaml.v3` only.
- No step may edit the ssh access path (`sshd_config`, `authorized_keys`, user shells). `02-plan_test.go:518` enforces this; keep it passing.
- `Check` predicates run **without sudo**, as the ssh login user. Author them against world-readable state (`/proc/sys/...`, `systemctl is-active`, `findmnt`, files under `/etc` that are 0644). A check that needs root is a design error — pick a different observable.
- Comments in this codebase explain *why*, at length, and often cite the incident that motivated the code. Match that; do not write `// set the flag`.
- Every commit must leave `go test ./...` green.
- The syncthing release pinned in `03-syncthing.go` is `SyncthingRelease = "2.1.2"`. Checks that assert a version must read that constant, never a literal.
- After the final task, `just deploy` (CLAUDE.md requires the local :8888 dashboard to match the code).

---

### Task 1: SD cards stop looking like unmounted data drives

The bug that wasted the fiona run: a Pi's SD card reports `TRAN=mmc HOTPLUG=1`, so
`UnmountedDrives()` offered fiona's dead boot card as two mount steps.

**Files:**
- Modify: `internal/provision/provision.go:342-370` (`UnmountedDrives`)
- Test: `internal/provision/02-plan_test.go` (append)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func (p *Probe) UnmountedDrives() []Drive` — unchanged signature, narrower result.

- [ ] **Step 1: Write the failing test**

Append to `internal/provision/02-plan_test.go`:

```go
// A Raspberry Pi's SD card reports TRAN=mmc HOTPLUG=1, which is
// indistinguishable from removable storage by the hotplug flag alone. fiona
// boots from an SSD and still has its old boot card in the slot; offering to
// mount that card at /mnt/rootfs is nonsense, and worse, declining the step
// used to block the whole install.
func TestUnmountedDrivesIgnoresMMC(t *testing.T) {
	p := &Probe{
		Disks: []BlockDevice{{
			Name: "mmcblk0", Path: "/dev/mmcblk0", Type: "disk",
			Tran: "mmc", Hotplug: true, Size: 31_000_000_000,
			Children: []BlockDevice{
				{Name: "mmcblk0p1", Path: "/dev/mmcblk0p1", Type: "part", FSType: "vfat", UUID: "0B22-2966"},
				{Name: "mmcblk0p2", Path: "/dev/mmcblk0p2", Type: "part", FSType: "ext4", UUID: "3ad7386b-e1ae-4032-ae33-0c40f5ecc4ac"},
			},
		}},
	}
	if got := p.UnmountedDrives(); len(got) != 0 {
		t.Errorf("SD card offered as a data drive: %+v", got)
	}
}

// The real case must keep working: a USB disk plugged into a headless box does
// not auto-mount, and finding it is the whole point of the check.
func TestUnmountedDrivesFindsUSB(t *testing.T) {
	p := &Probe{
		Disks: []BlockDevice{{
			Name: "sdb", Path: "/dev/sdb", Type: "disk",
			Tran: "usb", Hotplug: true, Rota: true, Size: 500_000_000_000,
			Children: []BlockDevice{
				{Name: "sdb1", Path: "/dev/sdb1", Type: "part", FSType: "ext4", UUID: "aaaa-bbbb"},
			},
		}},
	}
	got := p.UnmountedDrives()
	if len(got) != 1 || got[0].Device != "/dev/sdb1" {
		t.Fatalf("USB drive not found: %+v", got)
	}
}

// A partition already described by fstab is configured storage, not something
// awaiting adoption — it is unmounted right now for a reason (a drive that is
// powered off, a mount that failed this boot), and re-adding it to fstab would
// duplicate the entry.
func TestUnmountedDrivesSkipsFstabbedPartitions(t *testing.T) {
	p := &Probe{
		Disks: []BlockDevice{{
			Name: "sdb", Path: "/dev/sdb", Type: "disk", Tran: "usb", Hotplug: true,
			Children: []BlockDevice{
				{Name: "sdb1", Path: "/dev/sdb1", Type: "part", FSType: "ext4", UUID: "aaaa-bbbb"},
			},
		}},
		Fstab: []string{"UUID=aaaa-bbbb\t/srv/data\text4\tdefaults,nofail\t0\t0"},
	}
	if got := p.UnmountedDrives(); len(got) != 0 {
		t.Errorf("already-fstabbed partition offered as a new drive: %+v", got)
	}
}
```

Check `Probe.Fstab`'s actual type before writing that literal — read
`internal/provision/provision.go` around `Fstab` and `FstabEntry` and match it.

- [ ] **Step 2: Run the tests, watch them fail**

```bash
go test ./internal/provision/ -run 'TestUnmountedDrives' -v
```

Expected: `TestUnmountedDrivesIgnoresMMC` and
`TestUnmountedDrivesSkipsFstabbedPartitions` FAIL (one drive returned where zero
were wanted). `TestUnmountedDrivesFindsUSB` already passes — that is the
regression guard.

- [ ] **Step 3: Narrow the filter**

In `internal/provision/provision.go`, replace the removable-media test inside
`UnmountedDrives` (currently `if disk.Tran != "usb" && !disk.Hotplug { continue }`):

```go
		// Removable media only — never offer to mount an unmounted OS partition.
		//
		// Hotplug alone is not enough. A Raspberry Pi's SD card reports
		// TRAN=mmc HOTPLUG=1, so a box that has been migrated to an SSD and
		// still has its old boot card in the slot had that dead card offered as
		// two mount steps (/mnt/bootfs, /mnt/rootfs). Declining them then
		// blocked the syncthing install outright. mmc is never the data drive
		// on these boxes: it is the boot media, mounted or spare.
		if disk.Tran != "usb" && (!disk.Hotplug || disk.Tran == "mmc") {
			continue
		}
```

and inside the partition loop, after the existing `FSType`/`Mountpoint` guard:

```go
			// Already in fstab: configured storage that happens to be unmounted
			// right now (drive powered off, mount failed this boot). Adopting it
			// would append a second fstab line for the same UUID.
			if p.InFstab(part.UUID) {
				continue
			}
```

- [ ] **Step 4: Run the tests**

```bash
go test ./internal/provision/ -run 'TestUnmountedDrives' -v && go test ./...
```

Expected: all three PASS, whole suite PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provision/provision.go internal/provision/02-plan_test.go
git commit -m "probe: SD card is not an unmounted data drive"
```

---

### Task 2: Steps carry a Check predicate and a Needs list

**Files:**
- Modify: `internal/provision/02-plan.go:9-20` (`Step`), and every `Step{...}` literal in `Plan`
- Modify: `internal/provision/02-plan_test.go:8-15` (`stepByID` helper)
- Test: `internal/provision/02-plan_test.go` (append)

**Interfaces:**
- Consumes: Task 1's narrowed `UnmountedDrives`.
- Produces:
  - `Step.Check []string` — predicate commands, joined with `&&`, run unprivileged; exit 0 means "already satisfied".
  - `Step.Needs []string` — step IDs that must be satisfied first.
  - Step IDs that can occur more than once per plan become `"<kind>:<mountpoint>"` (`mount:/mnt/data`, `fstab:/srv/data`, `remount:/srv/data`). Single-occurrence IDs (`apt-update`, `inotify`, `ufw`, `fail2ban`, `fail2ban-repair`, `unattended-upgrades`, `usb-power`, `hd-idle`) are unchanged.

- [ ] **Step 1: Extend the struct**

In `internal/provision/02-plan.go`:

```go
type Step struct {
	ID    string
	Title string
	Why   string   // why this matters — shown at the prompt, not buried in a doc
	Cmds  []string // run in order, as root, via `ssh -t sudo sh -c`
	Warn  string   // extra warning shown before the prompt

	// Check is a predicate for the step's END STATE, joined with && and run as
	// the LOGIN USER, not root. Exit 0 means the box is already in the wanted
	// state.
	//
	// It runs twice: before the commands, so a re-run costs nothing and applies
	// nothing; and after them, so a step whose commands all exited 0 while the
	// box did not actually change is reported failed instead of silently
	// counting as done. The second half is the one that matters — the wizard
	// used to trust exit codes, and a half-applied step looked identical to a
	// finished one.
	//
	// Unprivileged on purpose: a sudo prompt in the middle of a read-only check
	// is both surprising and unnecessary. Everything worth checking here is
	// observable without root.
	Check []string

	// Needs names step IDs that must be satisfied before this one can run. Kept
	// deliberately sparse: only where a step genuinely cannot work without
	// another (installing a package needs a fresh apt index). Steps that merely
	// happen to be related must NOT be chained — an over-connected graph
	// re-creates the failure this replaces, where declining one step stops
	// everything downstream.
	//
	// An ID that is not in the plan at all counts as satisfied: nothing was
	// planned, so there is nothing to wait for.
	Needs []string

	// Confirm, when set, is a word the user must type exactly — a bare "y" is
	// not enough. Reserved for steps that can sever your own access.
	Confirm string
}
```

- [ ] **Step 2: Write the failing tests**

Append to `internal/provision/02-plan_test.go`:

```go
// A step without a Check cannot be verified, cannot be skipped when it is
// already done, and cannot be re-run safely. There is no such thing as a
// legitimate check-less step, so assert it structurally rather than trusting
// review.
func TestEveryPlannedStepHasACheck(t *testing.T) {
	for _, fixture := range []string{"rue-pi1b.ndjson", "rue-provisioned.ndjson"} {
		p := parseFixture(t, fixture)
		steps, _ := Plan(p, "symunona")
		for _, s := range steps {
			if len(s.Check) == 0 {
				t.Errorf("%s: step %q has no Check", fixture, s.ID)
			}
		}
	}
}

// Checks run as the login user. A check that shells out to sudo would either
// prompt for a password in the middle of a read-only verification, or fail
// outright on a box without passwordless sudo — which is every fresh box.
func TestChecksDoNotUseSudo(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")
	for _, s := range steps {
		if strings.Contains(strings.Join(s.Check, " "), "sudo") {
			t.Errorf("step %q has a Check that needs sudo: %v", s.ID, s.Check)
		}
	}
}

// Anything that installs a package needs the apt index refreshed first, and
// nothing else needs anything. An over-connected graph would re-create the
// deadlock this whole change removes.
func TestNeedsIsSparseAndPointsAtRealSteps(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")
	ids := map[string]bool{}
	for _, s := range steps {
		ids[s.ID] = true
	}
	for _, s := range steps {
		installs := strings.Contains(strings.Join(s.Cmds, " "), "install -y")
		needsApt := false
		for _, n := range s.Needs {
			if !ids[n] {
				t.Errorf("step %q needs %q, which is not in the plan", s.ID, n)
			}
			if n == "apt-update" {
				needsApt = true
			}
		}
		if installs && !needsApt {
			t.Errorf("step %q installs a package but does not need apt-update", s.ID)
		}
		if !installs && len(s.Needs) > 0 {
			t.Errorf("step %q needs %v but installs nothing — keep Needs sparse", s.ID, s.Needs)
		}
	}
}

// Two unmounted drives would otherwise both be called "mount", and the ledger
// and Needs graph key on ID.
func TestStepIDsAreUnique(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")
	seen := map[string]bool{}
	for _, s := range steps {
		if seen[s.ID] {
			t.Errorf("duplicate step ID %q", s.ID)
		}
		seen[s.ID] = true
	}
}
```

Also relax the existing helper at `internal/provision/02-plan_test.go:8` so the
older tests keep finding per-drive steps by their kind:

```go
func stepByID(steps []Step, id string) *Step {
	for i := range steps {
		// Per-drive steps are keyed "<kind>:<mountpoint>" so two drives cannot
		// collide; callers still ask for the kind.
		if steps[i].ID == id || strings.HasPrefix(steps[i].ID, id+":") {
			return &steps[i]
		}
	}
	return nil
}
```

- [ ] **Step 3: Run the tests, watch them fail**

```bash
go test ./internal/provision/ -run 'TestEveryPlannedStepHasACheck|TestChecksDoNotUseSudo|TestNeedsIsSparse|TestStepIDsAreUnique' -v
```

Expected: FAIL — every step reports "has no Check"; installs report a missing
`apt-update` dependency.

- [ ] **Step 4: Fill in Check and Needs across `Plan`**

Edit each `Step` literal in `internal/provision/02-plan.go`. Exact values:

```go
// inotify (ID "inotify")
Check: []string{fmt.Sprintf("[ \"$(cat /proc/sys/fs/inotify/max_user_watches)\" -ge %d ]", want)},

// unmounted drive (ID becomes "mount:"+mp)
ID:    "mount:" + mp,
Check: []string{"findmnt -n " + mp + " >/dev/null"},

// fstab (ID becomes "fstab:"+d.Mountpoint)
ID:    "fstab:" + d.Mountpoint,
Check: []string{fmt.Sprintf("grep -q '%s' /etc/fstab", d.UUID)},

// remount (ID becomes "remount:"+d.Mountpoint)
ID:    "remount:" + d.Mountpoint,
Check: []string{fmt.Sprintf("findmnt -no OPTIONS %s | grep -q noatime", d.Mountpoint)},

// usb-power
Check: []string{"CFG=/boot/firmware/config.txt; [ -f $CFG ] || CFG=/boot/config.txt; grep -q '^max_usb_current=1' $CFG"},

// hd-idle
Check: []string{"systemctl is-active --quiet hd-idle"},
Needs: []string{"apt-update"},

// ufw — `ufw status` needs root, so check the observables that do not.
Check: []string{"command -v ufw >/dev/null", "systemctl is-active --quiet ufw"},
Needs: []string{"apt-update"},

// fail2ban (installs the package)
Check: []string{"systemctl is-active --quiet fail2ban", "test -f /etc/fail2ban/jail.local"},
Needs: []string{"apt-update"},

// fail2ban-repair — SAME Check, but it installs nothing, so no Needs at all
// (TestNeedsIsSparseAndPointsAtRealSteps enforces this).
Check: []string{"systemctl is-active --quiet fail2ban", "test -f /etc/fail2ban/jail.local"},

// unattended-upgrades
Check: []string{"test -f /etc/apt/apt.conf.d/20auto-upgrades", "systemctl is-enabled --quiet unattended-upgrades"},
Needs: []string{"apt-update"},

// apt-update
Check: []string{"find /var/lib/apt/lists -maxdepth 1 -name '*Packages*' -newermt '-1 day' | grep -q ."},
```

- [ ] **Step 5: Run the tests**

```bash
go test ./... 
```

Expected: PASS, including the pre-existing `TestPlanRue`, which finds `fstab`
through the relaxed helper.

- [ ] **Step 6: Commit**

```bash
git add internal/provision/02-plan.go internal/provision/02-plan_test.go
git commit -m "plan: steps carry a Check predicate and sparse Needs"
```

---

### Task 3: A runner that verifies each step and isolates failure

**Files:**
- Create: `internal/provision/02-run.go`
- Create: `internal/provision/02-run_test.go`
- Modify: `internal/provision/02-apply.go` (append `SSHExecutor`)

**Interfaces:**
- Consumes: `Step.Check`, `Step.Needs` from Task 2; `Apply(ctx, *SSH, Step, io.Writer) error` from `02-apply.go:19`.
- Produces:
  - `type State string` with `StateAlready`, `StateOK`, `StateSkipped`, `StateFailed`, `StateBlocked`
  - `type Result struct { Step Step; State State; Err error }`
  - `type Ledger []Result` with `Satisfied(id string) bool`, `Failed() []Result`, `Count(State) int`
  - `type Executor interface { Satisfied(context.Context, []string) (bool, error); Apply(context.Context, Step) error }`
  - `type RunOpts struct { Confirm func(Step) bool; Report func(Result) }`
  - `func Run(ctx context.Context, ex Executor, steps []Step, opts RunOpts) Ledger`
  - `type SSHExecutor struct { S *SSH; Out io.Writer }` implementing `Executor`

- [ ] **Step 1: Write the failing tests**

Create `internal/provision/02-run_test.go`:

```go
package provision

import (
	"context"
	"errors"
	"testing"
)

// fakeExec answers Satisfied from a set of step IDs and records what ran.
// checkAfter lets a test model the step that "succeeds" while changing nothing.
type fakeExec struct {
	satisfied  map[string]bool // step ID -> current answer
	applyErr   map[string]error
	applied    []string
	checkAfter map[string]bool // step ID -> what Satisfied returns AFTER Apply
	checks     int
}

// The fake keys checks by the step ID smuggled through Check[0], so the test
// does not have to write real shell.
func (f *fakeExec) Satisfied(_ context.Context, check []string) (bool, error) {
	f.checks++
	return f.satisfied[check[0]], nil
}

func (f *fakeExec) Apply(_ context.Context, s Step) error {
	f.applied = append(f.applied, s.ID)
	if err := f.applyErr[s.ID]; err != nil {
		return err
	}
	if after, ok := f.checkAfter[s.ID]; ok {
		f.satisfied[s.ID] = after
	} else {
		f.satisfied[s.ID] = true
	}
	return nil
}

func step(id string, needs ...string) Step {
	return Step{ID: id, Title: id, Check: []string{id}, Needs: needs}
}

func alwaysYes(Step) bool { return true }

// A box that is already in the wanted state must cost nothing: no commands, no
// prompts. This is what makes re-running the wizard the recovery mechanism.
func TestRunSkipsAlreadySatisfiedSteps(t *testing.T) {
	f := &fakeExec{satisfied: map[string]bool{"inotify": true}}
	led := Run(context.Background(), f, []Step{step("inotify")}, RunOpts{Confirm: alwaysYes})

	if len(f.applied) != 0 {
		t.Errorf("ran commands for an already-satisfied step: %v", f.applied)
	}
	if led[0].State != StateAlready {
		t.Errorf("state = %q, want %q", led[0].State, StateAlready)
	}
}

// The half-applied step: every command exits 0, the box does not change. This
// used to be indistinguishable from success.
func TestRunFailsWhenPostCheckDoesNotConfirm(t *testing.T) {
	f := &fakeExec{
		satisfied:  map[string]bool{"hd-idle": false},
		checkAfter: map[string]bool{"hd-idle": false},
	}
	led := Run(context.Background(), f, []Step{step("hd-idle")}, RunOpts{Confirm: alwaysYes})

	if led[0].State != StateFailed {
		t.Fatalf("state = %q, want %q", led[0].State, StateFailed)
	}
	if led[0].Err == nil {
		t.Error("failed step carries no error to show the user")
	}
}

// The fiona deadlock, in miniature: declining one step must not cost you the
// steps that do not depend on it.
func TestRunBlocksOnlyDependents(t *testing.T) {
	f := &fakeExec{satisfied: map[string]bool{}}
	steps := []Step{step("apt-update"), step("hd-idle", "apt-update"), step("inotify")}

	led := Run(context.Background(), f, steps, RunOpts{
		Confirm: func(s Step) bool { return s.ID != "apt-update" },
	})

	want := map[string]State{"apt-update": StateSkipped, "hd-idle": StateBlocked, "inotify": StateOK}
	for _, r := range led {
		if want[r.Step.ID] != r.State {
			t.Errorf("%s: state = %q, want %q", r.Step.ID, r.State, want[r.Step.ID])
		}
	}
	for _, id := range f.applied {
		if id == "hd-idle" {
			t.Error("blocked step ran anyway")
		}
	}
}

// A failure propagates the same way a decline does.
func TestRunBlocksDependentsOfAFailure(t *testing.T) {
	f := &fakeExec{
		satisfied: map[string]bool{},
		applyErr:  map[string]error{"apt-update": errors.New("dpkg lock held")},
	}
	led := Run(context.Background(), f, []Step{step("apt-update"), step("ufw", "apt-update")},
		RunOpts{Confirm: alwaysYes})

	if led[0].State != StateFailed || led[1].State != StateBlocked {
		t.Errorf("states = %q, %q; want failed, blocked", led[0].State, led[1].State)
	}
}

// A Needs entry naming a step that was never planned means the requirement is
// already met — the planner only emits steps for work that is outstanding.
func TestRunTreatsUnplannedNeedsAsSatisfied(t *testing.T) {
	f := &fakeExec{satisfied: map[string]bool{}}
	led := Run(context.Background(), f, []Step{step("hd-idle", "apt-update")},
		RunOpts{Confirm: alwaysYes})

	if led[0].State != StateOK {
		t.Errorf("state = %q, want %q — apt-update was never planned, so nothing blocks", led[0].State, StateOK)
	}
}

func TestLedgerReportsFailures(t *testing.T) {
	led := Ledger{
		{Step: step("a"), State: StateOK},
		{Step: step("b"), State: StateFailed, Err: errors.New("boom")},
	}
	if len(led.Failed()) != 1 || led.Failed()[0].Step.ID != "b" {
		t.Errorf("Failed() = %+v", led.Failed())
	}
	if !led.Satisfied("a") || led.Satisfied("b") {
		t.Error("Satisfied() disagrees with the ledger")
	}
	if !led.Satisfied("never-planned") {
		t.Error("an unplanned ID must count as satisfied")
	}
}
```

- [ ] **Step 2: Run the tests, watch them fail**

```bash
go test ./internal/provision/ -run 'TestRun|TestLedger' -v
```

Expected: compile failure — `undefined: Run`, `undefined: Ledger`.

- [ ] **Step 3: Write the runner**

Create `internal/provision/02-run.go`:

```go
package provision

import (
	"context"
	"fmt"
	"io"
)

// State is what became of one step in a run.
type State string

const (
	StateAlready State = "already" // Check passed before anything ran
	StateOK      State = "ok"      // applied, and the post-Check confirmed it
	StateSkipped State = "skipped" // the user declined
	StateFailed  State = "failed"  // a command failed, or the post-Check did not confirm
	StateBlocked State = "blocked" // something in Needs is not satisfied
)

// Result is one row of the ledger.
type Result struct {
	Step  Step
	State State
	Err   error
}

// Ledger is the outcome of a run: one Result per step, in plan order.
type Ledger []Result

// Satisfied reports whether a step ID ended the run in the wanted state.
//
// An ID with no row counts as satisfied. The planner only emits steps for work
// that is OUTSTANDING, so a dependency that never appeared is a dependency that
// was already met — a node whose drive is mounted plans no mount step, and the
// steps that need a mounted drive must not block on its absence.
func (l Ledger) Satisfied(id string) bool {
	for _, r := range l {
		if r.Step.ID == id {
			return r.State == StateOK || r.State == StateAlready
		}
	}
	return true
}

func (l Ledger) Failed() []Result {
	var out []Result
	for _, r := range l {
		if r.State == StateFailed {
			out = append(out, r)
		}
	}
	return out
}

func (l Ledger) Count(s State) int {
	n := 0
	for _, r := range l {
		if r.State == s {
			n++
		}
	}
	return n
}

// Executor is the box, as far as the runner is concerned. Splitting it out is
// what lets the run logic be tested without an ssh connection or a Pi.
type Executor interface {
	// Satisfied evaluates a Check predicate. It runs UNPRIVILEGED.
	Satisfied(ctx context.Context, check []string) (bool, error)
	// Apply runs the step's commands as root.
	Apply(ctx context.Context, s Step) error
}

// RunOpts carries the caller's UI. Both hooks are optional: a nil Confirm
// applies everything, a nil Report prints nothing.
type RunOpts struct {
	Confirm func(Step) bool
	Report  func(Result)
}

// Run walks the steps in order and returns a ledger.
//
// It does NOT abort on failure. The wizard used to stop dead at the first
// problem, which meant one unwanted step — an SD card offered as a data drive,
// say — permanently prevented everything after it, including the syncthing
// install the user actually came for. A failure now propagates only along
// Needs; independent work still gets done, and the ledger says exactly what did
// not.
//
// Recovery is re-running the command. State lives on the box, Check reads it,
// and a second run therefore skips what succeeded and retries what did not.
// There is no journal file to go stale.
func Run(ctx context.Context, ex Executor, steps []Step, opts RunOpts) Ledger {
	led := make(Ledger, 0, len(steps))

	record := func(r Result) {
		led = append(led, r)
		if opts.Report != nil {
			opts.Report(r)
		}
	}

	for _, s := range steps {
		if blocker, ok := firstUnsatisfied(led, s.Needs); !ok {
			record(Result{Step: s, State: StateBlocked,
				Err: fmt.Errorf("needs %q, which did not succeed", blocker)})
			continue
		}

		// Before: is the box already there? A re-run must cost nothing.
		done, err := ex.Satisfied(ctx, s.Check)
		if err != nil {
			record(Result{Step: s, State: StateFailed, Err: fmt.Errorf("check: %w", err)})
			continue
		}
		if done {
			record(Result{Step: s, State: StateAlready})
			continue
		}

		if opts.Confirm != nil && !opts.Confirm(s) {
			record(Result{Step: s, State: StateSkipped})
			continue
		}

		if err := ex.Apply(ctx, s); err != nil {
			record(Result{Step: s, State: StateFailed, Err: err})
			continue
		}

		// After: commands exiting 0 is not evidence the box changed.
		done, err = ex.Satisfied(ctx, s.Check)
		if err != nil {
			record(Result{Step: s, State: StateFailed, Err: fmt.Errorf("verify: %w", err)})
			continue
		}
		if !done {
			record(Result{Step: s, State: StateFailed,
				Err: fmt.Errorf("commands succeeded but the box did not change (check still fails: %v)", s.Check)})
			continue
		}
		record(Result{Step: s, State: StateOK})
	}
	return led
}

// firstUnsatisfied returns the first unmet dependency, if any.
func firstUnsatisfied(led Ledger, needs []string) (string, bool) {
	for _, n := range needs {
		if !led.Satisfied(n) {
			return n, false
		}
	}
	return "", true
}

// SSHExecutor runs steps on a real box.
type SSHExecutor struct {
	S   *SSH
	Out io.Writer
}

func (e SSHExecutor) Apply(ctx context.Context, s Step) error {
	return Apply(ctx, e.S, s, e.Out)
}

// Satisfied runs the predicate as the login user, with no tty and no sudo: a
// password prompt in the middle of a read-only verification would be both
// surprising and, on a fresh box, unanswerable.
//
// A non-zero exit means "not satisfied", not "error" — that is the whole point
// of a predicate. Only a transport failure is an error, and the caller turns
// that into a failed step rather than pretending the box is fine.
func (e SSHExecutor) Satisfied(ctx context.Context, check []string) (bool, error) {
	if len(check) == 0 {
		return false, fmt.Errorf("step has no Check")
	}
	script := "set -e; " + joinAnd(check)
	cmd := e.S.Command(ctx, false, "sh -c "+shellQuote(script))
	if err := cmd.Run(); err != nil {
		if isExitError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
```

Add the two small helpers to the same file (`shellQuote` already exists in
`02-apply.go`; reuse it, do not redefine):

```go
func joinAnd(cmds []string) string {
	return strings.Join(cmds, " && ")
}

// isExitError distinguishes "the predicate said no" from "ssh could not run it".
func isExitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}
```

with `errors`, `os/exec` and `strings` added to the imports.

- [ ] **Step 4: Run the tests**

```bash
go test ./internal/provision/ -run 'TestRun|TestLedger' -v && go test ./...
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provision/02-run.go internal/provision/02-run_test.go internal/provision/02-apply.go
git commit -m "provision: step runner with a ledger, blocks only dependents"
```

---

### Task 4: Checks and Needs for the syncthing steps

**Files:**
- Modify: `internal/provision/03-syncthing.go:83-188` (the three `Step` literals)
- Test: `internal/provision/03-syncthing_test.go` (create)

**Interfaces:**
- Consumes: `Step.Check` / `Step.Needs` (Task 2).
- Produces: `syncthing-install`, `syncthing-service`, `syncthing-gui` each with a
  `Check`; `syncthing-service` needs `syncthing-install`, `syncthing-gui` needs
  `syncthing-service`.

Note a deliberate deviation from the spec: the spec listed "syncthing binary
needs the mount step". That cross-stage edge is not implemented, because it is
already enforced more strongly — `layoutFor` re-probes and returns an error when
no data drive is mounted, so the syncthing stage cannot even be planned without
one. Do not add a `Needs` entry pointing at a hardening step ID; nothing in this
stage can see that ledger.

- [ ] **Step 1: Write the failing test**

Create `internal/provision/03-syncthing_test.go`:

```go
package provision

import (
	"strings"
	"testing"
)

func syncLayout() SyncthingLayout {
	return SyncthingLayout{
		User:      "pi",
		ConfigDir: "/home/pi/.config/syncthing",
		DataDir:   "/srv/data/syncthing-db",
		FolderDir: "/srv/data/syncthing",
		Mount:     "/srv/data",
		TailnetIP: "100.86.131.51",
	}
}

func syncProbe() *Probe {
	p := &Probe{}
	p.Box.Arch = "aarch64"
	return p
}

func TestSyncthingStepsHaveChecks(t *testing.T) {
	steps, err := PlanSyncthing(syncProbe(), syncLayout())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if len(s.Check) == 0 {
			t.Errorf("step %q has no Check", s.ID)
		}
		if strings.Contains(strings.Join(s.Check, " "), "sudo") {
			t.Errorf("step %q has a Check that needs sudo: %v", s.ID, s.Check)
		}
	}
}

// The install check must pin the version. A box left on an older syncthing
// after a bumped SyncthingRelease is out of date, not done.
func TestSyncthingInstallCheckPinsTheRelease(t *testing.T) {
	steps, _ := PlanSyncthing(syncProbe(), syncLayout())
	s := stepByID(steps, "syncthing-install")
	if s == nil {
		t.Fatal("no syncthing-install step")
	}
	if !strings.Contains(strings.Join(s.Check, " "), SyncthingRelease) {
		t.Errorf("install check does not mention v%s: %v", SyncthingRelease, s.Check)
	}
}

// The GUI must never end up on 0.0.0.0, so the check verifies the tailnet
// address is actually in config.xml rather than trusting sed's exit code.
func TestSyncthingGUICheckAssertsTheTailnetBind(t *testing.T) {
	l := syncLayout()
	steps, _ := PlanSyncthing(syncProbe(), l)
	s := stepByID(steps, "syncthing-gui")
	if s == nil {
		t.Fatal("no syncthing-gui step")
	}
	joined := strings.Join(s.Check, " ")
	if !strings.Contains(joined, l.TailnetIP+":8384") || !strings.Contains(joined, l.ConfigDir) {
		t.Errorf("gui check does not verify the bind address in config.xml: %v", s.Check)
	}
}

func TestSyncthingStepsChainTheirNeeds(t *testing.T) {
	steps, _ := PlanSyncthing(syncProbe(), syncLayout())
	want := map[string][]string{
		"syncthing-install": nil,
		"syncthing-service": {"syncthing-install"},
		"syncthing-gui":     {"syncthing-service"},
	}
	for _, s := range steps {
		got := strings.Join(s.Needs, ",")
		if got != strings.Join(want[s.ID], ",") {
			t.Errorf("%s: Needs = %v, want %v", s.ID, s.Needs, want[s.ID])
		}
	}
}
```

- [ ] **Step 2: Run the tests, watch them fail**

```bash
go test ./internal/provision/ -run 'TestSyncthing' -v
```

Expected: FAIL — every step reports "has no Check".

- [ ] **Step 3: Add the checks**

In `internal/provision/03-syncthing.go`, add to the three step literals:

```go
// syncthing-install
Check: []string{
	fmt.Sprintf("/usr/local/bin/syncthing --version 2>/dev/null | grep -q 'v%s'", v),
	"test -f /etc/systemd/system/syncthing@.service",
},

// syncthing-service
Needs: []string{"syncthing-install"},
Check: []string{
	fmt.Sprintf("test -d %s", l.DataDir),
	fmt.Sprintf("test -d %s", l.FolderDir),
	fmt.Sprintf("grep -q -- '--data=%s' /etc/systemd/system/%s.service.d/override.conf", l.DataDir, unit),
	"systemctl is-active --quiet " + unit,
},

// syncthing-gui
Needs: []string{"syncthing-service"},
Check: []string{
	fmt.Sprintf("grep -q '<address>%s:8384</address>' %s/config.xml", l.TailnetIP, l.ConfigDir),
	"systemctl is-active --quiet " + unit,
},
```

The override file is written 0644 by `printf > …`, so the login user can read
it — no sudo needed, as the tests require.

- [ ] **Step 4: Run the tests**

```bash
go test ./internal/provision/ -run 'TestSyncthing' -v && go test ./...
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provision/03-syncthing.go internal/provision/03-syncthing_test.go
git commit -m "syncthing: per-step checks, install -> service -> gui chain"
```

---

### Task 5: Join becomes upsert

**Files:**
- Modify: `internal/provision/04-join.go:24-65` (`AppendNode` → `UpsertNode` + `DiffNode`)
- Test: `internal/provision/04-join_test.go` (create)

**Interfaces:**
- Consumes: `config.Node`.
- Produces:
  - `type FieldChange struct { Field, Old, New string }`
  - `func DiffNode(path string, n config.Node) (changes []FieldChange, exists bool, err error)`
  - `func UpsertNode(path string, n config.Node) error` — appends when absent, rewrites the named node's scalar fields in place when present, preserving comments and every other node.

`AppendNode` is removed; the only caller is `cmd/stc/bootstrap.go:266`, rewired
in Task 7.

- [ ] **Step 1: Write the failing test**

Create `internal/provision/04-join_test.go`:

```go
package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/symunona/syncthing-dashboard/internal/config"
)

const swarmFixture = `listen: :8888
pollSeconds: 10
nodes:
  - name: pandora        # this machine (hub) — local syncthing
    url: http://127.0.0.1:8384
    apikey: AAA
    root: /home/symunona/syncthing

  - name: fiona          # rpi, tailscale
    url: http://100.86.131.51:8384
    mount: /media/hdd    # root's drive belongs here
    apikey: OLDKEY
    root: /media/hdd/syncthing
    ssh: fiona           # for disk stats (df over ssh)

  - name: taskbot        # tailscale, ssh :2222
    url: http://100.96.124.62:8384
    apikey: BBB
`

func writeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "swarm.yaml")
	if err := os.WriteFile(path, []byte(swarmFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func rebuiltFiona() config.Node {
	return config.Node{
		Name:   "fiona",
		URL:    "http://100.86.131.51:8384",
		APIKey: "NEWKEY",
		Root:   "/srv/data/syncthing",
		Mount:  "/srv/data",
		SSH:    "fiona",
	}
}

// A rebuilt box keeps its name and loses everything else. The diff is what the
// user is asked to approve, so it must name every field that actually moves and
// nothing that does not.
func TestDiffNodeReportsChangedFieldsOnly(t *testing.T) {
	path := writeFixture(t)
	changes, exists, err := DiffNode(path, rebuiltFiona())
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("fiona is in the fixture but DiffNode says it is not")
	}
	got := map[string]FieldChange{}
	for _, c := range changes {
		got[c.Field] = c
	}
	for field, want := range map[string]string{
		"apikey": "NEWKEY",
		"root":   "/srv/data/syncthing",
		"mount":  "/srv/data",
	} {
		if got[field].New != want {
			t.Errorf("%s: New = %q, want %q", field, got[field].New, want)
		}
	}
	if got["mount"].Old != "/media/hdd" {
		t.Errorf("mount: Old = %q, want /media/hdd", got["mount"].Old)
	}
	for _, unchanged := range []string{"url", "ssh", "name"} {
		if _, ok := got[unchanged]; ok {
			t.Errorf("%s reported as changed but it did not move", unchanged)
		}
	}
}

// swarm.yaml is a hand-maintained cred store. Round-tripping it through the
// marshaller would eat every comment in the file, which is why the original
// AppendNode appended text — the rewrite must respect the same rule.
func TestUpsertNodeRewritesInPlaceAndKeepsComments(t *testing.T) {
	path := writeFixture(t)
	if err := UpsertNode(path, rebuiltFiona()); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)

	if strings.Contains(s, "OLDKEY") || strings.Contains(s, "/media/hdd/syncthing") {
		t.Error("stale values survived the upsert")
	}
	if !strings.Contains(s, "apikey: NEWKEY") || !strings.Contains(s, "root: /srv/data/syncthing") {
		t.Error("new values not written")
	}
	if !strings.Contains(s, "# rpi, tailscale") || !strings.Contains(s, "# for disk stats (df over ssh)") {
		t.Error("comments were eaten")
	}
	if !strings.Contains(s, "apikey: AAA") || !strings.Contains(s, "apikey: BBB") {
		t.Error("another node was damaged")
	}
	// It must still parse, and fiona must be exactly one node.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("upserted file no longer loads: %v", err)
	}
	n := 0
	for _, node := range cfg.Nodes {
		if node.Name == "fiona" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("fiona appears %d times", n)
	}
}

// The absent case is the old AppendNode behaviour, unchanged.
func TestUpsertNodeAppendsWhenAbsent(t *testing.T) {
	path := writeFixture(t)
	err := UpsertNode(path, config.Node{
		Name: "rue", URL: "http://100.1.2.3:8384", APIKey: "CCC",
		Root: "/mnt/hdd/syncthing", Mount: "/mnt/hdd", SSH: "rue",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Nodes[len(cfg.Nodes)-1].Name != "rue" {
		t.Errorf("rue not appended: %+v", cfg.Nodes)
	}
}
```

- [ ] **Step 2: Run the tests, watch them fail**

```bash
go test ./internal/provision/ -run 'TestDiffNode|TestUpsertNode' -v
```

Expected: compile failure — `undefined: DiffNode`, `undefined: UpsertNode`.

- [ ] **Step 3: Implement the upsert**

In `internal/provision/04-join.go`, replace `AppendNode` with:

```go
// FieldChange is one scalar that an upsert would rewrite.
type FieldChange struct{ Field, Old, New string }

// nodeFields is the upsert's whole surface: the scalars bootstrap knows how to
// derive from a freshly provisioned box. `local` is deliberately absent — it is
// a human's declaration about which node the dashboard shares FROM, and the
// wizard has no business guessing it.
func nodeFields(n config.Node) []FieldChange {
	return []FieldChange{
		{Field: "url", New: n.URL},
		{Field: "apikey", New: n.APIKey},
		{Field: "root", New: n.Root},
		{Field: "mount", New: n.Mount},
		{Field: "ssh", New: n.SSH},
	}
}

// DiffNode reports what UpsertNode would change. exists is false when the node
// is new, in which case changes is nil and the upsert is a plain append.
func DiffNode(path string, n config.Node) (changes []FieldChange, exists bool, err error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, false, err
	}
	var cur *config.Node
	for i := range cfg.Nodes {
		if cfg.Nodes[i].Name == n.Name {
			cur = &cfg.Nodes[i]
			break
		}
	}
	if cur == nil {
		return nil, false, nil
	}
	old := map[string]string{
		"url": cur.URL, "apikey": cur.APIKey, "root": cur.Root,
		"mount": cur.Mount, "ssh": cur.SSH,
	}
	for _, f := range nodeFields(n) {
		// Never blank a field that swarm.yaml has and this run could not derive.
		if f.New == "" || f.New == old[f.Field] {
			continue
		}
		f.Old = old[f.Field]
		changes = append(changes, f)
	}
	return changes, true, nil
}

// UpsertNode writes the node into swarm.yaml: appended when new, rewritten in
// place when it is already there.
//
// Text surgery rather than a yaml.v3 round-trip, for the same reason the
// original AppendNode appended text: swarm.yaml is a hand-maintained cred store
// and the marshaller would eat every comment in it.
//
// The in-place case exists because a node outlives its hardware. fiona was
// rebuilt onto a new SSD and came back with a different drive path and a
// different API key, while its swarm.yaml entry still described the machine it
// used to be. Refusing to touch an existing entry — the old behaviour — meant
// the wizard could install syncthing on a box and then be unable to record the
// result.
func UpsertNode(path string, n config.Node) error {
	_, exists, err := DiffNode(path, n)
	if err != nil {
		return err
	}
	if !exists {
		return appendNode(path, n)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")

	start, end := nodeBlock(lines, n.Name)
	if start < 0 {
		return fmt.Errorf("node %q parsed from %s but its lines could not be found", n.Name, path)
	}

	want := map[string]string{}
	for _, f := range nodeFields(n) {
		if f.New != "" {
			want[f.Field] = f.New
		}
	}

	for i := start; i < end; i++ {
		key, _, comment, ok := splitScalar(lines[i])
		if !ok {
			continue
		}
		v, wanted := want[key]
		if !wanted {
			continue
		}
		indent := lines[i][:len(lines[i])-len(strings.TrimLeft(lines[i], " "))]
		lines[i] = fmt.Sprintf("%s%s: %s%s", indent, key, v, comment)
		delete(want, key)
	}

	// Fields the old entry never had (a node provisioned before `mount` existed)
	// get appended to the block rather than dropped.
	if len(want) > 0 {
		var add []string
		for _, f := range nodeFields(n) {
			if v, ok := want[f.Field]; ok {
				add = append(add, fmt.Sprintf("    %s: %s", f.Field, v))
			}
		}
		rest := append([]string{}, lines[end:]...)
		lines = append(lines[:end], append(add, rest...)...)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
}

// nodeBlock finds the line range of one node's YAML block: from its "- name:"
// line up to (not including) the next list item at the same indent, or EOF.
func nodeBlock(lines []string, name string) (start, end int) {
	start = -1
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if !strings.HasPrefix(t, "- name:") {
			continue
		}
		if start >= 0 {
			return start, i
		}
		// "- name: fiona          # rpi" -> "fiona"
		v := strings.TrimSpace(strings.TrimPrefix(t, "- name:"))
		if i := strings.Index(v, "#"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if v == name {
			start = i
		}
	}
	if start < 0 {
		return -1, -1
	}
	return start, len(lines)
}

// splitScalar takes "    apikey: OLD    # note" apart, keeping the trailing
// comment so a rewrite does not destroy it.
func splitScalar(line string) (key, value, comment string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "- ") {
		return "", "", "", false
	}
	i := strings.Index(t, ":")
	if i < 0 {
		return "", "", "", false
	}
	key = strings.TrimSpace(t[:i])
	rest := t[i+1:]
	if j := strings.Index(rest, "#"); j >= 0 {
		// keep the original spacing before the comment
		raw := line[strings.Index(line, "#"):]
		comment = "    " + raw
		rest = rest[:j]
	}
	return key, strings.TrimSpace(rest), comment, true
}
```

and rename the old body to the unexported `appendNode(path string, n config.Node) error`,
dropping its "already has a node called" guard — `UpsertNode` decides that now.

- [ ] **Step 4: Run the tests**

```bash
go test ./internal/provision/ -run 'TestDiffNode|TestUpsertNode' -v && go test ./...
```

Expected: PASS. `cmd/stc` will still compile because `AppendNode`'s caller is
untouched until Task 7 — if the rename breaks the build, keep a thin
`AppendNode` wrapper for one commit and delete it in Task 7.

- [ ] **Step 5: Commit**

```bash
git add internal/provision/04-join.go internal/provision/04-join_test.go
git commit -m "join: upsert a node instead of refusing a known name"
```

---

### Task 6: Colour and status glyphs

**Files:**
- Create: `internal/provision/color.go`
- Create: `internal/provision/color_test.go`
- Modify: `internal/provision/01-report.go:11-60` (`Renderer` uses the palette)

**Interfaces:**
- Consumes: `State` from Task 3.
- Produces:
  - `type Style struct { on bool }`
  - `func NewStyle(w io.Writer) Style` — on only when `w` is a tty and `NO_COLOR` is unset
  - `func (s Style) Green/Red/Yellow/Cyan/Dim/Bold(text string) string`
  - `func (s Style) Mark(st State) string` — the coloured glyph for a run state
  - `Renderer` gains a `st Style` field, set in `NewRenderer`

- [ ] **Step 1: Write the failing test**

Create `internal/provision/color_test.go`:

```go
package provision

import (
	"bytes"
	"strings"
	"testing"
)

// Piped output must stay clean: the probe's own docs promise it "degrades to
// sane plain text when piped to a file", and escape sequences in a log someone
// greps later are noise.
func TestStyleOffWhenNotATTY(t *testing.T) {
	s := NewStyle(&bytes.Buffer{})
	for _, got := range []string{s.Green("ok"), s.Red("bad"), s.Dim("x"), s.Mark(StateOK)} {
		if strings.Contains(got, "\033") {
			t.Errorf("escape sequence in non-tty output: %q", got)
		}
	}
	if s.Green("ok") != "ok" {
		t.Errorf("Green() = %q, want the bare text", s.Green("ok"))
	}
}

// NO_COLOR is honoured even on a terminal (https://no-color.org), as is the
// dumb terminal every CI runner claims to be.
func TestStyleRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if !colorDisabledByEnv() {
		t.Error("NO_COLOR set but the environment check allows colour")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if !colorDisabledByEnv() {
		t.Error("TERM=dumb but the environment check allows colour")
	}
	t.Setenv("TERM", "xterm-256color")
	if colorDisabledByEnv() {
		t.Error("plain terminal reported as colour-disabled")
	}
}

// Every run state needs a distinguishable glyph — the ledger is unreadable if
// skipped and blocked look the same.
func TestMarkIsDistinctPerState(t *testing.T) {
	s := Style{}
	seen := map[string]State{}
	for _, st := range []State{StateOK, StateAlready, StateSkipped, StateFailed, StateBlocked} {
		g := s.Mark(st)
		if g == "" {
			t.Errorf("%s has no glyph", st)
		}
		if prev, dup := seen[g]; dup && prev != StateOK && st != StateAlready {
			t.Errorf("%s and %s share glyph %q", prev, st, g)
		}
		seen[g] = st
	}
}
```

`StateOK` and `StateAlready` deliberately share `✓` — both mean the box is in
the wanted state, and the ledger's text column already distinguishes them.

- [ ] **Step 2: Run the tests, watch them fail**

```bash
go test ./internal/provision/ -run 'TestStyle|TestMark' -v
```

Expected: compile failure — `undefined: NewStyle`.

- [ ] **Step 3: Write the palette**

Create `internal/provision/color.go`:

```go
package provision

import (
	"io"
	"os"
)

// Style paints terminal output, and knows when not to.
//
// Colour is a convenience for a human watching a wizard work through a box; it
// is actively harmful in a log file, where escape sequences survive into
// whatever greps the file later. So it switches itself off for anything that is
// not a terminal, and honours NO_COLOR (https://no-color.org) even on one.
type Style struct{ on bool }

func NewStyle(w io.Writer) Style {
	f, ok := w.(*os.File)
	if !ok {
		return Style{}
	}
	return Style{on: colorEnabled(f)}
}

func colorEnabled(f *os.File) bool {
	return !colorDisabledByEnv() && f != nil && isTerminal(f)
}

// colorDisabledByEnv is split out so it can be tested without a terminal.
func colorDisabledByEnv() bool {
	return os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
}

func (s Style) paint(code, text string) string {
	if !s.on {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

func (s Style) Green(t string) string  { return s.paint("32", t) }
func (s Style) Red(t string) string    { return s.paint("31", t) }
func (s Style) Yellow(t string) string { return s.paint("33", t) }
func (s Style) Cyan(t string) string   { return s.paint("36", t) }
func (s Style) Dim(t string) string    { return s.paint("2", t) }
func (s Style) Bold(t string) string   { return s.paint("1", t) }

// Mark is the glyph for a run state. ok and already share ✓ on purpose: both
// mean the box is in the wanted state, and only the reason differs.
func (s Style) Mark(st State) string {
	switch st {
	case StateOK, StateAlready:
		return s.Green("✓")
	case StateFailed:
		return s.Red("✗")
	case StateSkipped:
		return s.Yellow("⊘")
	case StateBlocked:
		return s.Dim("⋯")
	}
	return " "
}
```

- [ ] **Step 4: Colour the existing renderer**

In `internal/provision/01-report.go`, add `st Style` to `Renderer`, set it in
`NewRenderer` (`st: NewStyle(w)`), and paint the marks in `Event`:

```go
	mark, detail := r.st.Green("✓"), ""
	switch ev.State {
	case "skip":
		mark, detail = r.st.Yellow("-"), ev.Note
	case "err":
		mark, detail = r.st.Red("✗"), ev.Note
	default:
		detail = summarize(ev.Check, p)
	}
```

Read the rest of `Event` and `Summary` and paint in the same spirit: check names
cyan, notes dim, the `⚠` findings yellow. Do not paint the remote command output
— that is the box talking, and its own colours must not be second-guessed.

- [ ] **Step 5: Run the tests**

```bash
go test ./... && go run ./cmd/stc bootstrap fiona -dry-run -no-bench -config swarm.yaml | cat -v | grep -c '\^\[' 
```

Expected: tests PASS; the `grep -c` prints `0` — piping through `cat` means no
tty, so no escapes.

- [ ] **Step 6: Commit**

```bash
git add internal/provision/color.go internal/provision/color_test.go internal/provision/01-report.go
git commit -m "report: colour + status glyphs, off when piped"
```

---

### Task 7: Wire the wizard to the runner

**Files:**
- Modify: `cmd/stc/bootstrap.go:20-197` (`cmdBootstrap`) and `:199-283` (`stageSyncthing`)
- Test: `cmd/stc/bootstrap_test.go` (append)

**Interfaces:**
- Consumes: `provision.Run`, `provision.Ledger`, `provision.SSHExecutor`, `provision.RunOpts` (Task 3); `provision.UpsertNode`, `provision.DiffNode` (Task 5); `provision.NewStyle` (Task 6).
- Produces: no new exported API; `cmd/stc/bootstrap.go` gains
  `printLedger(w io.Writer, st provision.Style, led provision.Ledger)` and
  `confirmDiff(in *bufio.Reader, changes []provision.FieldChange, yes bool) bool`.

- [ ] **Step 1: Replace the apply loop**

In `cmdBootstrap`, delete the loop at `cmd/stc/bootstrap.go:113-149` (apply,
count applied/skipped, `os.Exit(1)` on the first failure, re-probe, and the
`len(remaining) > 0 → return` gate) and use the runner:

```go
	st := provision.NewStyle(os.Stdout)
	ex := provision.SSHExecutor{S: ssh, Out: os.Stdout}

	led := provision.Run(ctx, ex, steps, provision.RunOpts{
		Confirm: func(s provision.Step) bool {
			fmt.Printf("  %s %s\n", st.Cyan("→"), s.Title)
			if s.Warn != "" {
				fmt.Printf("     %s %s\n", st.Yellow("⚠"), s.Warn)
			}
			return confirm(in, s, *yes)
		},
		Report: stepReporter(st),
	})

	printLedger(os.Stdout, st, led)
```

and add — `stepReporter` is shared with `stageSyncthing` in Step 3, so define it
once, at package scope:

```go
// stepReporter narrates a run as it happens. The ledger printed afterwards is
// the receipt; this is the live commentary, and both use the same glyphs so a
// line means the same thing wherever it appears.
func stepReporter(st provision.Style) func(provision.Result) {
	return func(r provision.Result) {
		switch r.State {
		case provision.StateAlready:
			fmt.Printf("  %s %s %s\n", st.Mark(r.State), r.Step.Title, st.Dim("(already done)"))
		case provision.StateOK:
			fmt.Printf("  %s %s\n", st.Mark(r.State), r.Step.Title)
		case provision.StateSkipped:
			fmt.Printf("  %s %s %s\n", st.Mark(r.State), r.Step.Title, st.Dim("(skipped)"))
		case provision.StateBlocked:
			fmt.Printf("  %s %s %s\n", st.Mark(r.State), r.Step.Title, st.Dim(r.Err.Error()))
		case provision.StateFailed:
			fmt.Printf("  %s %s\n     %s\n", st.Mark(r.State), r.Step.Title, st.Red(r.Err.Error()))
		}
	}
}
```

and:

```go
// printLedger is the run's receipt. A skipped or failed step is not a reason to
// hide everything else that worked — the wizard's old behaviour of stopping
// dead meant a run's only output was its first problem.
func printLedger(w io.Writer, st provision.Style, led provision.Ledger) {
	fmt.Fprintf(w, "\n  ── result ──\n\n")
	for _, r := range led {
		fmt.Fprintf(w, "  %s %-12s %s\n", st.Mark(r.State), r.State, r.Step.Title)
	}
	fmt.Fprintf(w, "\n  %d ok, %d already, %d skipped, %d failed, %d blocked\n",
		led.Count(provision.StateOK), led.Count(provision.StateAlready),
		led.Count(provision.StateSkipped), led.Count(provision.StateFailed),
		led.Count(provision.StateBlocked))
	if f := led.Failed(); len(f) > 0 {
		fmt.Fprintf(w, "\n  %s the box is the state — fix the cause and re-run; the probe picks up\n"+
			"    from wherever reality actually is, and finished steps cost nothing.\n", st.Yellow("⚠"))
	}
}
```

- [ ] **Step 2: Gate the syncthing stage on the drive, not on an empty plan**

Where the old code returned early, call the stage unconditionally and let it
report what is unmet:

```go
	if !*syncthing {
		fmt.Println("\n  (pass -syncthing to install syncthing and join the swarm)")
		return
	}

	// Re-probe: the hardening just changed the box, and the syncthing layout is
	// derived from what is mounted NOW.
	p2, err := provision.RunProbe(ctx, ssh, provision.ProbeOpts{}, nil)
	die(err)

	// Advisory, not blocking. Declining a step you do not want must never cost
	// you the install — that is exactly the deadlock this replaces: an SD card
	// wrongly offered as a data drive, declined, and syncthing never installed.
	// The one hard requirement is a mounted data drive, which layoutFor enforces.
	for _, r := range led {
		if r.State == provision.StateSkipped || r.State == provision.StateFailed || r.State == provision.StateBlocked {
			fmt.Printf("  %s %s did not complete — continuing to syncthing anyway\n",
				st.Yellow("⚠"), r.Step.Title)
		}
	}

	stageSyncthing(ctx, ssh, p2, in, *cfgPath, dest, *node, *yes)
```

- [ ] **Step 3: Run the syncthing stage through the runner too**

In `stageSyncthing`, replace its own apply loop (`cmd/stc/bootstrap.go:228-239`,
which returns on the first skip and `os.Exit(1)`s on the first failure) with the
same `provision.Run` call, then guard the join on the ledger:

```go
	st := provision.NewStyle(os.Stdout) // stageSyncthing has no style of its own yet

	led := provision.Run(ctx, provision.SSHExecutor{S: ssh, Out: os.Stdout}, steps, provision.RunOpts{
		Confirm: func(s provision.Step) bool { return confirm(in, s, yes) },
		Report:  stepReporter(st),
	})
	printLedger(os.Stdout, st, led)

	if len(led.Failed()) > 0 || !led.Satisfied("syncthing-service") {
		fmt.Printf("\n  %s syncthing is not running — not joining the swarm. Re-run when it is.\n", st.Red("✗"))
		return
	}
```

- [ ] **Step 4: Swap the append for the upsert**

Replace `die(provision.AppendNode(cfgPath, newNode))` at
`cmd/stc/bootstrap.go:266` with:

```go
	changes, exists, err := provision.DiffNode(cfgPath, newNode)
	die(err)
	if exists && len(changes) == 0 {
		fmt.Printf("  %s %s already in %s, unchanged\n", st.Mark(provision.StateAlready), nodeName, cfgPath)
	} else {
		if exists {
			fmt.Printf("\n  %s is already in %s and this run found different values:\n\n", nodeName, cfgPath)
			for _, c := range changes {
				old := c.Old
				if old == "" {
					old = "(unset)"
				}
				fmt.Printf("    %-7s %s → %s\n", c.Field, st.Dim(old), st.Green(c.New))
			}
			fmt.Println("\n  rewrite these values? (comments and other nodes are untouched)")
			if !confirmYN(in, yes) {
				fmt.Println("  skipped. The node is installed but swarm.yaml still describes the old box.")
				return
			}
		}
		die(provision.UpsertNode(cfgPath, newNode))
		fmt.Printf("  %s wrote %s to %s\n", st.Mark(provision.StateOK), nodeName, cfgPath)
	}
```

Delete the `AppendNode` wrapper left over from Task 5 if you kept one.

- [ ] **Step 5: Write the regression test**

Append to `cmd/stc/bootstrap_test.go` — read the existing file first and match
its style:

```go
// The wizard must never again gate the syncthing stage on "the hardening plan
// came back empty". A step the user declined, or one that was blocked, is not
// a reason to withhold the install they asked for.
func TestBootstrapDoesNotGateSyncthingOnAnEmptyPlan(t *testing.T) {
	src, err := os.ReadFile("bootstrap.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"len(remaining) > 0",
		"still outstanding",
	} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("bootstrap.go still gates on the plan being empty (%q)", forbidden)
		}
	}
}
```

- [ ] **Step 6: Build, vet and test**

```bash
go build ./... && go vet ./... && go test ./...
```

Expected: PASS. Then confirm the dry run is unchanged in shape:

```bash
go run ./cmd/stc bootstrap fiona \
  -syncthing -dry-run -no-bench \
  -config swarm.yaml
```

Expected: no `/mnt/bootfs` or `/mnt/rootfs` mount steps (Task 1), and the plan
lists `apt-get update` and `hd-idle` only.

- [ ] **Step 7: Commit**

```bash
git add cmd/stc/bootstrap.go cmd/stc/bootstrap_test.go
git commit -m "bootstrap: run the ledger, gate on the drive, upsert the node"
```

---

### Task 8: Provision fiona for real

The plan's actual purpose. Everything above is unproven until a Pi runs it.

**Files:** none — this task changes a box, and `swarm.yaml` (gitignored).

- [ ] **Step 1: Deploy**

```bash
just deploy
```

CLAUDE.md requires the :8888 dashboard to match the code after every task.

- [ ] **Step 2: Dry run against fiona**

```bash
./stc bootstrap fiona -syncthing \
  -dry-run -no-bench -config swarm.yaml
```

Expected: the plan contains `apt-get update` and `hd-idle` and nothing about
`/mnt/bootfs` or `/mnt/rootfs`. The syncthing plan targets `/srv/data`
(`--data=/srv/data/syncthing-db`, folders at `/srv/data/syncthing`).

- [ ] **Step 3: Real run**

```bash
./stc bootstrap fiona -syncthing \
  -config swarm.yaml
```

Answer the prompts. The `swarm.yaml` diff should offer `apikey`, `root:
/media/hdd/syncthing → /srv/data/syncthing`, and `mount: /media/hdd →
/srv/data`.

- [ ] **Step 4: Verify on the box**

```bash
ssh fiona 'syncthing --version; \
  systemctl is-active syncthing@pi; \
  grep -o "<address>[^<]*</address>" \
    ~/.config/syncthing/config.xml'
```

Expected: `v2.1.2`, `active`, and a GUI address of `100.86.131.51:8384` — never
`0.0.0.0`.

- [ ] **Step 5: Verify idempotence**

Run the exact same command again:

```bash
./stc bootstrap fiona -syncthing \
  -config swarm.yaml
```

Expected: every step reports `already`, zero commands run, and the `swarm.yaml`
step reports "already in swarm.yaml, unchanged". This is the property the whole
plan exists to produce — if anything reports `ok` instead of `already`, that
step's `Check` does not describe its end state, and it must be fixed before this
task is done.

- [ ] **Step 6: Confirm the dashboard sees it**

```bash
curl -s localhost:8888/api/nodes \
  | grep -o '"name":"[^"]*"'
```

Expected: fiona present. Then `just deploy` once more if you changed any code
while fixing checks.

- [ ] **Step 7: Commit any fixes**

```bash
git add -A
git commit -m "bootstrap: fixes from the fiona run"
```
