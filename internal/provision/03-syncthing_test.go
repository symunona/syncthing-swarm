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
