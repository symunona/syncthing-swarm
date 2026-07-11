package provision

import (
	"strings"
	"testing"
)

func stepByID(steps []Step, id string) *Step {
	for i := range steps {
		if steps[i].ID == id {
			return &steps[i]
		}
	}
	return nil
}

// The plan for rue, derived from its real probe: nothing on this box is set up.
func TestPlanRue(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")

	for _, want := range []string{"apt-update", "inotify", "fstab", "hd-idle", "ufw", "fail2ban", "unattended-upgrades"} {
		if stepByID(steps, want) == nil {
			t.Errorf("plan is missing step %q", want)
		}
	}

	// apt-get update must come before anything that installs.
	firstInstall := -1
	for i, s := range steps {
		if strings.Contains(strings.Join(s.Cmds, " "), "apt-get install") {
			firstInstall = i
			break
		}
	}
	if firstInstall == 0 || steps[0].ID != "apt-update" {
		t.Errorf("apt-get update must be the first step; got %q first", steps[0].ID)
	}

	// The inotify limit must clear the actual file count, not a magic number.
	cap := p.CapacityAt("/mnt/hdd")
	if got := wantedWatches(p); int64(got) < cap.InodesUsed {
		t.Errorf("wantedWatches = %d, below the %d files on the drive", got, cap.InodesUsed)
	}

	// hd-idle spins down DISKS, not partitions: /dev/sda1 -> /dev/sda.
	hd := stepByID(steps, "hd-idle")
	joined := strings.Join(hd.Cmds, " ")
	if !strings.Contains(joined, "/dev/sda ") && !strings.Contains(joined, "/dev/sda\"") {
		t.Errorf("hd-idle should target the disk /dev/sda, got: %s", joined)
	}
	if strings.Contains(joined, "/dev/sda1") {
		t.Error("hd-idle targets a PARTITION (/dev/sda1); it spins down disks")
	}

	// fstab must be non-blocking. nofail ALONE is not enough: systemd still waits
	// its default 90s for a missing device, which hangs the boot the user
	// explicitly asked us not to hang.
	fs := stepByID(steps, "fstab")
	fstabCmds := strings.Join(fs.Cmds, " ")
	for _, want := range []string{"nofail", "noatime", "x-systemd.device-timeout=10s"} {
		if !strings.Contains(fstabCmds, want) {
			t.Errorf("fstab entry missing %q — a missing drive must never stall boot", want)
		}
	}
	if !strings.Contains(fstabCmds, p.Drives()[0].UUID) {
		t.Error("fstab entry must be by UUID, not by device node (/dev/sda1 is not stable)")
	}
}

// ufw is the one step that can sever your own access. Three properties must hold,
// and they are all structural — not "we wrote the commands carefully".
func TestPlanUFWCannotLockYouOut(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")
	ufw := stepByID(steps, "ufw")
	if ufw == nil {
		t.Fatal("no ufw step")
	}

	// 1. It demands a typed word, not a bare y.
	if ufw.Confirm != "ufw" {
		t.Errorf("Confirm = %q; enabling a firewall over ssh must not be one keystroke away", ufw.Confirm)
	}

	// 2. The ssh allow rule uses the LIVE sshd port and comes BEFORE enable.
	allowAt, enableAt := -1, -1
	for i, c := range ufw.Cmds {
		if strings.Contains(c, "allow") && strings.Contains(c, "/tcp") {
			allowAt = i
		}
		if strings.Contains(c, "enable") {
			enableAt = i
		}
	}
	if allowAt < 0 || enableAt < 0 {
		t.Fatalf("ufw step lacks an allow rule or an enable: %v", ufw.Cmds)
	}
	if allowAt > enableAt {
		t.Fatal("ufw enable runs BEFORE the ssh allow rule — this locks you out")
	}
	if !strings.Contains(ufw.Cmds[allowAt], "22/tcp") {
		t.Errorf("ssh allow rule = %q; must use the port read from sshd, not a guess", ufw.Cmds[allowAt])
	}

	// 3. The commands are chained with && so a FAILED allow rule aborts before
	// the enable. With `;` the firewall would come up with no ssh rule.
	script := buildScript(*ufw)
	if strings.Count(script, "&&") < len(ufw.Cmds)-1 {
		t.Errorf("ufw commands are not fully &&-chained; a failed allow rule could still "+
			"let `ufw enable` run and lock you out:\n%s", script)
	}
	if !strings.HasPrefix(script, "set -e") {
		t.Error("script does not set -e")
	}
}

// Flash storage must not get the spinning-disk policy: hd-idle on an SSD is dead
// weight, and there is nothing to spin down.
func TestPlanNoSpindownOnFlash(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	// same box, but the drive is an SSD
	for i := range p.Disks {
		p.Disks[i].Rota = false
	}
	steps, done := Plan(p, "symunona")
	if s := stepByID(steps, "hd-idle"); s != nil {
		t.Errorf("planned hd-idle for a flash drive: %v", s.Cmds)
	}
	if !strings.Contains(strings.Join(done, " "), "flash") {
		t.Error("should report that spindown was skipped because the drive is flash")
	}
}

// A box that is already provisioned yields no steps. This is what makes re-running
// the wizard a safe "check my node" tool rather than a thing you do once.
func TestPlanIdempotentOnProvisionedBox(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	p.Inotify.MaxUserWatches = 524288
	p.Security.UFW = Tool{Present: true, Enabled: "enabled"}
	p.Security.Fail2ban = Tool{Present: true, Enabled: "enabled"}
	p.Security.UnattendedUpgrades = Tool{Present: true, Enabled: "enabled"}
	p.Spindown = Spindown{Present: true, Enabled: "enabled"}
	d := p.Drives()[0]
	p.Fstab = append(p.Fstab, "UUID="+d.UUID+"\t/mnt/hdd\text4\tdefaults,nofail,noatime\t0\t0")
	for i := range p.Mounts {
		if p.Mounts[i].Target == "/mnt/hdd" {
			p.Mounts[i].Options = "rw,noatime,nofail"
		}
	}

	steps, done := Plan(p, "symunona")
	if len(steps) != 0 {
		var ids []string
		for _, s := range steps {
			ids = append(ids, s.ID)
		}
		t.Errorf("provisioned box still yields steps: %v", ids)
	}
	if len(done) == 0 {
		t.Error("no `done` items reported for a fully provisioned box")
	}
}

// The wizard reports things it must never touch. Editing sshd_config is the one
// change that can leave a headless box unreachable with no way back in.
func TestFindingsNeverTouchSSHD(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")
	for _, s := range steps {
		joined := strings.Join(s.Cmds, " ")
		if strings.Contains(joined, "sshd_config") || strings.Contains(joined, "restart ssh") {
			t.Errorf("step %q edits the ssh access path: %s", s.ID, joined)
		}
	}
	// rue already has PasswordAuthentication no, so nothing to report there. On a
	// box that does accept passwords, it must be REPORTED — with the command —
	// and still never applied.
	weak := parseFixture(t, "rue-pi1b.ndjson")
	weak.Security.PasswordAuth = "yes"
	weak.Security.PermitRootLogin = "yes"
	f := strings.Join(Findings(weak), " ")
	if !strings.Contains(f, "PasswordAuthentication") {
		t.Error("password auth is on, but it is not reported as a finding")
	}
	if !strings.Contains(f, "PermitRootLogin") {
		t.Error("root login is permitted, but it is not reported as a finding")
	}
	weakSteps, _ := Plan(weak, "symunona")
	for _, s := range weakSteps {
		if strings.Contains(strings.Join(s.Cmds, " "), "sshd") {
			t.Errorf("step %q tries to fix sshd; findings are reported, never applied", s.ID)
		}
	}
}
