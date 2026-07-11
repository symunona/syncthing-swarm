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

	// 3. A firewall that cannot filter must never be "enabled". On rue's ARMv6
	// kernel the nf_tables backend is broken (`iptables -L` itself fails), and ufw
	// happily reported success while applying no rules at all — you'd believe you
	// had a firewall and have nothing. The step repairs the backend, then HARD
	// STOPS if iptables still cannot list a chain.
	joined := strings.Join(ufw.Cmds, "\n")
	if !strings.Contains(joined, "iptables-legacy") {
		t.Error("no fallback to the legacy iptables backend; ufw silently applies no rules when nf_tables is broken")
	}
	verifyAt, ufwFirstAt := -1, -1
	for i, c := range ufw.Cmds {
		if strings.HasPrefix(c, "iptables -L -n >/dev/null") && !strings.Contains(c, "||") {
			verifyAt = i
		}
		if strings.HasPrefix(c, "ufw ") && ufwFirstAt < 0 {
			ufwFirstAt = i
		}
	}
	if verifyAt < 0 {
		t.Fatal("ufw step never verifies that iptables actually works before enabling")
	}
	if verifyAt > ufwFirstAt {
		t.Error("iptables is verified AFTER ufw starts configuring — a broken backend would slip through")
	}

	// 4. The commands are chained with && so a FAILED allow rule aborts before
	// the enable. With `;` the firewall would come up with no ssh rule.
	script := buildScript(*ufw)
	if strings.Count(script, "&&") < len(ufw.Cmds)-1 {
		t.Errorf("ufw commands are not fully &&-chained; a failed allow rule could still "+
			"let `ufw enable` run and lock you out:\n%s", script)
	}
	if !strings.Contains(script, "set -e") {
		t.Error("script does not set -e")
	}
	// A debconf prompt on a headless box with nobody to answer it hangs forever.
	if !strings.Contains(script, "DEBIAN_FRONTEND=noninteractive") {
		t.Error("apt runs without DEBIAN_FRONTEND=noninteractive; a prompt would hang the step")
	}
}

// nofail lives in fstab and NEVER appears in the live mount options — systemd
// consumes it at mount time. Checking /proc/mounts for it reported "missing
// nofail" on a drive whose fstab line plainly had it.
//
// noatime is the opposite: it IS a live option, and an fstab edit does not apply
// to an already-mounted filesystem. Until the drive is remounted, every file READ
// writes an access timestamp — which keeps waking a spinning disk and quietly
// undoes the whole spindown policy.
func TestPlanFstabOptionsVsLiveMount(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	d := p.Drives()[0]

	// fstab is fully correct; the drive is still mounted the old way.
	p.Fstab = append(p.Fstab,
		"UUID="+d.UUID+"\t/mnt/hdd\text4\tdefaults,nofail,noatime,x-systemd.device-timeout=10s\t0\t0")
	for i := range p.Mounts {
		if p.Mounts[i].Target == "/mnt/hdd" {
			p.Mounts[i].Options = "rw,relatime" // no noatime yet
		}
	}

	steps, done := Plan(p, "symunona")

	if stepByID(steps, "fstab") != nil {
		t.Error("planned an fstab step for a drive already correctly in fstab")
	}
	if strings.Contains(strings.Join(done, " "), "missing nofail") {
		t.Error("reported nofail missing: it is an fstab-only option and never shows in /proc/mounts")
	}
	if stepByID(steps, "remount") == nil {
		t.Error("fstab says noatime but the live mount does not have it — a remount is needed, " +
			"or reads keep waking the disk until the next reboot")
	}

	// Once remounted, there is nothing left to do.
	for i := range p.Mounts {
		if p.Mounts[i].Target == "/mnt/hdd" {
			p.Mounts[i].Options = "rw,noatime"
		}
	}
	steps, _ = Plan(p, "symunona")
	if stepByID(steps, "remount") != nil {
		t.Error("still planning a remount on a drive already mounted noatime")
	}
}

// "Installed and enabled" is not "running". fail2ban installs, enables itself,
// and then CRASHES on Debian 12 — bookworm ships no rsyslog, so its sshd jail
// finds no /var/log/auth.log. The wizard called that done, and the box had no
// brute-force protection at all.
func TestPlanFail2banCrashIsNotDone(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")

	// fresh install must configure the journal backend up front
	fresh, _ := Plan(p, "symunona")
	f2b := stepByID(fresh, "fail2ban")
	if f2b == nil {
		t.Fatal("no fail2ban step")
	}
	joined := strings.Join(f2b.Cmds, "\n")
	if !strings.Contains(joined, "backend = systemd") {
		t.Error("fail2ban installed without the journal backend; it will crash on Debian 12")
	}
	if !strings.Contains(joined, "is-active --quiet fail2ban") {
		t.Error("fail2ban step never verifies the service actually RUNS after starting it")
	}

	// installed + enabled + failed = broken, and must be repaired, not called done
	broken := parseFixture(t, "rue-pi1b.ndjson")
	broken.Security.Fail2ban = Tool{Present: true, Enabled: "enabled", Active: "failed"}
	steps, done := Plan(broken, "symunona")
	if stepByID(steps, "fail2ban-repair") == nil {
		t.Error("a FAILED fail2ban was not scheduled for repair — installed is not running")
	}
	if strings.Contains(strings.Join(done, " "), "fail2ban present") {
		t.Error("a crashed fail2ban was reported as done")
	}

	// a genuinely running one is done
	okp := parseFixture(t, "rue-pi1b.ndjson")
	okp.Security.Fail2ban = Tool{Present: true, Enabled: "enabled", Active: "active"}
	steps, _ = Plan(okp, "symunona")
	if stepByID(steps, "fail2ban") != nil || stepByID(steps, "fail2ban-repair") != nil {
		t.Error("planned fail2ban work on a box where it is already running")
	}
}

// rue AFTER stage 2 actually ran. This fixture is the real post-provision probe,
// and it is not clean: fail2ban is installed and enabled but FAILED. A wizard
// that reports "done" here leaves a box with no brute-force protection while
// telling you it is protected.
func TestPlanProvisionedRueStillRepairsFail2ban(t *testing.T) {
	p := parseFixture(t, "rue-provisioned.ndjson")

	if !p.Security.Fail2ban.Broken() {
		t.Fatalf("fixture should show fail2ban failed; got %+v", p.Security.Fail2ban)
	}
	steps, done := Plan(p, "symunona")

	if stepByID(steps, "fail2ban-repair") == nil {
		t.Error("crashed fail2ban not scheduled for repair")
	}
	// everything else on this box really IS done
	for _, id := range []string{"inotify", "fstab", "hd-idle", "ufw", "unattended-upgrades"} {
		if stepByID(steps, id) != nil {
			t.Errorf("re-planning %q on a box where it is already applied", id)
		}
	}
	joined := strings.Join(done, " ")
	for _, want := range []string{"inotify", "nofail+noatime", "hd-idle", "ufw"} {
		if !strings.Contains(joined, want) {
			t.Errorf("done list does not mention %q: %v", want, done)
		}
	}

	// ufw.service reads "inactive" — that is NORMAL for its oneshot unit, which
	// applies the rules and exits. It must not be mistaken for a crash.
	if p.Security.UFW.Broken() {
		t.Error("ufw reported broken because its oneshot unit is inactive; only `failed` is broken")
	}
}

// Every apt command must WAIT for the dpkg lock rather than die on it.
//
// This bit us on the real box: our own plan enables unattended-upgrades, which
// immediately starts its own apt run, which held the lock and killed the very
// next step. Any box that auto-updates can do this at any moment.
func TestPlanAptWaitsForTheDpkgLock(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")

	for _, s := range steps {
		for _, c := range s.Cmds {
			if !strings.Contains(c, "apt-get") {
				continue
			}
			if !strings.Contains(c, "DPkg::Lock::Timeout") {
				t.Errorf("step %q runs apt without a lock timeout — a concurrent "+
					"unattended-upgrades run will kill it:\n  %s", s.ID, c)
			}
		}
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
	// Precisely: nothing may touch the ssh ACCESS PATH. Configuring fail2ban's
	// [sshd] jail is fine and necessary; editing sshd_config or bouncing the ssh
	// daemon is not, because a mistake there is unrecoverable on a headless box.
	weakSteps, _ := Plan(weak, "symunona")
	for _, s := range weakSteps {
		for _, c := range s.Cmds {
			for _, forbidden := range []string{"sshd_config", "restart ssh", "restart sshd", "reload ssh"} {
				if strings.Contains(c, forbidden) {
					t.Errorf("step %q touches the ssh access path (%q): findings are reported, never applied",
						s.ID, forbidden)
				}
			}
		}
	}
}
