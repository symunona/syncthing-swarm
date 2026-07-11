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
		steps = append(steps, Step{
			ID:    "inotify",
			Title: fmt.Sprintf("raise inotify watches %d → %d", p.Inotify.MaxUserWatches, want),
			Why: fmt.Sprintf("the drive holds ~%s files; at %d watches syncthing gives up on watching\n"+
				"    and falls back to periodic scans, which keep the disk spinning",
				human(inodesOnDrives(p, drives)), p.Inotify.MaxUserWatches),
			Cmds: []string{
				fmt.Sprintf("printf 'fs.inotify.max_user_watches=%d\\n' > /etc/sysctl.d/60-syncthing.conf", want),
				"sysctl --system >/dev/null",
			},
		})
	} else {
		done = append(done, fmt.Sprintf("inotify watches already %d", p.Inotify.MaxUserWatches))
	}

	// --- the drive -----------------------------------------------------------
	for _, d := range drives {
		if !p.InFstab(d.UUID) {
			steps = append(steps, Step{
				ID:    "fstab",
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
					ID:    "remount",
					Title: fmt.Sprintf("remount %s to pick up noatime", d.Mountpoint),
					Why: "fstab says noatime but the live mount does not have it — an fstab edit\n" +
						"    does not apply to an already-mounted filesystem. Until this is\n" +
						"    remounted, every file READ writes an access timestamp, which keeps\n" +
						"    waking the disk and undoes the spindown policy",
					Cmds: []string{"mount -o remount " + d.Mountpoint},
				})
			}
		}

		// Spindown is meaningless on flash, and hd-idle would be dead weight.
		if !d.Rotational {
			done = append(done, fmt.Sprintf("%s is flash — no spindown policy needed", d.Mountpoint))
			continue
		}
		if !p.Spindown.Present {
			disk := parentDisk(d.Device)
			steps = append(steps, Step{
				ID:    "hd-idle",
				Title: fmt.Sprintf("spin %s down after 10min idle (hd-idle)", disk),
				Why: "hdparm -S sets the DRIVE's own idle timer, and most USB-SATA bridges\n" +
					"    silently swallow it — you set it, hdparm says OK, the disk never sleeps.\n" +
					"    hd-idle watches /proc/diskstats and issues the sleep itself",
				Cmds: []string{
					aptGet("install -y hd-idle"),
					fmt.Sprintf("printf 'START_HD_IDLE=true\\nHD_IDLE_OPTS=\"-i 0 -a %s -i 600\"\\n' > /etc/default/hd-idle", disk),
					"systemctl enable --now hd-idle",
				},
			})
		} else {
			done = append(done, "hd-idle already installed")
		}
	}
	if len(drives) == 0 {
		done = append(done, "no data drive — skipping fstab and spindown steps")
	}

	// --- security ------------------------------------------------------------
	// Additive and reversible only. Nothing here edits sshd_config.
	if !p.Security.UFW.Present {
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
		})
	} else {
		done = append(done, "ufw present ("+p.Security.UFW.Enabled+")")
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
			Cmds: append([]string{aptGet("install -y fail2ban")}, f2bCmds...),
		})
	case p.Security.Fail2ban.Broken():
		steps = append(steps, Step{
			ID:    "fail2ban-repair",
			Title: "repair fail2ban (installed, but crashed)",
			Why: "fail2ban is installed and enabled but the service is FAILED — almost always\n" +
				"    the missing /var/log/auth.log on Debian 12. Installed is not running:\n" +
				"    right now this box has no brute-force protection at all",
			Cmds: f2bCmds,
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
				Cmds:  []string{aptGet("update")},
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
