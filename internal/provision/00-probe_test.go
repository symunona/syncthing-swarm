package provision

import (
	"os"
	"strings"
	"testing"
)

// parseFixture feeds a captured probe transcript through the same absorb path
// RunProbe uses, without needing a box to ssh to.
func parseFixture(t *testing.T, name string) *Probe {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	p := &Probe{Checks: map[string]Check{}}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev Event
		if err := jsonUnmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("unparseable probe line: %s: %v", truncate(line, 100), err)
		}
		if ev.State != "ok" {
			continue
		}
		p.Checks[ev.Check] = Check{State: ev.State, MS: ev.MS, Note: ev.Note}
		if err := p.absorb(ev); err != nil {
			t.Fatalf("absorb %s: %v", ev.Check, err)
		}
	}
	return p
}

// rue is a Pi 1 B+ (ARMv6, 1 core, 427MB) with a 466GB USB HDD holding nine
// pre-existing syncthing folders. It is the box that caught two design bugs:
// the apt repo is ARMv7-only, and .stfolder carries no folder ID.
func TestProbeRue(t *testing.T) {
	p := parseFixture(t, "rue-pi1b.ndjson")

	if got, want := p.Box.Arch, "armv6l"; got != want {
		t.Errorf("arch = %q, want %q — this is what rules out the armhf apt repo", got, want)
	}
	if p.Box.Cores != 1 || p.Box.MemBytes == 0 {
		t.Errorf("box = %d cores, %d bytes RAM; want a 1-core box with nonzero RAM", p.Box.Cores, p.Box.MemBytes)
	}
	if p.Box.SwapBytes == 0 {
		t.Error("swap = 0; rue has 511MiB and it matters on a 427MiB box")
	}

	// The drive: one mounted partition on a rotational USB disk. Getting this
	// right means walking the lsblk tree — `tran`/`rota` live on the parent
	// disk, `mountpoint` on the child partition.
	drives := p.Drives()
	if len(drives) != 1 {
		t.Fatalf("Drives() = %d, want 1 (the USB HDD)", len(drives))
	}
	d := drives[0]
	if !d.Rotational {
		t.Error("drive not detected as rotational — the whole spindown policy hangs off this")
	}
	if d.Mountpoint != "/mnt/hdd" || d.FSType != "ext4" {
		t.Errorf("drive = %s (%s), want /mnt/hdd (ext4)", d.Mountpoint, d.FSType)
	}
	if d.UUID == "" {
		t.Error("drive has no UUID — the fstab entry is written from it")
	}

	// The drive is NOT in fstab: it does not survive a reboot today. This is a
	// finding, and the test pins it so the fstab step stays derived, not assumed.
	if p.InFstab(d.UUID) {
		t.Error("InFstab = true; rue's drive has no fstab entry (that's the finding)")
	}

	// inotify: 8192 default vs 31k files. Under the file count means syncthing
	// silently abandons watching and falls back to periodic scans.
	cap := p.CapacityAt(d.Mountpoint)
	if cap == nil {
		t.Fatal("no capacity for the drive; the scan ETA and inotify sizing both need it")
	}
	if cap.InodesUsed == 0 {
		t.Error("inodesUsed = 0 — `df -i --output` is an error, it must be `df --output`")
	}
	if int64(p.Inotify.MaxUserWatches) >= cap.InodesUsed {
		t.Errorf("watches %d >= %d files: fixture should show the limit being too low",
			p.Inotify.MaxUserWatches, cap.InodesUsed)
	}

	// Nine folders on the drive, none carrying an ID — syncthing keeps the ID in
	// its index, not on disk. Adoption joins them to the swarm by name.
	if len(p.StFolders) != 9 {
		t.Errorf("stfolders = %d, want 9", len(p.StFolders))
	}
	for _, f := range p.StFolders {
		if f.ID != "" {
			t.Errorf("folder %q has ID %q on disk; .stfolder is a bare marker — "+
				"if this ever passes, upstream started stamping IDs and adopt can use them",
				f.Name, f.ID)
		}
		if f.Name == "" || f.Path == "" {
			t.Errorf("folder with empty name/path: %+v", f)
		}
	}

	// Security: the three tools are all absent on rue, and sshd's real port must
	// be read (never assumed) since the ufw allow rule is written from it.
	if p.Security.UFW.Present || p.Security.Fail2ban.Present {
		t.Error("fixture should show ufw and fail2ban absent")
	}
	if p.Security.SSHDPort == 0 {
		t.Error("sshd port = 0; the ufw allow rule is written from this — a wrong value locks us out")
	}

	// The hash benchmark is the initial-scan ETA input.
	if p.Hash == nil || p.Hash.BytesPerSec == 0 {
		t.Fatal("no hash benchmark in fixture")
	}

	// The ETA must be sized off the FOLDERS, not the filesystem. Getting this
	// wrong is not cosmetic: rue's drive holds 305 GiB but only ~20 GiB of
	// syncthing folders, so df-based sizing claimed "7h15m" where the truth is
	// ~23 min. An ETA that overstates by 15x is one nobody reads.
	fb := p.FolderBytes()
	if fb == 0 {
		t.Fatal("FolderBytes = 0: per-folder du missing from the probe")
	}
	if fb >= cap.UsedBytes {
		t.Errorf("FolderBytes (%s) >= filesystem used (%s): the folders are a SUBSET of the disk",
			HumanBytes(fb), HumanBytes(cap.UsedBytes))
	}
	fsSecs, _ := p.ScanETA(cap.UsedBytes)
	folderSecs, ok := p.ScanETA(fb)
	if !ok || folderSecs <= 0 {
		t.Fatalf("ScanETA over folders = %ds (ok=%v)", folderSecs, ok)
	}
	if folderSecs >= fsSecs {
		t.Errorf("folder ETA (%s) is not cheaper than filesystem ETA (%s) — sizing is still off df",
			HumanDuration(folderSecs), HumanDuration(fsSecs))
	}
}

// A box with no data drive must not produce a drive, a capacity, or a plan that
// touches fstab/hd-idle. The wizard has to degrade, not guess.
//
// The SD card here is the trap: on a Raspberry Pi it reports hotplug=true, so a
// "is it hotplug/USB?" test hands you the BOOT MEDIA as a syncthing target —
// slow, and it wears out. Drives() tests the mountpoint instead.
func TestProbeSDCardIsNotADrive(t *testing.T) {
	p := &Probe{
		Checks: map[string]Check{"stfolders": {State: "skip", Note: "no external drive"}},
		Disks: []BlockDevice{{
			Name: "mmcblk0", Type: "disk", Tran: "mmc", Rota: false, Hotplug: true,
			Children: []BlockDevice{
				{Name: "mmcblk0p1", Path: "/dev/mmcblk0p1", Mountpoint: "/boot/firmware", FSType: "vfat"},
				{Name: "mmcblk0p2", Path: "/dev/mmcblk0p2", Mountpoint: "/", FSType: "ext4"},
			},
		}},
	}
	if got := p.Drives(); len(got) != 0 {
		t.Errorf("Drives() = %+v, want none: the SD card is the boot media, not a data drive", got)
	}
	if p.CapacityAt("/mnt/hdd") != nil {
		t.Error("CapacityAt returned a value for a mountpoint that does not exist")
	}
	if _, ok := p.ScanETA(1 << 40); ok {
		t.Error("ScanETA claimed an estimate with no hash benchmark")
	}
}

// lsblk prints no PATH for a swap partition — it prints the literal placeholder
// "[SWAP]" in the MOUNTPOINT column. That string is not a path, but it passed
// every test Drives() applied: it has a filesystem, it has a non-empty
// mountpoint, and "[SWAP]" is not an OS path. So an ordinary single-disk box —
// EFI + root + swap, no data drive at all — had its SWAP PARTITION adopted as
// the syncthing target, and the layout came out RELATIVE:
//
//	--data=[SWAP]/syncthing-db   RequiresMountsFor=[SWAP]
//
// which root's shell resolved against the login user's home (creating a stray
// ~/[SWAP]) while systemd, whose cwd is /, could not resolve it at all: the unit
// enabled fine and then crash-looped. Verified on chloe.
func TestProbeSwapIsNotADrive(t *testing.T) {
	p := &Probe{
		Disks: []BlockDevice{{
			Name: "sda", Type: "disk", Tran: "sata", Rota: false,
			Children: []BlockDevice{
				{Name: "sda1", Path: "/dev/sda1", Mountpoint: "/boot/efi", FSType: "vfat"},
				{Name: "sda2", Path: "/dev/sda2", Mountpoint: "/", FSType: "ext4"},
				{Name: "sda3", Path: "/dev/sda3", Mountpoint: "[SWAP]", FSType: "swap"},
			},
		}},
	}
	if got := p.Drives(); len(got) != 0 {
		t.Errorf("Drives() = %+v, want none: swap is not a data drive", got)
	}
}

// Belt and braces for the same class of bug: any mountpoint that is not an
// absolute path is a placeholder, not a place to put a database.
func TestProbeRelativeMountpointIsNotADrive(t *testing.T) {
	p := &Probe{
		Disks: []BlockDevice{{
			Name: "sdb", Type: "disk", Tran: "usb", Rota: true,
			Children: []BlockDevice{
				{Name: "sdb1", Path: "/dev/sdb1", Mountpoint: "[SOMETHING]", FSType: "ext4"},
			},
		}},
	}
	if got := p.Drives(); len(got) != 0 {
		t.Errorf("Drives() = %+v, want none: %q is not an absolute path", got, "[SOMETHING]")
	}
}
