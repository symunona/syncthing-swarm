// Package provision bootstraps a fresh box into the swarm: probe it read-only,
// derive a hardening plan, install syncthing, mesh it with every node, and adopt
// folders already sitting on an intact drive.
//
// Stages are numbered files (00-probe, 01-report, …) so the directory listing is
// the flow. Every stage is a plain function over these types, so a web wizard can
// later be handlers over the same package — the trick internal/sharing already
// uses to serve both swarmd and stc without drift.
package provision

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// --- ssh --------------------------------------------------------------------

// SSH runs commands on the target box. Dest is handed straight to ssh, exactly
// like the `ssh:` field in swarm.yaml (see internal/diskusage): ~/.ssh/config
// owns aliases, ports, keys and tailscale. We never parse hostnames or manage
// keys.
//
// All commands ride one ControlMaster socket: a single auth, and no per-command
// handshake — ~300ms x ~40 commands is real money on a Pi 2. The socket dies
// with the process.
type SSH struct {
	Dest    string // e.g. "rue" or "-p 2222 taskbot"
	ctlPath string
}

func NewSSH(dest string) (*SSH, error) {
	if strings.TrimSpace(dest) == "" {
		return nil, fmt.Errorf("empty ssh destination")
	}
	dir, err := os.MkdirTemp("", "stc-ssh-")
	if err != nil {
		return nil, err
	}
	return &SSH{Dest: dest, ctlPath: filepath.Join(dir, "ctl")}, nil
}

// Close tears down the shared connection.
func (s *SSH) Close() {
	if s.ctlPath == "" {
		return
	}
	args := append(s.opts(), "-O", "exit")
	args = append(args, strings.Fields(s.Dest)...)
	_ = exec.Command("ssh", args...).Run()
	_ = os.RemoveAll(filepath.Dir(s.ctlPath))
}

func (s *SSH) opts() []string {
	return []string{
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + s.ctlPath,
		"-o", "ControlPersist=120",
		"-o", "ConnectTimeout=10",
	}
}

// Command builds an ssh invocation running remote through the remote shell.
// tty=true allocates a terminal, which sudo needs to prompt for a password.
func (s *SSH) Command(ctx context.Context, tty bool, remote string) *exec.Cmd {
	args := s.opts()
	if tty {
		args = append(args, "-t")
	}
	args = append(args, strings.Fields(s.Dest)...)
	args = append(args, remote)
	return exec.CommandContext(ctx, "ssh", args...)
}

// --- probe types ------------------------------------------------------------

// Probe is everything 00-probe.sh learned about the box. Zero values mean "the
// check did not run or failed" — consult Checks before trusting a field.
type Probe struct {
	Box       Box              `json:"box"`
	Inotify   Inotify          `json:"inotify"`
	Disks     []BlockDevice    `json:"disks"`
	Mounts    []Mount          `json:"mounts"`
	Fstab     []string         `json:"fstab"`
	Security  Security         `json:"security"`
	Spindown  Spindown         `json:"spindown"`
	Syncthing SyncthingState   `json:"syncthing"`
	Tailscale Tailscale        `json:"tailscale"`
	Power     Power            `json:"power"`
	Capacity  []FSCapacity     `json:"capacity"`
	StFolders []StFolder       `json:"stfolders"`
	Hash      *HashBench       `json:"hash,omitempty"`
	Checks    map[string]Check `json:"checks"`
}

// Check is the outcome of one probe check.
type Check struct {
	State string `json:"state"` // ok | skip | err | start
	MS    int    `json:"ms"`
	Note  string `json:"note,omitempty"`
}

type Box struct {
	Model     string `json:"model"` // "Raspberry Pi 2 Model B Rev 1.1" — SBCs self-identify
	OS        string `json:"os"`
	Codename  string `json:"codename"`
	Arch      string `json:"arch"` // armv7l -> 32-bit, decides the apt repo arch
	Kernel    string `json:"kernel"`
	Cores     int    `json:"cores"`
	MemBytes  int64  `json:"memBytes"`
	SwapBytes int64  `json:"swapBytes"` // matters on a 512MB Pi: hashing can outgrow RAM
	Hostname  string `json:"hostname"`
	User      string `json:"user"`
	UID       int    `json:"uid"`
}

type Inotify struct {
	MaxUserWatches   int `json:"maxUserWatches"`
	MaxUserInstances int `json:"maxUserInstances"`
}

// BlockDevice mirrors lsblk -J. Children are partitions: note that TRAN and ROTA
// live on the parent disk while MOUNTPOINT lives on the partition, which is why
// callers must walk the tree rather than look at one row.
type BlockDevice struct {
	Name       string        `json:"name"`
	Path       string        `json:"path"`
	Type       string        `json:"type"`
	Size       int64         `json:"size"`
	Rota       bool          `json:"rota"` // true = spinning platter
	Tran       string        `json:"tran"` // usb | sata | nvme | mmc…
	Hotplug    bool          `json:"hotplug"`
	FSType     string        `json:"fstype"`
	UUID       string        `json:"uuid"`
	Label      string        `json:"label"`
	Mountpoint string        `json:"mountpoint"`
	Model      string        `json:"model"`
	Children   []BlockDevice `json:"children"`
}

// Mount mirrors findmnt -J, which nests submounts under their parent — so the
// external drive arrives as a child of /, not as a top-level row. Probe.Mounts
// holds the flattened list.
type Mount struct {
	Target   string  `json:"target"`
	Source   string  `json:"source"`
	FSType   string  `json:"fstype"`
	Options  string  `json:"options"`
	Children []Mount `json:"children,omitempty"`
}

type Security struct {
	UFW                UFWStatus `json:"ufw"`
	Fail2ban           Tool      `json:"fail2ban"`
	UnattendedUpgrades Tool      `json:"unattendedUpgrades"`
	Listening          []Socket  `json:"listening"`
	SSHDPort           int       `json:"sshdPort"` // the LIVE port — never assume 22
	PasswordAuth       string    `json:"passwordAuth"`
	PermitRootLogin    string    `json:"permitRootLogin"`
}

type Tool struct {
	Present bool   `json:"present"`
	Enabled string `json:"enabled"` // enabled | disabled | not-found
	Active  string `json:"active"`  // active | inactive | failed | unknown
}

// UFWStatus is ufw's Tool status plus whether the FIREWALL ITSELF is
// actually up. Present/Enabled/Active describe the systemd unit
// (ufw.service), and none of the three answer the question that matters:
// is anything being filtered right now.
//
// ufw.service is Type=oneshot + RemainAfterExit=yes, and apt's postinst
// enables AND starts it the moment the package is unpacked — entirely
// independent of whether anyone ever ran `ufw enable`. So a box can show
// Enabled=="enabled" and Active=="active" while /etc/ufw/ufw.conf still
// says ENABLED=no and every connection sails through unfiltered. Keying
// anything — the Plan gate, the `done` report — on the systemd fields
// alone reproduces exactly the bug this type exists to fix, just moved up
// one layer from is-active to is-enabled. FirewallUp is ufw's own
// ENABLED=yes flag, read directly from ufw.conf (0644, no sudo needed),
// and it is the only field here that is safe to treat as "the firewall is
// on".
type UFWStatus struct {
	Tool
	FirewallUp bool `json:"firewallUp"`
}

// Broken reports a service that is installed but crashed on startup.
//
// Installed and enabled is NOT the same as running, and conflating them hid a
// real failure: fail2ban installed, enabled itself, then died because bookworm
// ships no rsyslog and its sshd jail found no /var/log/auth.log. The wizard
// reported it as done. Only "failed" is treated as broken — "inactive" is
// legitimate for oneshot units like ufw.service, which applies its rules and
// exits.
func (t Tool) Broken() bool { return t.Present && t.Active == "failed" }

type Socket struct {
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

// Exposed reports whether the socket is reachable from off-box. A service on
// 127.0.0.1 is not a finding; one on 0.0.0.0 or :: is.
func (s Socket) Exposed() bool {
	return s.Addr == "0.0.0.0" || s.Addr == "*" || s.Addr == "::" || s.Addr == "[::]"
}

type Spindown struct {
	Present bool   `json:"present"`
	Enabled string `json:"enabled"`
	Active  string `json:"active"`
	Config  string `json:"config"`
}

type SyncthingState struct {
	Present   bool   `json:"present"`
	Version   string `json:"version"`
	ConfigDir string `json:"configDir"`
	Unit      string `json:"unit"` // syncthing@<user>.service
	Enabled   string `json:"enabled"`
	Active    string `json:"active"`
}

// Power is the Raspberry Pi's own under-voltage record — the check that would
// have caught what killed rue.
//
// A bus-powered 2.5" HDD spikes ~1A on spin-up. A Pi 1 B+ caps its ENTIRE USB bus
// at 600mA unless config.txt sets max_usb_current=1 (which raises it to 1.2A).
// The board browns out, the SD card is corrupted mid-write, and the box never
// boots again. hd-idle makes this far worse: it turns one spin-up at boot into a
// spin-up every time the disk is touched.
type Power struct {
	Throttled     string `json:"throttled"`     // vcgencmd get_throttled, e.g. "0x50005"
	MaxUsbCurrent string `json:"maxUsbCurrent"` // the config.txt line, empty if unset
}

// UnderVoltage reports whether the board has browned out since boot
// (bit 0 = under-voltage now, bit 16 = it has happened).
func (p Power) UnderVoltage() (now, everSinceBoot bool) {
	v, err := strconv.ParseUint(strings.TrimPrefix(p.Throttled, "0x"), 16, 64)
	if err != nil {
		return false, false
	}
	return v&(1<<0) != 0, v&(1<<16) != 0
}

type Tailscale struct {
	IP4    string `json:"ip4"`
	Status string `json:"status"`
}

// FSCapacity comes from df, never du: walking the tree for exact numbers costs
// hours on a USB2 HDD and buys nothing. InodesUsed is an upper bound on the
// inotify watches syncthing will need; UsedBytes is the initial-scan ETA input.
type FSCapacity struct {
	Root        string `json:"root"`
	SizeBytes   int64  `json:"sizeBytes"`
	UsedBytes   int64  `json:"usedBytes"`
	AvailBytes  int64  `json:"availBytes"`
	InodesTotal int64  `json:"inodesTotal"`
	InodesUsed  int64  `json:"inodesUsed"`
}

// StFolder is a directory on the drive carrying a .stfolder marker.
//
// ID is almost always empty, and that is not a bug: .stfolder is a bare marker
// that only proves the drive is mounted. Syncthing keeps the folder ID in its
// index database — exactly what dies with the SD card. The ID is recovered from
// the swarm instead, joined to this by Name (see 05-adopt.go).
type StFolder struct {
	Path  string `json:"path"`  // /mnt/hdd/syncthing/dropx
	Root  string `json:"root"`  // /mnt/hdd
	ID    string `json:"id"`    // ~always "" — see above
	Name  string `json:"name"`  // dropx — the join key against folder labels
	Bytes int64  `json:"bytes"` // du of the folder; 0 if the tree was too big to walk
}

type HashBench struct {
	BytesPerSec int64 `json:"bytesPerSec"`
	SampleBytes int64 `json:"sampleBytes"`
}

// --- derived facts ----------------------------------------------------------

// Drive is a filesystem worth putting syncthing folders on: a mounted partition
// that is not part of the OS install. Rotational drives get the whole aging
// policy (hd-idle, weekly rescan, noatime); flash gets none of it.
type Drive struct {
	Device     string // /dev/sda1
	Mountpoint string // /mnt/hdd — EMPTY means attached but not mounted
	FSType     string
	UUID       string
	Label      string
	Rotational bool
	USB        bool
	SizeBytes  int64
	Model      string
}

// osMount reports whether a mountpoint belongs to the OS install rather than
// being a data drive.
//
// "Is it hotplug/USB?" is the tempting test and it is wrong: on a Raspberry Pi
// the SD card reports hotplug=true, so that rule hands you the boot media as a
// syncthing target. Test what we actually care about instead — where it is
// mounted. This mirrors scan_roots() in 00-probe.sh, so the folder scan and the
// drive list can never disagree about what counts as a drive.
func osMount(mp string) bool {
	switch mp {
	case "", "/", "/boot", "/home", "/usr", "/var":
		return true
	}
	for _, pre := range []string{"/boot/", "/usr/", "/var/", "/snap/", "/proc", "/sys", "/run", "/dev"} {
		if strings.HasPrefix(mp, pre) {
			return true
		}
	}
	return false
}

// Drives walks the lsblk tree pairing each mounted partition with its parent
// disk, since the parent carries `tran`/`rota` while the child carries the
// mountpoint.
func (p *Probe) Drives() []Drive {
	var out []Drive
	for _, disk := range p.Disks {
		if disk.Type != "disk" {
			continue
		}
		for _, part := range disk.Children {
			if part.FSType == "" || part.Mountpoint == "" || osMount(part.Mountpoint) {
				continue
			}
			out = append(out, Drive{
				Device:     part.Path,
				Mountpoint: part.Mountpoint,
				FSType:     part.FSType,
				UUID:       part.UUID,
				Label:      part.Label,
				Rotational: disk.Rota,
				USB:        disk.Tran == "usb",
				SizeBytes:  part.Size,
				Model:      strings.TrimSpace(disk.Model),
			})
		}
	}
	return out
}

// UnmountedDrives are data partitions that are PLUGGED IN but not mounted.
//
// A USB drive attached to a headless box does not auto-mount: there is no
// desktop session running udisks to do it for you. So the obvious flow — plug
// the disk into the Pi, then run the wizard — lands exactly here, and a wizard
// that only looks at MOUNTED filesystems reports "no external drive" while the
// disk sits right there, spinning.
func (p *Probe) UnmountedDrives() []Drive {
	var out []Drive
	for _, disk := range p.Disks {
		if disk.Type != "disk" {
			continue
		}
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
		for _, part := range disk.Children {
			if part.FSType == "" || part.FSType == "swap" || part.Mountpoint != "" {
				continue
			}
			// Already in fstab: configured storage that happens to be unmounted
			// right now (drive powered off, mount failed this boot). Adopting it
			// would append a second fstab line for the same UUID.
			if p.InFstab(part.UUID) {
				continue
			}
			out = append(out, Drive{
				Device:     part.Path,
				FSType:     part.FSType,
				UUID:       part.UUID,
				Label:      part.Label,
				Rotational: disk.Rota,
				USB:        disk.Tran == "usb",
				SizeBytes:  part.Size,
				Model:      strings.TrimSpace(disk.Model),
			})
		}
	}
	return out
}

// ConfiguredUnmounted are partitions that HAVE an fstab entry but are not
// mounted right now.
//
// UnmountedDrives deliberately skips these — an already-configured partition is
// not a new drive awaiting adoption, and offering to re-add it to fstab would
// duplicate the line. But skipping it entirely made the drive vanish: not in
// Drives (not mounted), not in UnmountedDrives (already configured), so the
// wizard told a user with a plugged-in, fully configured disk that no external
// drive was attached. A disk that simply failed to mount this boot is the most
// common way one of these boxes breaks, so name that case and offer the one
// thing it needs — a mount.
func (p *Probe) ConfiguredUnmounted() []Drive {
	var out []Drive
	for _, disk := range p.Disks {
		if disk.Type != "disk" {
			continue
		}
		for _, part := range disk.Children {
			if part.FSType == "" || part.FSType == "swap" || part.Mountpoint != "" {
				continue
			}
			line, ok := p.FstabEntry(part.UUID)
			if !ok {
				continue
			}
			mp := fstabTarget(line)
			if mp == "" || osMount(mp) {
				continue
			}
			out = append(out, Drive{
				Device:     part.Path,
				Mountpoint: mp,
				FSType:     part.FSType,
				UUID:       part.UUID,
				Label:      part.Label,
				Rotational: disk.Rota,
				USB:        disk.Tran == "usb",
				SizeBytes:  part.Size,
				Model:      strings.TrimSpace(disk.Model),
			})
		}
	}
	return out
}

// fstabTarget pulls the mountpoint (field 2) out of an fstab line.
func fstabTarget(line string) string {
	f := strings.Fields(line)
	if len(f) < 2 {
		return ""
	}
	return f[1]
}

// SuggestMountpoint guesses where a drive belongs: /mnt/<label> when the
// filesystem carries one, else /mnt/hdd for a spinning disk, /mnt/ssd for flash.
func SuggestMountpoint(d Drive) string {
	if l := strings.ToLower(strings.TrimSpace(d.Label)); l != "" {
		return "/mnt/" + l
	}
	if d.Rotational {
		return "/mnt/hdd"
	}
	return "/mnt/ssd"
}

// WeakUSBPower reports a board whose USB bus cannot reliably feed a spinning
// disk: a Pi 1 or Pi 2 (600mA total across all ports) with max_usb_current unset.
//
// Later Pis (3 and up) provide 1.2A by default and ignore the knob. Boards that
// are not Pis have no such limiter modelled here.
func (p *Probe) WeakUSBPower() bool {
	m := strings.ToLower(p.Box.Model)
	if !strings.Contains(m, "raspberry pi") {
		return false
	}
	// "Raspberry Pi Model B Plus" (Pi 1), "Raspberry Pi 2 Model B", "Raspberry Pi Zero"
	old := strings.Contains(m, "raspberry pi model") ||
		strings.Contains(m, "raspberry pi 2") ||
		strings.Contains(m, "raspberry pi zero")
	if !old {
		return false
	}
	return !strings.Contains(p.Power.MaxUsbCurrent, "max_usb_current=1")
}

// SpinningUSBDisk reports a rotational disk hanging off USB — the load that
// browns these boards out.
func (p *Probe) SpinningUSBDisk() bool {
	for _, d := range p.Drives() {
		if d.Rotational && d.USB {
			return true
		}
	}
	return false
}

// Capacity of the filesystem mounted at root, nil if the probe skipped it.
func (p *Probe) CapacityAt(root string) *FSCapacity {
	for i := range p.Capacity {
		if p.Capacity[i].Root == root {
			return &p.Capacity[i]
		}
	}
	return nil
}

// MountOptions of target, "" if not mounted.
func (p *Probe) MountOptions(target string) string {
	for _, m := range p.Mounts {
		if m.Target == target {
			return m.Options
		}
	}
	return ""
}

// InFstab reports whether /etc/fstab already has an entry for this UUID — i.e.
// whether the drive comes back on its own after a reboot.
func (p *Probe) InFstab(uuid string) bool {
	_, ok := p.FstabEntry(uuid)
	return ok
}

// FstabEntry returns the fstab line for a UUID.
func (p *Probe) FstabEntry(uuid string) (string, bool) {
	if uuid == "" {
		return "", false
	}
	for _, line := range p.Fstab {
		if strings.Contains(line, uuid) {
			return line, true
		}
	}
	return "", false
}

// FstabHasOption reports whether the fstab entry carries a mount option.
//
// Check the FSTAB LINE, not the live mount options: `nofail` and
// `x-systemd.device-timeout` are consumed by systemd at mount time and never
// show up in /proc/mounts. Testing the live options told us the drive was
// "missing nofail" when the fstab line said nofail plain as day.
func (p *Probe) FstabHasOption(uuid, opt string) bool {
	line, ok := p.FstabEntry(uuid)
	return ok && strings.Contains(line, opt)
}

// FolderBytes totals the folders that would actually be adopted.
//
// NOT the filesystem's used bytes. Syncthing hashes the folders you configure,
// not the disk they sit on — and on the first real box those differed by 15x
// (a 305 GiB drive holding 20 GiB of syncthing folders). Sizing the ETA off df
// produced "7h15m" where the truth was "13 min", which is how you train someone
// to ignore the number.
func (p *Probe) FolderBytes() int64 {
	var n int64
	for _, f := range p.StFolders {
		n += f.Bytes
	}
	return n
}

// ScanETA estimates the one-time initial hash of the given bytes. The scan is
// hash-bound on a small ARM core, and it transfers nothing over the network:
// once hashed, blocks that match what peers already hold are simply marked in
// sync. Returns ok=false when the hash benchmark did not run, or when du was
// skipped (a tree too large to walk), rather than inventing a number.
func (p *Probe) ScanETA(bytes int64) (secs int64, ok bool) {
	if p.Hash == nil || p.Hash.BytesPerSec <= 0 || bytes <= 0 {
		return 0, false
	}
	return bytes / p.Hash.BytesPerSec, true
}

// --- formatting -------------------------------------------------------------

func HumanBytes(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u && exp < 5; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func HumanDuration(secs int64) string {
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%dh%02dm", secs/3600, (secs%3600)/60)
	}
}
