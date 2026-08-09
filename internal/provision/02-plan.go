package provision

import (
	"fmt"
	"strings"
)

// Step is one unit of change on the box. Every step prints its exact commands
// before it runs, and nothing runs without a prompt.
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

// Plan derives what needs doing on this box. Only what is actually missing shows
// up: a step whose precondition is already satisfied is reported in `done`
// instead, which is what makes re-running the wizard a "check my node" tool.
//
// Nothing here touches the ssh access path. Password auth and exposed ports are
// reported (see Findings) but never edited: a bug that locks you out of a
// headless box in another room is unrecoverable without a keyboard and a monitor.
func Plan(p *Probe, user string) (steps []Step, done []string) {
	drives := p.Drives()

	// --- inotify -------------------------------------------------------------
	// Syncthing needs roughly one watch per directory. Over the limit it stops
	// watching, silently, and falls back to periodic scanning — which is exactly
	// what keeps a spinning disk awake, defeating the rest of this plan.
	want := wantedWatches(p)
	if p.Inotify.MaxUserWatches < want {
		// Don't claim "the drive holds ~0 files" on a box with no drive attached.
		why := fmt.Sprintf("at %d watches syncthing gives up on watching and falls back to\n"+
			"    periodic scans, which keep the disk spinning", p.Inotify.MaxUserWatches)
		if n := inodesOnDrives(p, drives); n > 0 {
			why = fmt.Sprintf("the drive holds ~%s files; at %d watches syncthing gives up on watching\n"+
				"    and falls back to periodic scans, which keep the disk spinning",
				human(n), p.Inotify.MaxUserWatches)
		}
		steps = append(steps, Step{
			ID:    "inotify",
			Title: fmt.Sprintf("raise inotify watches %d → %d", p.Inotify.MaxUserWatches, want),
			Why:   why,
			Cmds: []string{
				fmt.Sprintf("printf 'fs.inotify.max_user_watches=%d\\n' > /etc/sysctl.d/60-syncthing.conf", want),
				"sysctl --system >/dev/null",
			},
			Check: []string{fmt.Sprintf("[ \"$(cat /proc/sys/fs/inotify/max_user_watches)\" -ge %d ]", want)},
		})
	} else {
		done = append(done, fmt.Sprintf("inotify watches already %d", p.Inotify.MaxUserWatches))
	}

	// --- an attached but unmounted drive -------------------------------------
	// A USB disk on a headless box does not auto-mount — no desktop session, no
	// udisks. So "plug the drive in, run the wizard" lands here, and until it is
	// mounted the wizard is blind to it: no fstab step, no spindown, no folder
	// scan, and syncthing folders would default onto the SD card.
	for _, u := range p.UnmountedDrives() {
		mp := SuggestMountpoint(u)
		kind := "flash"
		if u.Rotational {
			kind = "spinning disk"
		}
		steps = append(steps, Step{
			ID: "mount:" + mp,
			Title: fmt.Sprintf("mount %s (%s %s) at %s",
				u.Device, HumanBytes(u.SizeBytes), kind, mp),
			Why: "the drive is plugged in but NOT mounted — a USB disk on a headless box\n" +
				"    never auto-mounts. Until it is, syncthing folders would land on the SD\n" +
				"    card. Mounted by UUID with nofail + a 10s device timeout, so a missing\n" +
				"    drive can never stall boot",
			Cmds: []string{
				"mkdir -p " + mp,
				"cp /etc/fstab /etc/fstab.stc-backup",
				fmt.Sprintf("grep -q '%s' /etc/fstab || "+
					"printf 'UUID=%s\\t%s\\t%s\\tdefaults,nofail,noatime,x-systemd.device-timeout=10s\\t0\\t0\\n' >> /etc/fstab",
					u.UUID, u.UUID, mp, u.FSType),
				"systemctl daemon-reload",
				"mount " + mp,
				"findmnt " + mp,
			},
			Check: []string{"findmnt -n " + mp + " >/dev/null"},
		})
	}

	// --- a configured but currently unmounted drive --------------------------
	// Task 1 stopped counting an already-fstabbed partition as a new drive
	// awaiting adoption (UnmountedDrives), which is right — but skipping it
	// entirely made it invisible: not in Drives (not mounted), not in
	// UnmountedDrives (already configured). The wizard then told a user with a
	// plugged-in, fully configured disk that no external drive was attached at
	// all. Nothing needs configuring here, only mounting.
	for _, d := range p.ConfiguredUnmounted() {
		steps = append(steps, Step{
			ID:    "mount-configured:" + d.Mountpoint,
			Title: fmt.Sprintf("mount %s at %s (already in fstab)", d.Device, d.Mountpoint),
			Why: "the drive has an fstab entry but is NOT mounted right now — it failed to\n" +
				"    mount this boot, or it was powered off. Nothing needs configuring; it\n" +
				"    just needs mounting. Until it is, the wizard sees no data drive at all\n" +
				"    and syncthing folders would land on the boot media",
			Cmds:  []string{"mount " + d.Mountpoint, "findmnt " + d.Mountpoint},
			Check: []string{"findmnt -n " + d.Mountpoint + " >/dev/null"},
		})
	}

	// --- the drive -----------------------------------------------------------
	for _, d := range drives {
		if !p.InFstab(d.UUID) {
			steps = append(steps, Step{
				ID:    "fstab:" + d.Mountpoint,
				Title: fmt.Sprintf("mount %s at %s on boot", d.Device, d.Mountpoint),
				Why: "the drive has no fstab entry — it does NOT come back after a reboot.\n" +
					"    nofail + a 10s device timeout mean a missing drive never stalls boot\n" +
					"    (nofail alone still waits systemd's default 90s), and pass 0 skips a\n" +
					"    boot-time fsck of a 466GB USB disk, which is its own kind of hang",
				Cmds: []string{
					"cp /etc/fstab /etc/fstab.stc-backup",
					fmt.Sprintf("grep -q '%s' /etc/fstab || "+
						"printf 'UUID=%s\\t%s\\t%s\\tdefaults,nofail,noatime,x-systemd.device-timeout=10s\\t0\\t0\\n' >> /etc/fstab",
						d.UUID, d.UUID, d.Mountpoint, d.FSType),
					"systemctl daemon-reload",
					"findmnt " + d.Mountpoint + " >/dev/null || mount " + d.Mountpoint,
				},
				Check: []string{fmt.Sprintf("grep -q '%s' /etc/fstab", d.UUID)},
			})
		} else {
			// Check the FSTAB LINE, not the live mount options: nofail and
			// x-systemd.device-timeout are consumed by systemd at mount time and
			// never appear in /proc/mounts, so testing the live options reported
			// "missing nofail" on a drive whose fstab line plainly had it.
			var lacks []string
			for _, o := range []string{"nofail", "noatime"} {
				if !p.FstabHasOption(d.UUID, o) {
					lacks = append(lacks, o)
				}
			}
			if len(lacks) > 0 {
				done = append(done, fmt.Sprintf("%s in fstab, but missing %s — edit /etc/fstab by hand",
					d.Mountpoint, strings.Join(lacks, "+")))
			} else {
				done = append(done, fmt.Sprintf("%s in fstab with nofail+noatime", d.Mountpoint))
			}

			// noatime IS a live mount option, and an fstab edit does not apply to
			// an already-mounted filesystem. Without a remount the drive keeps
			// writing an access timestamp for every file read — which on a
			// spinning disk means read traffic keeps waking it up, quietly
			// undoing the whole spindown policy until the next reboot.
			if p.FstabHasOption(d.UUID, "noatime") && !strings.Contains(p.MountOptions(d.Mountpoint), "noatime") {
				steps = append(steps, Step{
					ID:    "remount:" + d.Mountpoint,
					Title: fmt.Sprintf("remount %s to pick up noatime", d.Mountpoint),
					Why: "fstab says noatime but the live mount does not have it — an fstab edit\n" +
						"    does not apply to an already-mounted filesystem. Until this is\n" +
						"    remounted, every file READ writes an access timestamp, which keeps\n" +
						"    waking the disk and undoes the spindown policy",
					Cmds:  []string{"mount -o remount " + d.Mountpoint},
					Check: []string{fmt.Sprintf("findmnt -no OPTIONS %s | grep -q noatime", d.Mountpoint)},
				})
			}
		}

		// Spindown is meaningless on flash, and hd-idle would be dead weight.
		if !d.Rotational {
			done = append(done, fmt.Sprintf("%s is flash — no spindown policy needed", d.Mountpoint))
			continue
		}

		// Can this board actually feed that disk?
		//
		// This is the check that would have saved rue. A bus-powered 2.5" HDD
		// spikes ~1A on spin-up; a Pi 1 B+ caps its WHOLE USB bus at 600mA unless
		// config.txt raises it. hd-idle turns one spin-up at boot into a spin-up
		// every time the disk is touched — so enabling it on an underpowered board
		// converts a working node into a box that browns out, corrupts its SD card
		// mid-write, and never boots again. Which is exactly what happened.
		if d.USB && p.WeakUSBPower() {
			steps = append(steps, Step{
				ID:    "usb-power",
				Title: "raise the USB current limit (max_usb_current=1)",
				Why: fmt.Sprintf("%s caps its ENTIRE USB bus at 600mA, and the %s spins up at ~1A.\n"+
					"    Your power supply is not the bottleneck — the BOARD is. Without this the\n"+
					"    board browns out on spin-up and corrupts the SD card mid-write.\n"+
					"    Needs a >=2A supply, and a reboot to take effect",
					p.Box.Model, d.Device),
				Warn: "This raises the limit to 1.2A, which is still marginal against a ~1A spin-up.\n" +
					"     The real fix is a powered USB hub or a Y-cable, so the drive does not\n" +
					"     draw from the Pi at all.",
				Cmds: []string{
					"CFG=/boot/firmware/config.txt; [ -f $CFG ] || CFG=/boot/config.txt; " +
						"grep -q '^max_usb_current=1' $CFG || printf 'max_usb_current=1\\n' >> $CFG; " +
						"grep -n max_usb_current $CFG",
				},
				Check: []string{"CFG=/boot/firmware/config.txt; [ -f $CFG ] || CFG=/boot/config.txt; grep -q '^max_usb_current=1' $CFG"},
			})
		}

		if !p.Spindown.Present {
			disk := parentDisk(d.Device)
			step := Step{
				ID:    "hd-idle",
				Title: fmt.Sprintf("spin %s down after 10min idle (hd-idle)", disk),
				Why: "hdparm -S sets the DRIVE's own idle timer, and most USB-SATA bridges\n" +
					"    silently swallow it — you set it, hdparm says OK, the disk never sleeps.\n" +
					"    hd-idle watches /proc/diskstats and issues the sleep itself",
				Cmds: []string{
					aptGet("install -y hd-idle"),
					fmt.Sprintf("printf 'START_HD_IDLE=true\\nHD_IDLE_OPTS=\"-i 0 -a %s -i 600\"\\n' > /etc/default/hd-idle", disk),
					// The package's postinst starts hd-idle with its DEFAULT config —
					// before ours exists — so it fails, retries, and burns systemd's
					// start limit. Our `enable --now` then gets refused with
					// "start-limit-hit", and the real error is buried. Clear the
					// counter before starting the service with the config we wrote.
					"systemctl reset-failed hd-idle 2>/dev/null || true",
					"systemctl enable hd-idle",
					"systemctl restart hd-idle",
					// installed is not running
					"systemctl is-active --quiet hd-idle",
				},
				// is-active alone is satisfied by a hd-idle watching the WRONG
				// disk — a stale /etc/default/hd-idle left from a drive that
				// has since been replaced, or a restart that raced a previous
				// instance. Mirror what the syncthing service Check does with
				// /proc/<pid>/cmdline: verify the config we actually wrote is
				// the one in effect, not just that some hd-idle is running.
				Check: []string{
					"systemctl is-active --quiet hd-idle",
					fmt.Sprintf("grep -q -- '-a %s' /etc/default/hd-idle", disk),
				},
				Needs: []string{"apt-update"},
			}
			// A spindown policy on a board that cannot feed the disk is not a
			// tuning choice, it is a way to destroy the box: every spin-up is a
			// brownout risk, and a brownout mid-write corrupts the SD card. Make
			// the user type it out rather than tapping y.
			if d.USB && p.WeakUSBPower() {
				step.Warn = "DANGEROUS ON THIS BOARD. " + p.Box.Model + " limits USB to 600mA and this\n" +
					"     disk spins up at ~1A. hd-idle turns one spin-up at boot into a spin-up\n" +
					"     every time the disk is touched — each one a brownout risk that can\n" +
					"     corrupt the SD card mid-write and leave the box unbootable.\n" +
					"     Apply the max_usb_current step first, and prefer a powered hub.\n" +
					"     Spin-up/down CYCLES are what age a drive; continuous spinning does not."
				step.Confirm = "spindown"
			}
			if now, ever := p.Power.UnderVoltage(); now || ever {
				step.Warn += "\n     ⚡ this board HAS ALREADY BROWNED OUT (vcgencmd get_throttled=" +
					p.Power.Throttled + "). Fix the power before cycling the disk."
				step.Confirm = "spindown"
			}
			steps = append(steps, step)
		} else {
			done = append(done, "hd-idle already installed")
		}
	}
	if len(drives) == 0 {
		if len(p.UnmountedDrives()) > 0 {
			done = append(done, "drive is attached but unmounted — the mount step below fixes that; "+
				"spindown gets planned once it is mounted (re-run to pick it up)")
		} else {
			done = append(done, "no data drive — skipping fstab and spindown steps")
		}
	}

	// --- security ------------------------------------------------------------
	// Additive and reversible only. Nothing here edits sshd_config.
	// Key on FirewallUp, not Present and not Enabled. ufw.service is
	// Type=oneshot + RemainAfterExit=yes, and apt's postinst both enables AND
	// starts it the moment the package lands — entirely independent of
	// whether anyone ever ran `ufw enable`. So an installed ufw can show
	// Present=true AND Enabled=="enabled" while the box has no firewall at
	// all: those are systemd's account of the UNIT, not ufw's account of
	// itself. Gating on Enabled (an earlier version of this fix) reproduced
	// the exact bug it was meant to close, just one layer up from is-active.
	// FirewallUp is read straight from /etc/ufw/ufw.conf's ENABLED= flag —
	// the one thing `ufw enable`/`ufw disable` actually write — so it is the
	// only field that answers "is anything being filtered right now". Gating
	// on anything else meant no step was ever planned for an
	// installed-but-never-enabled box, and it was reported under `done` as
	// "✓ ufw present (enabled)" — a box with no firewall, called finished.
	if !p.Security.UFW.FirewallUp {
		port := p.Security.SSHDPort
		steps = append(steps, Step{
			ID:    "ufw",
			Title: "install ufw and enable it",
			Why: fmt.Sprintf("no firewall. The allow rules go in BEFORE enable, and the ssh rule uses\n"+
				"    port %d — read from this box's sshd config, never assumed to be 22", port),
			Warn: "THIS CAN LOCK YOU OUT of a headless box. The ssh allow rule for port " +
				fmt.Sprint(port) + " and the tailscale0 rule are applied first, and the enable is last.",
			Confirm: "ufw",
			Cmds: []string{
				aptGet("install -y ufw"),
				// ufw is only as good as the iptables underneath it, and on an old
				// ARMv6 Pi the nf_tables backend is broken outright — even
				// `iptables -L` dies with "target extension not found". ufw then
				// fails to apply ANY rule while still reporting itself enabled,
				// which is the worst of both worlds: you believe you have a
				// firewall and you have nothing. Fall back to the legacy backend
				// when the default one cannot even list a chain.
				"iptables -L -n >/dev/null 2>&1 || { " +
					"echo 'nf_tables backend broken — switching iptables to legacy'; " +
					"update-alternatives --set iptables /usr/sbin/iptables-legacy && " +
					"update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy; }",
				"iptables -L -n >/dev/null", // hard stop if it is STILL broken: never "enable" a firewall that cannot filter
				"ufw default deny incoming",
				"ufw default allow outgoing",
				// ssh FIRST. If this line fails, the enable below must not run —
				// hence && rather than ;
				fmt.Sprintf("ufw allow %d/tcp comment 'ssh'", port),
				"ufw allow in on tailscale0 comment 'tailnet (syncthing + gui)'",
				"ufw --force enable",
				"ufw status verbose",
			},
			// `ufw status` needs root, so check the observables that do not.
			// NOT `systemctl is-active --quiet ufw`: that unit is Type=oneshot +
			// RemainAfterExit=yes and goes active the instant apt's postinst
			// starts it, whether or not `ufw enable` was ever run — so a box
			// with is-active=active can still have ENABLED=no in
			// /etc/ufw/ufw.conf and let everything through. That file is 0644,
			// so reading the real flag stays unprivileged.
			Check: []string{"command -v ufw >/dev/null", "grep -q '^ENABLED=yes' /etc/ufw/ufw.conf"},
			Needs: []string{"apt-update"},
		})
	} else {
		done = append(done, "ufw present and enabled (firewall up)")
	}

	// fail2ban's sshd jail defaults to reading /var/log/auth.log — which does not
	// exist on Debian 12, because bookworm ships no rsyslog and logs to the
	// journal. Out of the box it installs, enables itself, and then DIES with
	// "Have not found any log file for sshd jail". So configure the journal
	// backend up front, and repair a box where this already happened.
	f2bCmds := []string{
		"printf '[sshd]\\nenabled = true\\nbackend = systemd\\n' > /etc/fail2ban/jail.local",
		"systemctl enable fail2ban",
		"systemctl restart fail2ban",
		// Do not declare victory on a service that is not running.
		"systemctl is-active --quiet fail2ban",
		// ...but do not race it either. fail2ban-server takes a second or two to
		// create its socket, and asking it for jail status the instant after
		// `restart` returns fails with "Is fail2ban running?" on a daemon that is
		// in fact starting perfectly well. Wait for readiness instead of guessing.
		"for i in $(seq 1 20); do fail2ban-client status sshd >/dev/null 2>&1 && break; sleep 1; done",
		"fail2ban-client status sshd",
	}
	switch {
	case !p.Security.Fail2ban.Present:
		why := "sshd is reachable from the network; fail2ban bans repeat offenders"
		if p.Security.PasswordAuth == "yes" {
			why = "sshd is reachable AND accepts passwords; fail2ban bans repeat offenders"
		}
		steps = append(steps, Step{
			ID:    "fail2ban",
			Title: "install fail2ban (journal backend)",
			Why: why + "\n" +
				"    Debian 12 has no rsyslog and thus no /var/log/auth.log, which fail2ban's\n" +
				"    sshd jail reads by default — so it installs, enables, and then crashes.\n" +
				"    Point it at the journal instead",
			Cmds:  append([]string{aptGet("install -y fail2ban")}, f2bCmds...),
			Check: []string{"systemctl is-active --quiet fail2ban", "test -f /etc/fail2ban/jail.local"},
			Needs: []string{"apt-update"},
		})
	case p.Security.Fail2ban.Broken():
		steps = append(steps, Step{
			ID:    "fail2ban-repair",
			Title: "repair fail2ban (installed, but crashed)",
			Why: "fail2ban is installed and enabled but the service is FAILED — almost always\n" +
				"    the missing /var/log/auth.log on Debian 12. Installed is not running:\n" +
				"    right now this box has no brute-force protection at all",
			Cmds: f2bCmds,
			// Same end state as a fresh install, but fail2ban-repair installs
			// nothing — the package is already there — so it must NOT need
			// apt-update. TestNeedsIsSparseAndPointsAtRealSteps enforces this.
			Check: []string{"systemctl is-active --quiet fail2ban", "test -f /etc/fail2ban/jail.local"},
		})
	default:
		done = append(done, "fail2ban present and "+p.Security.Fail2ban.Active)
	}

	if !p.Security.UnattendedUpgrades.Present {
		steps = append(steps, Step{
			ID:    "unattended-upgrades",
			Title: "install unattended-upgrades",
			Why:   "an unpatched box on a tailnet is still an unpatched box",
			Cmds: []string{
				aptGet("install -y unattended-upgrades"),
				"printf 'APT::Periodic::Update-Package-Lists \"1\";\\nAPT::Periodic::Unattended-Upgrade \"1\";\\n' > /etc/apt/apt.conf.d/20auto-upgrades",
				"systemctl enable --now unattended-upgrades",
			},
			// The step runs `enable --now`, whose end state is BOTH enabled and
			// running. Checking only is-enabled misses that: `is-enabled
			// --quiet` also exits 0 for a unit in "static" state, which is not
			// what `--now` promises, and either way a unit can be enabled
			// without ever having started (a prior `enable` with no `--now`,
			// or a start that failed silently). is-active pins the half the
			// old Check let slip through.
			Check: []string{
				"test -f /etc/apt/apt.conf.d/20auto-upgrades",
				"systemctl is-enabled --quiet unattended-upgrades",
				"systemctl is-active --quiet unattended-upgrades",
			},
			Needs: []string{"apt-update"},
		})
	} else {
		done = append(done, "unattended-upgrades present ("+p.Security.UnattendedUpgrades.Enabled+")")
	}

	// apt-get update once, before any install, if we're installing anything.
	// NB: match on "install", not "apt-get install" — aptGet() splices options
	// between the two words, and matching the literal silently dropped this step.
	for _, s := range steps {
		if strings.Contains(strings.Join(s.Cmds, " "), "install -y") {
			steps = append([]Step{{
				ID:    "apt-update",
				Title: "apt-get update",
				Why:   "package lists must be fresh or the installs below fail on a stale index",
				Cmds:  []string{aptGet("update"), "touch /var/lib/apt/lists/.stc-update-stamp"},
				// NOT "did an index file's mtime change in the last day": apt sets
				// each index's mtime from the server's Last-Modified header, not
				// from when WE downloaded it, and on a 304 Not Modified it does not
				// touch the file at all. Debian bookworm's main Packages index is
				// only regenerated at point releases, months apart — so on a real
				// box `apt-get update` runs clean, every index comes back 304 or
				// with a months-old Last-Modified, and `-newermt '-1 day'` is false
				// for every file. The step then recorded `failed` even though the
				// update worked, which blocked hd-idle/ufw/fail2ban/
				// unattended-upgrades (all of them Needs: apt-update) on every
				// single run. apt-get update has no other observable end state —
				// no version number, no file it always rewrites — so give the step
				// its own stamp instead of trying to read apt's mind about the
				// index files. /var/lib/apt/lists is 0755, so the stamp is
				// readable by the unprivileged Check.
				Check: []string{"find /var/lib/apt/lists/.stc-update-stamp -newermt '-1 day' 2>/dev/null | grep -q ."},
			}}, steps...)
			break
		}
	}
	return steps, done
}

// Findings are things the wizard reports but will NOT change: everything that
// touches the ssh access path, plus anything listening off-box. You get the exact
// command; you decide.
func Findings(p *Probe) []string {
	var out []string

	// Under-voltage is a hardware fact the Pi records for you. It is the single
	// most useful thing to know about a Pi with a disk hanging off it, and it is
	// invisible unless you ask.
	if now, ever := p.Power.UnderVoltage(); now || ever {
		when := "has browned out since boot"
		if now {
			when = "is browning out RIGHT NOW"
		}
		out = append(out, fmt.Sprintf("⚡ under-voltage: this board %s (get_throttled=%s). "+
			"A brownout mid-write corrupts the SD card. Suspect the USB load first — "+
			"a bus-powered spinning disk is the usual culprit", when, p.Power.Throttled))
	}
	if p.Security.PasswordAuth == "yes" {
		out = append(out, "sshd accepts passwords — consider: PasswordAuthentication no in "+
			"/etc/ssh/sshd_config.d/10-hardening.conf (the wizard will NOT do this for you: "+
			"a mistake here locks you out of a headless box)")
	}
	if r := p.Security.PermitRootLogin; r == "yes" {
		out = append(out, "sshd permits root login — consider: PermitRootLogin prohibit-password")
	}
	seen := map[int]bool{}
	for _, s := range p.Security.Listening {
		if s.Exposed() && !seen[s.Port] {
			seen[s.Port] = true
			if s.Port != p.Security.SSHDPort {
				out = append(out, fmt.Sprintf("port %d listens on all interfaces — bind it to "+
					"the tailnet or localhost if it does not need to face the LAN", s.Port))
			}
		}
	}
	return out
}

// wantedWatches sizes the inotify limit to the actual tree, not a superstition
// number. Files-on-drive is an upper bound on directories, so this over-provisions
// on purpose — the failure mode of too few watches (silent fallback to polling)
// is far worse than a few MB of kernel memory.
func wantedWatches(p *Probe) int {
	const floor = 204800
	n := inodesOnDrives(p, p.Drives())
	want := int(n) * 2
	if want < floor {
		want = floor
	}
	// round up to a tidy number
	return ((want + 8191) / 8192) * 8192
}

func inodesOnDrives(p *Probe, drives []Drive) int64 {
	var n int64
	for _, d := range drives {
		if c := p.CapacityAt(d.Mountpoint); c != nil {
			n += c.InodesUsed
		}
	}
	return n
}

// aptGet builds an apt-get command that WAITS for the dpkg lock rather than
// dying on it.
//
// This is not theoretical: enabling unattended-upgrades makes it immediately
// start its own apt run, which held the lock and killed the very next step of
// our own plan. Any box that auto-updates can do this to you at any moment.
// DPkg::Lock::Timeout makes apt block instead (apt >= 2.0; bookworm has 2.6).
func aptGet(args string) string {
	return "apt-get -o DPkg::Lock::Timeout=300 " + args
}

// parentDisk turns /dev/sda1 into /dev/sda: hd-idle spins down disks, not
// partitions.
func parentDisk(part string) string {
	s := strings.TrimRight(part, "0123456789")
	// nvme0n1p1 -> nvme0n1 (strip the trailing "p")
	if strings.HasSuffix(s, "p") && strings.Contains(s, "nvme") {
		s = strings.TrimSuffix(s, "p")
	}
	return s
}
