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
			opts := p.MountOptions(d.Mountpoint)
			var lacks []string
			for _, o := range []string{"nofail", "noatime"} {
				if !strings.Contains(opts, o) {
					lacks = append(lacks, o)
				}
			}
			if len(lacks) > 0 {
				done = append(done, fmt.Sprintf("%s in fstab, but missing %s (edit by hand: %s)",
					d.Mountpoint, strings.Join(lacks, "+"), "/etc/fstab"))
			} else {
				done = append(done, fmt.Sprintf("%s in fstab with nofail+noatime", d.Mountpoint))
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
					"apt-get install -y hd-idle",
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
				"apt-get install -y ufw",
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

	if !p.Security.Fail2ban.Present {
		why := "sshd is reachable from the network; fail2ban bans repeat offenders"
		if p.Security.PasswordAuth == "yes" {
			why = "sshd is reachable AND accepts passwords; fail2ban bans repeat offenders"
		}
		steps = append(steps, Step{
			ID:    "fail2ban",
			Title: "install fail2ban",
			Why:   why,
			Cmds: []string{
				"apt-get install -y fail2ban",
				"systemctl enable --now fail2ban",
			},
		})
	} else {
		done = append(done, "fail2ban present ("+p.Security.Fail2ban.Enabled+")")
	}

	if !p.Security.UnattendedUpgrades.Present {
		steps = append(steps, Step{
			ID:    "unattended-upgrades",
			Title: "install unattended-upgrades",
			Why:   "an unpatched box on a tailnet is still an unpatched box",
			Cmds: []string{
				"apt-get install -y unattended-upgrades",
				"printf 'APT::Periodic::Update-Package-Lists \"1\";\\nAPT::Periodic::Unattended-Upgrade \"1\";\\n' > /etc/apt/apt.conf.d/20auto-upgrades",
				"systemctl enable --now unattended-upgrades",
			},
		})
	} else {
		done = append(done, "unattended-upgrades present ("+p.Security.UnattendedUpgrades.Enabled+")")
	}

	// apt-get update once, before any install, if we're installing anything.
	for _, s := range steps {
		if strings.Contains(strings.Join(s.Cmds, " "), "apt-get install") {
			steps = append([]Step{{
				ID:    "apt-update",
				Title: "apt-get update",
				Why:   "package lists must be fresh or the installs below fail on a stale index",
				Cmds:  []string{"apt-get update"},
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
