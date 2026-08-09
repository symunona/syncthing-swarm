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

// PlanSyncthing used to short-circuit to (nil, nil) for a node that LOOKED
// fully configured (Present && enabled && active) before a single Check ran.
// That coarse gate raced the per-step Checks below it: a node that installed
// and started syncthing fine but whose syncthing-gui step failed — GUI still
// on 127.0.0.1 — passed the coarse gate on the next run (the SERVICE really
// is present/enabled/active; only the GUI bind is wrong) and PlanSyncthing
// returned an empty step list. An empty ledger then reads as "nothing to
// join" rather than "one step still needs repair" — see stageSyncthing and
// Ledger.Satisfied's doc comment for why an absent ID is satisfied by
// default. The fix deletes the coarse gate; this pins that all three steps
// are still returned (and still checkable) for a present-and-active node —
// they are simply expected to each report `already` once actually run
// against a box in that state.
func TestPlanSyncthingStillReturnsAllStepsWhenAlreadyRunning(t *testing.T) {
	p := syncProbe()
	p.Syncthing.Present = true
	p.Syncthing.Enabled = "enabled"
	p.Syncthing.Active = "active"

	steps, err := PlanSyncthing(p, syncLayout())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"syncthing-install", "syncthing-service", "syncthing-gui"} {
		if stepByID(steps, id) == nil {
			t.Errorf("PlanSyncthing on a present+enabled+active node dropped step %q — "+
				"a half-configured node (e.g. gui step previously failed) could no longer "+
				"be repaired by a re-run", id)
		}
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

// The service Check must inspect the running process's cmdline, not just the file.
// A deployment interrupted between writing override.conf and daemon-reload+restart
// would leave the unit active but still using the old data directory.
func TestSyncthingServiceCheckInspectsRunningProcess(t *testing.T) {
	steps, _ := PlanSyncthing(syncProbe(), syncLayout())
	s := stepByID(steps, "syncthing-service")
	if s == nil {
		t.Fatal("no syncthing-service step")
	}
	joined := strings.Join(s.Check, " ")
	if !strings.Contains(joined, "/proc") || !strings.Contains(joined, "MainPID") {
		t.Errorf("service check does not inspect running process: %v", s.Check)
	}
}

// The GUI Check must verify the live listening socket is on the tailnet address,
// not just that config.xml says so. A sed interrupted before systemctl restart
// would leave the socket on 127.0.0.1.
func TestSyncthingGUICheckInspectsLiveSocket(t *testing.T) {
	l := syncLayout()
	steps, _ := PlanSyncthing(syncProbe(), l)
	s := stepByID(steps, "syncthing-gui")
	if s == nil {
		t.Fatal("no syncthing-gui step")
	}
	joined := strings.Join(s.Check, " ")
	if !strings.Contains(joined, "ss") || !strings.Contains(joined, l.TailnetIP+":8384") {
		t.Errorf("gui check does not inspect live socket: %v", s.Check)
	}
}

// The gui step must WAIT for syncthing's HTTP listener before it finishes.
//
// Found on the first real fiona run. `systemctl start` returns, `is-active`
// goes true, and the step ends — but syncthing opens its GUI socket a second or
// two LATER. The step's own post-Check then asked `ss` whether the tailnet
// address was bound, got "not yet", and recorded a step that had in fact done
// everything right as FAILED. Measured on fiona: is-active true immediately,
// socket open after 2s.
//
// This is the same race the fail2ban step and HarvestIdentity already guard
// against with explicit readiness loops; the socket predicate was added without
// one. A Check may only assert what the step has already waited for.
func TestSyncthingGUIWaitsForTheSocketBeforeItEnds(t *testing.T) {
	l := syncLayout()
	steps, _ := PlanSyncthing(syncProbe(), l)
	s := stepByID(steps, "syncthing-gui")
	if s == nil {
		t.Fatal("no syncthing-gui step")
	}

	startAt, waitAt := -1, -1
	for i, c := range s.Cmds {
		if strings.HasPrefix(c, "systemctl start ") {
			startAt = i
		}
		// a readiness loop: polls the socket and breaks out
		if strings.Contains(c, "ss -tlnH") && strings.Contains(c, "sleep") && strings.Contains(c, l.TailnetIP) {
			waitAt = i
		}
	}
	if startAt < 0 {
		t.Fatal("gui step never starts the unit")
	}
	if waitAt < 0 {
		t.Fatal("gui step does not wait for the GUI socket — its own post-Check will race it")
	}
	if waitAt < startAt {
		t.Errorf("socket wait at %d comes BEFORE the start at %d", waitAt, startAt)
	}
}

// The pinned release is a FLOOR, not an exact version.
//
// Tarball installs deliberately leave syncthing's auto-upgrade ENABLED, so a
// healthy node drifts AHEAD of SyncthingRelease on its own — fiona was on
// 2.1.2 with 2.1.3 already out, and would have upgraded itself within 12h.
// An install Check that demands equality would then read false on a node that
// is correctly current, and the wizard would reinstall the older pin over it:
// a downgrade on every run, which auto-upgrade undoes, forever.
func TestSyncthingInstallCheckTreatsTheReleaseAsAFloor(t *testing.T) {
	steps, _ := PlanSyncthing(syncProbe(), syncLayout())
	s := stepByID(steps, "syncthing-install")
	if s == nil {
		t.Fatal("no syncthing-install step")
	}
	joined := strings.Join(s.Check, " ")
	if !strings.Contains(joined, "sort -V") {
		t.Errorf("install check does not compare versions, so a newer syncthing reads as wrong: %v", s.Check)
	}
	if strings.Contains(joined, "grep -qE 'v"+SyncthingRelease) {
		t.Errorf("install check still demands an exact version match: %v", s.Check)
	}
}
