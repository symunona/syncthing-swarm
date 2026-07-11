package provision

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Renderer prints probe checks as they land, one line each. Deliberately
// line-oriented rather than a full-screen TUI: scrollback survives, there is no
// bubbletea dependency, and it degrades to sane plain text when piped to a file.
type Renderer struct {
	w     io.Writer
	tty   bool
	dirty bool // a spinner/progress line is on screen and must be cleared
}

func NewRenderer(w io.Writer) *Renderer {
	f, ok := w.(*os.File)
	tty := ok && isTerminal(f)
	return &Renderer{w: w, tty: tty}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// clear removes an in-progress line so the finished line can take its place.
func (r *Renderer) clear() {
	if r.dirty && r.tty {
		fmt.Fprint(r.w, "\r\033[K")
	}
	r.dirty = false
}

// Event renders one probe event. "start" events show what is about to be paid
// for — a slow check announces its cost before it runs, so a wait is expected
// rather than alarming.
func (r *Renderer) Event(ev Event, p *Probe) {
	if ev.State == "start" {
		if r.tty {
			fmt.Fprintf(r.w, "  … %-10s %s (%s)", ev.Check, ev.Note, ev.Hint)
			r.dirty = true
		}
		return
	}
	r.clear()

	mark, detail := "✓", ""
	switch ev.State {
	case "skip":
		mark, detail = "-", ev.Note
	case "err":
		mark, detail = "✗", ev.Note
	default:
		detail = summarize(ev.Check, p)
	}
	fmt.Fprintf(r.w, "  %s %-10s %-58s %5s\n", mark, ev.Check, truncate(detail, 58), fmt.Sprintf("%.1fs", float64(ev.MS)/1000))
}

// summarize is the one-line human form of a completed check.
func summarize(check string, p *Probe) string {
	switch check {
	case "box":
		s := fmt.Sprintf("%s — %s, %dc, %s RAM", p.Box.Model, p.Box.Arch, p.Box.Cores, HumanBytes(p.Box.MemBytes))
		if p.Box.SwapBytes > 0 {
			s += " + " + HumanBytes(p.Box.SwapBytes) + " swap"
		}
		return s
	case "inotify":
		s := fmt.Sprintf("max_user_watches=%d", p.Inotify.MaxUserWatches)
		if p.Inotify.MaxUserWatches <= 8192 {
			s += "  ← default, too low for syncthing"
		}
		return s
	case "disks":
		ds := p.Drives()
		if len(ds) == 0 {
			return "no external drive mounted"
		}
		var parts []string
		for _, d := range ds {
			kind := "SSD/flash"
			if d.Rotational {
				kind = "rotational(HDD)"
			}
			parts = append(parts, fmt.Sprintf("%s %s %s %s → %s",
				d.Device, HumanBytes(d.SizeBytes), kind, d.FSType, d.Mountpoint))
		}
		return strings.Join(parts, "; ")
	case "mounts":
		return fmt.Sprintf("%d mounts", len(p.Mounts))
	case "fstab":
		ds := p.Drives()
		if len(ds) == 0 {
			return fmt.Sprintf("%d entries", len(p.Fstab))
		}
		var miss []string
		for _, d := range ds {
			if !p.InFstab(d.UUID) {
				miss = append(miss, d.Mountpoint+": no UUID entry (will NOT survive reboot)")
				continue
			}
			opts := p.MountOptions(d.Mountpoint)
			var lacks []string
			if !strings.Contains(opts, "nofail") {
				lacks = append(lacks, "nofail")
			}
			if !strings.Contains(opts, "noatime") {
				lacks = append(lacks, "noatime")
			}
			if len(lacks) > 0 {
				miss = append(miss, fmt.Sprintf("%s: missing %s", d.Mountpoint, strings.Join(lacks, "+")))
			}
		}
		if len(miss) == 0 {
			return "drive entries present, options ok"
		}
		return strings.Join(miss, "; ")
	case "security":
		s := p.Security
		return fmt.Sprintf("ufw:%s  fail2ban:%s  unattended:%s  sshd:%d  %d listening",
			toolWord(s.UFW), toolWord(s.Fail2ban), toolWord(s.UnattendedUpgrades),
			s.SSHDPort, len(s.Listening))
	case "spindown":
		if !p.Spindown.Present {
			return "hd-idle not installed"
		}
		return fmt.Sprintf("hd-idle present (%s) %s", p.Spindown.Enabled, p.Spindown.Config)
	case "syncthing":
		if !p.Syncthing.Present {
			return "not installed"
		}
		return fmt.Sprintf("%s  unit:%s", shortVersion(p.Syncthing.Version), p.Syncthing.Enabled)
	case "tailscale":
		return p.Tailscale.IP4
	case "capacity":
		var parts []string
		for _, c := range p.Capacity {
			parts = append(parts, fmt.Sprintf("%s %s used of %s, %s inodes",
				c.Root, HumanBytes(c.UsedBytes), HumanBytes(c.SizeBytes), human(c.InodesUsed)))
		}
		return strings.Join(parts, "; ")
	case "stfolders":
		if len(p.StFolders) == 0 {
			return "none found on the drive"
		}
		var names []string
		for _, f := range p.StFolders {
			names = append(names, f.Name)
		}
		return fmt.Sprintf("%d found: %s", len(p.StFolders), strings.Join(names, ", "))
	case "hash":
		if p.Hash == nil {
			return ""
		}
		return fmt.Sprintf("sha256: %s/s", HumanBytes(p.Hash.BytesPerSec))
	}
	return ""
}

func toolWord(t Tool) string {
	if !t.Present {
		return "absent"
	}
	return t.Enabled
}

func shortVersion(v string) string {
	if i := strings.Index(v, " ("); i > 0 {
		return v[:i]
	}
	return v
}

func human(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

// Summary prints what the box is and what to expect of it — the part that turns
// a pile of facts into a decision.
func (r *Renderer) Summary(p *Probe) {
	w := r.w
	fmt.Fprintln(w)

	drives := p.Drives()
	if len(drives) == 0 {
		fmt.Fprintln(w, "  ⚠ no external drive mounted — syncthing folders would land on the boot media.")
		fmt.Fprintln(w, "    On a Pi that means the SD card: slow, and it wears out. Mount the drive first.")
		return
	}

	for _, d := range drives {
		cap := p.CapacityAt(d.Mountpoint)
		if cap == nil {
			continue
		}
		kind := "flash"
		if d.Rotational {
			kind = "spinning disk"
		}
		fmt.Fprintf(w, "  drive        %s (%s, %s) — %s used of %s\n",
			d.Mountpoint, kind, d.Model, HumanBytes(cap.UsedBytes), HumanBytes(cap.SizeBytes))

		// Size the ETA off the syncthing FOLDERS, not the filesystem: syncthing
		// hashes what you configure, not what the disk happens to hold. On rue
		// those differ by 15x (305 GiB drive, 20 GiB of folders).
		if fb := p.FolderBytes(); fb > 0 {
			if secs, ok := p.ScanETA(fb); ok {
				fmt.Fprintf(w, "  initial scan %s of syncthing folders at ~%s/s → est. %s\n",
					HumanBytes(fb), HumanBytes(p.Hash.BytesPerSec), HumanDuration(secs))
				fmt.Fprintf(w, "               one-time, and only for the folders you adopt.\n")
				fmt.Fprintf(w, "               Transfers nothing over the network — matching blocks just verify.\n")
			}
		}

		// inotify: syncthing needs roughly one watch per directory. Over the
		// limit it silently stops watching and falls back to periodic scans —
		// which is what wakes a sleeping disk every hour.
		if int64(p.Inotify.MaxUserWatches) < cap.InodesUsed {
			fmt.Fprintf(w, "  inotify      %d watches < %s files on the drive → syncthing would fall back\n",
				p.Inotify.MaxUserWatches, human(cap.InodesUsed))
			fmt.Fprintf(w, "               to periodic scanning (and keep the disk awake). Needs raising.\n")
		}
	}

	if len(p.StFolders) > 0 {
		fmt.Fprintf(w, "  folders      %d existing syncthing folder(s) on the drive:\n", len(p.StFolders))
		for _, f := range p.StFolders {
			size := "—"
			if f.Bytes > 0 {
				size = HumanBytes(f.Bytes)
			}
			eta := ""
			if secs, ok := p.ScanETA(f.Bytes); ok {
				eta = "  (~" + HumanDuration(secs) + " to hash)"
			}
			fmt.Fprintf(w, "               %-18s %8s  %s%s\n", f.Name, size, f.Path, eta)
		}
		fmt.Fprintln(w, "               These carry no folder ID — syncthing keeps it in its index, not on")
		fmt.Fprintln(w, "               disk. They get matched to the swarm's folders by name + content.")
	}

	// Security findings, smallest useful set.
	var findings []string
	if !p.Security.UFW.Present {
		findings = append(findings, "ufw not installed")
	}
	if !p.Security.Fail2ban.Present {
		findings = append(findings, "fail2ban not installed")
	}
	if !p.Security.UnattendedUpgrades.Present {
		findings = append(findings, "unattended-upgrades not installed")
	}
	if p.Security.PasswordAuth == "yes" {
		findings = append(findings, "sshd allows password auth (reported only — the wizard never edits sshd_config)")
	}
	// Dedupe by port: a service bound to both 0.0.0.0 and :: is one finding, not
	// two, and "port 22, 22" reads like a bug.
	seen := map[int]bool{}
	var exposed []int
	for _, s := range p.Security.Listening {
		if s.Exposed() && !seen[s.Port] {
			seen[s.Port] = true
			exposed = append(exposed, s.Port)
		}
	}
	sort.Ints(exposed)
	if len(exposed) > 0 {
		var ps []string
		for _, port := range exposed {
			ps = append(ps, fmt.Sprintf("%d", port))
		}
		findings = append(findings, "listening on all interfaces: port "+strings.Join(ps, ", "))
	}
	if len(findings) > 0 {
		fmt.Fprintln(w, "  security")
		for _, f := range findings {
			fmt.Fprintf(w, "               • %s\n", f)
		}
	}
	fmt.Fprintln(w)
}
