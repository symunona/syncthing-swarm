package provision

import (
	"strings"
	"testing"
)

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

// Every service step must (a) clear a poisoned systemd start limit before
// starting, and (b) verify the service actually RUNS afterwards.
//
// Both come from real failures. hd-idle's package postinst starts the service
// with its DEFAULT config — before ours exists — so it fails, retries, and burns
// systemd's start limit; our `enable --now` was then refused with
// "start-limit-hit" and the real error was buried. And fail2ban installed,
// enabled, and crashed while the wizard reported success.
func TestPlanServiceStepsResetAndVerify(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	p.Power = Power{Throttled: "0x0", MaxUsbCurrent: "max_usb_current=1"}
	steps, _ := Plan(p, "symunona")

	for _, id := range []string{"hd-idle", "fail2ban"} {
		s := stepByID(steps, id)
		if s == nil {
			t.Fatalf("no %q step", id)
		}
		joined := strings.Join(s.Cmds, "\n")
		if !strings.Contains(joined, "is-active --quiet "+id) {
			t.Errorf("%s never verifies the service is actually running: installed is not running", id)
		}
	}

	hd := stepByID(steps, "hd-idle")
	if !strings.Contains(strings.Join(hd.Cmds, "\n"), "reset-failed hd-idle") {
		t.Error("hd-idle does not clear the start limit its own postinst poisoned; " +
			"systemd will refuse to start it with 'start-limit-hit'")
	}
}

// A USB disk on a headless box does not auto-mount — there is no desktop session
// running udisks. So the obvious flow ("plug the drive into the Pi, run the
// wizard") produces a disk that is plugged in, spinning, and invisible: the
// wizard said "no external drive" while the disk sat right there, and syncthing
// folders would have landed on the SD card.
func TestPlanMountsAnAttachedButUnmountedDrive(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	// same box, but the disk is attached and NOT mounted (papi's real state)
	for i := range p.Disks {
		for j := range p.Disks[i].Children {
			if p.Disks[i].Children[j].Mountpoint == "/mnt/hdd" {
				p.Disks[i].Children[j].Mountpoint = ""
			}
		}
	}
	p.Power = Power{Throttled: "0x0", MaxUsbCurrent: "max_usb_current=1"}

	if len(p.Drives()) != 0 {
		t.Fatal("fixture should have no MOUNTED data drive")
	}
	un := p.UnmountedDrives()
	if len(un) != 1 {
		t.Fatalf("UnmountedDrives() = %d, want the attached disk", len(un))
	}
	if un[0].UUID == "" {
		t.Error("unmounted drive has no UUID — the fstab entry is written from it")
	}

	steps, _ := Plan(p, "symunona")
	m := stepByID(steps, "mount")
	if m == nil {
		t.Fatal("a plugged-in, unmounted disk was not scheduled to be mounted; " +
			"the wizard would report 'no drive' and put folders on the SD card")
	}
	joined := strings.Join(m.Cmds, " ")
	for _, want := range []string{un[0].UUID, "nofail", "x-systemd.device-timeout=10s", "mkdir -p"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mount step missing %q: %s", want, joined)
		}
	}

	// The boot media must never be offered as a mount candidate, even though the
	// SD card reports hotplug=true on a Pi.
	for _, u := range un {
		if strings.Contains(u.Device, "mmcblk") {
			t.Errorf("offered to mount the SD card: %s", u.Device)
		}
	}
}

func TestSuggestMountpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Drive
		want string
	}{
		{"labelled", Drive{Label: "Backup", Rotational: true}, "/mnt/backup"},
		{"spinning, no label", Drive{Rotational: true}, "/mnt/hdd"},
		{"flash, no label", Drive{Rotational: false}, "/mnt/ssd"},
	} {
		if got := SuggestMountpoint(tc.d); got != tc.want {
			t.Errorf("%s: SuggestMountpoint = %q, want %q", tc.name, got, tc.want)
		}
	}
}

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

// The rule that killed rue.
//
// rue was a Pi 1 B+ with a bus-powered 2.5" HDD. The board caps its ENTIRE USB
// bus at 600mA; the disk spikes ~1A on spin-up. We enabled hd-idle, which turned
// one spin-up at boot into a spin-up every time the disk was touched. The board
// browned out, corrupted its SD card mid-write (cmdline.txt became 102 bytes of
// NUL), and never booted again.
//
// The wizard must now: raise the USB limit, and make spindown on such a board a
// typed confirmation rather than a keystroke.
func TestPlanWeakUSBPowerBoard(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson") // Pi 1 B+, rotational USB disk
	p.Power = Power{Throttled: "0x0"}       // no max_usb_current set

	if !p.WeakUSBPower() {
		t.Fatalf("Pi 1 B+ with no max_usb_current should be flagged weak; model=%q", p.Box.Model)
	}
	if !p.SpinningUSBDisk() {
		t.Fatal("rue has a rotational USB disk")
	}

	steps, _ := Plan(p, "symunona")

	usb := stepByID(steps, "usb-power")
	if usb == nil {
		t.Fatal("no max_usb_current step: the board cannot feed its disk and we did nothing")
	}
	if !strings.Contains(strings.Join(usb.Cmds, " "), "max_usb_current=1") {
		t.Error("usb-power step does not set max_usb_current=1")
	}

	hd := stepByID(steps, "hd-idle")
	if hd == nil {
		t.Fatal("no hd-idle step")
	}
	if hd.Confirm == "" {
		t.Error("spindown on an underpowered board is one keystroke away — it must be typed out; " +
			"this is what destroyed rue")
	}
	if !strings.Contains(hd.Warn, "600mA") {
		t.Errorf("hd-idle warning does not explain the power limit: %q", hd.Warn)
	}

	// A board that has ALREADY browned out must say so, loudly, and in the findings.
	brown := parseFixture(t, "rue-pi1b.ndjson")
	brown.Power = Power{Throttled: "0x50005"} // bit 0 and bit 16 set
	now, ever := brown.Power.UnderVoltage()
	if !now || !ever {
		t.Fatalf("0x50005 should decode as under-voltage now AND since boot; got now=%v ever=%v", now, ever)
	}
	if f := strings.Join(Findings(brown), " "); !strings.Contains(f, "under-voltage") {
		t.Error("a board that has browned out does not report it as a finding")
	}
}

// A Pi 3+ provides 1.2A by default and ignores max_usb_current. Do not nag about
// a limit that does not exist, and do not gate its spindown behind a typed word.
func TestPlanModernPiIsNotWeak(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	p.Box.Model = "Raspberry Pi 3 Model B Plus Rev 1.3"
	p.Power = Power{Throttled: "0x0"}

	if p.WeakUSBPower() {
		t.Error("Pi 3 B+ flagged as weak: it supplies 1.2A by default and has no such knob")
	}
	steps, _ := Plan(p, "symunona")
	if stepByID(steps, "usb-power") != nil {
		t.Error("planned max_usb_current on a Pi 3, where the setting does nothing")
	}
	if hd := stepByID(steps, "hd-idle"); hd != nil && hd.Confirm != "" {
		t.Error("gated spindown behind a typed word on a board that can actually feed its disk")
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
	p.Security.UFW = UFWStatus{Tool: Tool{Present: true, Enabled: "enabled"}, FirewallUp: true}
	p.Security.Fail2ban = Tool{Present: true, Enabled: "enabled"}
	p.Security.UnattendedUpgrades = Tool{Present: true, Enabled: "enabled"}
	p.Spindown = Spindown{Present: true, Enabled: "enabled"}
	p.Power = Power{Throttled: "0x0", MaxUsbCurrent: "max_usb_current=1"}
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

// Task 1 stopped treating an already-fstabbed partition as a drive awaiting
// adoption, which is right — but it must not make the drive vanish. A disk that
// is plugged in, configured, and simply did not mount this boot is the single
// most common way one of these boxes breaks, and the wizard used to answer it
// with "no external drive attached — attach the drive first".
func TestPlanOffersToMountAConfiguredButUnmountedDrive(t *testing.T) {
	p := &Probe{
		Disks: []BlockDevice{{
			Name: "sdb", Path: "/dev/sdb", Type: "disk", Tran: "usb", Rota: true,
			Children: []BlockDevice{
				{Name: "sdb1", Path: "/dev/sdb1", Type: "part", FSType: "ext4", UUID: "aaaa-bbbb"},
			},
		}},
		Fstab: []string{"UUID=aaaa-bbbb\t/srv/data\text4\tdefaults,nofail,noatime\t0\t0"},
	}

	got := p.ConfiguredUnmounted()
	if len(got) != 1 || got[0].Mountpoint != "/srv/data" {
		t.Fatalf("ConfiguredUnmounted() = %+v, want one drive at /srv/data", got)
	}

	steps, _ := Plan(p, "pi")
	s := stepByID(steps, "mount-configured")
	if s == nil {
		t.Fatal("no mount-configured step for a drive that is in fstab but unmounted")
	}
	joined := strings.Join(s.Cmds, " ")
	if strings.Contains(joined, "/etc/fstab") {
		t.Errorf("mount-configured must not edit fstab — the entry is already there: %v", s.Cmds)
	}
	if !strings.Contains(joined, "mount /srv/data") {
		t.Errorf("mount-configured does not mount the drive: %v", s.Cmds)
	}
}

// A mounted drive is not "configured but unmounted" — it must not produce a
// second, pointless mount step.
func TestConfiguredUnmountedIgnoresMountedDrives(t *testing.T) {
	p := &Probe{
		Disks: []BlockDevice{{
			Name: "sdb", Path: "/dev/sdb", Type: "disk", Tran: "usb",
			Children: []BlockDevice{
				{Name: "sdb1", Path: "/dev/sdb1", Type: "part", FSType: "ext4",
					UUID: "aaaa-bbbb", Mountpoint: "/srv/data"},
			},
		}},
		Fstab: []string{"UUID=aaaa-bbbb\t/srv/data\text4\tdefaults\t0\t0"},
	}
	if got := p.ConfiguredUnmounted(); len(got) != 0 {
		t.Errorf("ConfiguredUnmounted() = %+v on a mounted drive", got)
	}
}

// apt sets an index file's mtime from the server's Last-Modified header, not
// from when the download happened, and a 304 Not Modified does not touch the
// file at all. Debian bookworm's main Packages index is only regenerated at
// point releases, months apart — so on a real box, EVERY successful
// `apt-get update` still leaves every index file's mtime older than "-1 day".
// The old Check (`find … -name '*Packages*' -newermt '-1 day'`) was therefore
// false after every real update, the step recorded `failed`, and everything
// with `Needs: apt-update` (hd-idle, ufw, fail2ban, unattended-upgrades) was
// blocked on every single run. The fix gives apt-update its own stamp file
// instead of trying to read apt's mind about index mtimes.
func TestAptUpdateChecksItsOwnStampNotPackageIndexMtime(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")
	au := stepByID(steps, "apt-update")
	if au == nil {
		t.Fatal("no apt-update step")
	}
	joined := strings.Join(au.Check, " ")
	if strings.Contains(joined, "Packages") {
		t.Errorf("apt-update Check still tests package index mtimes, which apt does not "+
			"reliably update on a successful run: %v", au.Check)
	}
	if !strings.Contains(joined, ".stc-update-stamp") {
		t.Errorf("apt-update Check does not reference its own stamp file: %v", au.Check)
	}
	cmds := strings.Join(au.Cmds, " ")
	if !strings.Contains(cmds, "touch") || !strings.Contains(cmds, ".stc-update-stamp") {
		t.Errorf("apt-update Cmds never write the stamp the Check looks for: %v", au.Cmds)
	}
}

// ufw.service is Type=oneshot + RemainAfterExit=yes, and apt's postinst
// ENABLES AND STARTS it the instant the package is unpacked — entirely
// independent of whether anyone ever ran `ufw enable`. So on a box where
// nobody ever enabled the firewall, Present, Enabled=="enabled" AND
// Active=="active" can ALL read true: those are systemd's account of the
// unit, not ufw's account of itself. An earlier version of this fix gated
// on Enabled instead of Present, which reproduced the exact same bug one
// layer up — apt's postinst enables the unit too, so that gate was ALSO
// satisfied by a box with no firewall. FirewallUp (read from
// /etc/ufw/ufw.conf's own ENABLED= flag, set only by `ufw enable`/`ufw
// disable`) is the only field this drives through.
func TestPlanInstalledButDisabledUFWStillProducesTheStep(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	// installed, and apt's postinst both enabled AND started the oneshot
	// unit — but `ufw enable` itself was never run, so the actual firewall
	// (ufw.conf's ENABLED flag) is off.
	p.Security.UFW = UFWStatus{
		Tool:       Tool{Present: true, Enabled: "enabled", Active: "active"},
		FirewallUp: false,
	}

	steps, done := Plan(p, "symunona")
	ufw := stepByID(steps, "ufw")
	if ufw == nil {
		t.Fatal("an installed-but-disabled ufw produced no step — a box with no firewall " +
			"would be reported as finished")
	}
	if strings.Contains(strings.Join(done, " "), "ufw present") {
		t.Error("an installed-but-disabled ufw was reported under `done`")
	}
	// And the Check itself must read the real flag, not systemd's oneshot state.
	joined := strings.Join(ufw.Check, " ")
	if strings.Contains(joined, "is-active") {
		t.Error("ufw Check still tests is-active, which the oneshot unit satisfies " +
			"whether or not `ufw enable` ever ran")
	}
	if !strings.Contains(joined, "ufw.conf") || !strings.Contains(joined, "ENABLED=yes") {
		t.Errorf("ufw Check does not read /etc/ufw/ufw.conf's real ENABLED flag: %v", ufw.Check)
	}
}

// is-active alone is satisfied by hd-idle watching the WRONG disk — a stale
// /etc/default/hd-idle left behind by a drive that has since been replaced,
// or a restart that raced a leftover process. The Check must also confirm
// the config it wrote is the one actually in effect, the same way the
// syncthing-service Check inspects /proc/<pid>/cmdline instead of trusting
// is-active alone.
func TestHdIdleCheckVerifiesTheConfiguredDiskNotJustIsActive(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")
	hd := stepByID(steps, "hd-idle")
	if hd == nil {
		t.Fatal("no hd-idle step")
	}
	joined := strings.Join(hd.Check, " ")
	if !strings.Contains(joined, "is-active") {
		t.Error("hd-idle Check dropped the is-active test")
	}
	if !strings.Contains(joined, "/etc/default/hd-idle") || !strings.Contains(joined, "-a /dev/sda") {
		t.Errorf("hd-idle Check does not verify the -a <disk> it wrote to /etc/default/hd-idle: %v", hd.Check)
	}
}

// The step runs `systemctl enable --now unattended-upgrades`, whose end state
// is enabled AND running. `is-enabled --quiet` alone also exits 0 for a unit
// in "static" state — which `--now` never promises — and says nothing about
// whether the service actually started. The Check must confirm both halves
// of what the command actually did.
func TestUnattendedUpgradesCheckVerifiesActiveNotJustEnabled(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")
	steps, _ := Plan(p, "symunona")
	uu := stepByID(steps, "unattended-upgrades")
	if uu == nil {
		t.Fatal("no unattended-upgrades step")
	}
	joined := strings.Join(uu.Check, " ")
	if !strings.Contains(joined, "is-enabled") {
		t.Error("unattended-upgrades Check dropped the is-enabled test")
	}
	if !strings.Contains(joined, "is-active") {
		t.Error("unattended-upgrades Check does not verify is-active, only is-enabled — " +
			"the step runs `enable --now`, whose end state includes actually RUNNING")
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
