package provision

import (
	"context"
	"fmt"
	"strings"
)

// SyncthingRelease is the upstream version installed on new nodes.
//
// From the RELEASE TARBALL, not apt — for every architecture, deliberately:
//
//   - apt.syncthing.net ships armhf built for ARMv7. On an ARMv6 Pi (Zero, Pi 1)
//     it dies with "Illegal instruction". Verified on rue.
//   - a distro's own package is years behind (Raspbian: 1.19.2 vs upstream 2.x),
//     so the node would permanently out-drift the rest of the swarm.
//   - the tarball ships its own syncthing@.service unit, so `systemctl enable
//     syncthing@<user>` works exactly as it would with a deb.
//   - tarball installs keep syncthing's built-in auto-upgrade ENABLED (Debian
//     packages disable it), so the box stays current instead of rotting.
const SyncthingRelease = "2.1.2"

// syncthingArch maps `uname -m` to an upstream release asset.
func syncthingArch(unameM string) (string, error) {
	switch unameM {
	case "aarch64", "arm64":
		return "linux-arm64", nil
	case "armv6l", "armv7l", "armhf", "arm":
		return "linux-arm", nil // GOARM=6: runs on ARMv6 too (verified on a Pi 1)
	case "x86_64", "amd64":
		return "linux-amd64", nil
	case "i686", "i386":
		return "linux-386", nil
	}
	return "", fmt.Errorf("no syncthing release for arch %q", unameM)
}

// SyncthingLayout decides where syncthing's pieces live on a node.
type SyncthingLayout struct {
	User      string // runs as this user; must own the files already on the drive
	ConfigDir string // certs + config.xml
	DataDir   string // the index database
	FolderDir string // where shared folders land: <root>/<label>
	Mount     string // the drive the data dir depends on ("" = none)
	TailnetIP string // the GUI binds HERE and nowhere else
}

// PlanSyncthing lays out a node's syncthing install.
//
// Config on the BOOT MEDIA, database on the DRIVE. That split is deliberate:
//
//   - the database is the write-heavy part, and SD-card wear is what kills these
//     boxes (it is why rue needed a new card in the first place).
//   - the config dir is tiny and rarely written, and keeping it on the boot media
//     means the node's IDENTITY survives the drive dying.
//
// The unit is then gated on the mount (RequiresMountsFor). That gate is not
// optional: with the database on the drive, an unmounted drive would otherwise
// have syncthing build a fresh empty database ON THE MOUNTPOINT DIRECTORY — i.e.
// on the SD card — and then re-hash everything when the drive came back. Gated,
// it simply does not start, and the dashboard shows the node offline plus
// ⚠ DRIVE MISSING.
func PlanSyncthing(p *Probe, l SyncthingLayout) ([]Step, error) {
	if l.TailnetIP == "" {
		return nil, fmt.Errorf("no tailnet IP: the node must be on tailscale before syncthing " +
			"(the GUI binds to the tailnet address, never 0.0.0.0)")
	}
	arch, err := syncthingArch(p.Box.Arch)
	if err != nil {
		return nil, err
	}
	if p.Syncthing.Present && p.Syncthing.Enabled == "enabled" && p.Syncthing.Active == "active" {
		return nil, nil // already running
	}

	v := SyncthingRelease
	base := fmt.Sprintf("https://github.com/syncthing/syncthing/releases/download/v%s", v)
	pkg := fmt.Sprintf("syncthing-%s-v%s", arch, v)
	unit := "syncthing@" + l.User

	var steps []Step

	steps = append(steps, Step{
		ID:    "syncthing-install",
		Title: fmt.Sprintf("install syncthing v%s (%s) from the upstream release", v, arch),
		Why: "not apt: apt.syncthing.net's armhf build is ARMv7 and dies with 'Illegal\n" +
			"    instruction' on an ARMv6 Pi, and a distro package would be years behind the\n" +
			"    rest of the swarm. The tarball ships its own systemd unit and keeps\n" +
			"    syncthing's auto-upgrade enabled, so the box stays current by itself.\n" +
			"    The download is checksum-verified before anything is installed",
		Cmds: []string{
			"cd /tmp",
			fmt.Sprintf("curl -fsSL -o %s.tar.gz %s/%s.tar.gz", pkg, base, pkg),
			// The release publishes sha256sum.txt.asc — a PGP CLEAR-signed file —
			// and no plain sha256sum.txt (asking for one 404s). The checksum lines
			// sit in plaintext inside the signed block, so grep reads them directly.
			//
			// Honest about what this buys: both the tarball and the checksum file
			// come from the same TLS-protected GitHub release, so this catches a
			// corrupted or truncated download, not a compromised release. Verifying
			// the PGP signature would need syncthing's public key on the box;
			// syncthing's own auto-upgrade does verify signatures from here on.
			fmt.Sprintf("curl -fsSL -o sha256sum.txt.asc %s/sha256sum.txt.asc", base),
			// verify BEFORE unpacking, and fail loudly if it does not match
			fmt.Sprintf("grep ' %s.tar.gz$' sha256sum.txt.asc | sha256sum -c -", pkg),
			fmt.Sprintf("tar xzf %s.tar.gz", pkg),
			// The binary must be writable by the user it runs AS, or syncthing's
			// self-upgrade cannot replace it. Not an escalation: the unit already
			// runs as that user.
			fmt.Sprintf("install -o %s -g %s -m 0755 %s/syncthing /usr/local/bin/syncthing", l.User, l.User, pkg),
			fmt.Sprintf("install -m 0644 %s/etc/linux-systemd/system/syncthing@.service /etc/systemd/system/", pkg),
			fmt.Sprintf("rm -rf /tmp/%s /tmp/%s.tar.gz /tmp/sha256sum.txt.asc", pkg, pkg),
			// prove the binary actually EXECUTES on this CPU before we build a
			// service around it — this is the ARMv6/ARMv7 trap, caught early
			"sudo -u " + l.User + " /usr/local/bin/syncthing --version",
		},
		Check: []string{
			// Anchor the version to avoid matching v2.1.20 when looking for v2.1.2.
			// The --version output format is "syncthing v2.1.2" followed by other info.
			fmt.Sprintf("/usr/local/bin/syncthing --version 2>/dev/null | grep -qE 'v%s([^0-9.]|$)'", v),
			"test -f /etc/systemd/system/syncthing@.service",
		},
	})

	// dirs, owned by the user that runs syncthing (uid must match what is already
	// on the drive, or every file looks foreign and permissions churn)
	mk := []string{
		fmt.Sprintf("install -d -o %s -g %s %s", l.User, l.User, l.ConfigDir),
		fmt.Sprintf("install -d -o %s -g %s %s", l.User, l.User, l.DataDir),
		fmt.Sprintf("install -d -o %s -g %s %s", l.User, l.User, l.FolderDir),
	}

	override := strings.Join([]string{
		"[Unit]",
		// The gate. Without it, an unmounted drive means a fresh empty database
		// built on the SD card and a full re-hash when the drive returns.
		mountGate(l.Mount),
		"",
		"[Service]",
		"ExecStart=",
		fmt.Sprintf("ExecStart=/usr/local/bin/syncthing serve --no-browser --no-restart "+
			"--config=%s --data=%s", l.ConfigDir, l.DataDir),
	}, "\\n")

	steps = append(steps, Step{
		ID:    "syncthing-service",
		Title: fmt.Sprintf("run syncthing as %s (db on %s, config on the boot media)", l.User, l.DataDir),
		Why: "the index database is the write-heavy part, and SD-card wear is what kills\n" +
			"    these boxes — so it lives on the drive. The config and certs are tiny and\n" +
			"    stay on the boot media, so the node's IDENTITY survives the drive dying.\n" +
			"    The unit is gated on the mount: with the db on the drive, an unmounted\n" +
			"    drive would otherwise have syncthing build an empty database on the SD card\n" +
			"    and re-hash everything later. Gated, it simply does not start",
		Cmds: append(mk,
			fmt.Sprintf("mkdir -p /etc/systemd/system/%s.service.d", unit),
			fmt.Sprintf("printf '%s\\n' > /etc/systemd/system/%s.service.d/override.conf", override, unit),
			"systemctl daemon-reload",
			"systemctl reset-failed "+unit+" 2>/dev/null || true",
			"systemctl enable "+unit,
			"systemctl restart "+unit,
			"systemctl is-active --quiet "+unit,
		),
		Needs: []string{"syncthing-install"},
		Check: []string{
			fmt.Sprintf("test -d %s", l.DataDir),
			fmt.Sprintf("test -d %s", l.FolderDir),
			// Verify the running process carries the right --data flag, not just the file.
			// A run interrupted between writing override.conf and daemon-reload+restart
			// would leave a unit active from its previous state. We check /proc/<pid>/cmdline
			// to prove the live process reflects the desired configuration. This is
			// world-readable, so no sudo needed; a dead unit reports MainPID 0 and the
			// check correctly fails.
			fmt.Sprintf("tr '\\0' ' ' < /proc/$(systemctl show -p MainPID --value %s)/cmdline | grep -q -- '--data=%s'", unit, l.DataDir),
			"systemctl is-active --quiet " + unit,
		},
	})

	// The GUI must listen on the tailnet address ONLY. Binding 0.0.0.0 would put
	// an unauthenticated-by-default admin API on every interface the box has.
	steps = append(steps, Step{
		ID:    "syncthing-gui",
		Title: fmt.Sprintf("bind the GUI/API to %s:8384 (tailnet only)", l.TailnetIP),
		Why: "syncthing starts bound to 127.0.0.1. The dashboard needs to reach it over the\n" +
			"    tailnet — but it must NEVER bind 0.0.0.0: that would expose an admin API on\n" +
			"    the LAN and any public interface. Binding the tailnet IP specifically is the\n" +
			"    biggest security win in this whole flow and it costs nothing",
		Cmds: []string{
			// wait for the config it generates on first start
			fmt.Sprintf("for i in $(seq 1 30); do [ -s %s/config.xml ] && break; sleep 1; done", l.ConfigDir),
			fmt.Sprintf("test -s %s/config.xml", l.ConfigDir),
			"systemctl stop " + unit,
			// Only the GUI listen address. The sync (BEP) listeners are separate
			// entries (tcp://0.0.0.0:22000) and must NOT be touched — syncthing
			// still needs to accept sync connections; it is the admin API that must
			// not be exposed.
			//
			// NB: a plain s|||, not sed's `0,/re/` range form — `0,#re#` is invalid
			// sed (the range form needs / delimiters, or \# with a backslash) and
			// would have failed at runtime. "127.0.0.1:8384" appears only in the
			// GUI block, so a straight substitution is both correct and simpler.
			fmt.Sprintf("sed -i 's|<address>127.0.0.1:8384</address>|<address>%s:8384</address>|' %s/config.xml",
				l.TailnetIP, l.ConfigDir),
			fmt.Sprintf("grep -q '<address>%s:8384</address>' %s/config.xml", l.TailnetIP, l.ConfigDir),
			"systemctl start " + unit,
			"systemctl is-active --quiet " + unit,
		},
		Needs: []string{"syncthing-service"},
		Check: []string{
			// Verify config.xml has the right bind address — this is what survives a restart.
			fmt.Sprintf("grep -q '<address>%s:8384</address>' %s/config.xml", l.TailnetIP, l.ConfigDir),
			// Verify the live listening socket is on the tailnet address, not just that
			// config.xml says so. A sed interrupted before systemctl restart would leave
			// the socket on 127.0.0.1. `ss` listing does not need root (only -p would).
			fmt.Sprintf("ss -tlnH 'sport = :8384' | grep -q '%s:8384'", l.TailnetIP),
			"systemctl is-active --quiet " + unit,
		},
	})

	return steps, nil
}

func mountGate(mount string) string {
	if mount == "" {
		return "# no data drive: nothing to gate on"
	}
	return "RequiresMountsFor=" + mount
}

// SyncthingIdentity is what a freshly installed node tells us about itself.
type SyncthingIdentity struct {
	APIKey   string
	DeviceID string
	Version  string
}

// HarvestIdentity reads back the API key and device ID the node generated, so
// the hub can add it to swarm.yaml and mesh it with the other nodes.
func HarvestIdentity(ctx context.Context, s *SSH, l SyncthingLayout) (*SyncthingIdentity, error) {
	api := fmt.Sprintf("http://%s:8384", l.TailnetIP)

	// Wait for the REST API, do not race it. `systemctl is-active` goes true the
	// moment the process is up, but syncthing takes another second or two to open
	// its HTTP listener — so asking it for its device ID the instant the service
	// starts returns nothing, on a node that is coming up perfectly well. (The
	// same race bit the fail2ban step.)
	script := strings.Join([]string{
		fmt.Sprintf("KEY=$(sed -n 's:.*<apikey>\\(.*\\)</apikey>.*:\\1:p' %s/config.xml | head -n1)", l.ConfigDir),
		`[ -n "$KEY" ] || { echo "no apikey in config.xml" >&2; exit 1; }`,
		fmt.Sprintf(`for i in $(seq 1 30); do `+
			`curl -fsS -m 3 -H "X-API-Key: $KEY" %s/rest/system/status >/dev/null 2>&1 && break; sleep 1; done`, api),
		fmt.Sprintf(`ST=$(curl -fsS -m 5 -H "X-API-Key: $KEY" %s/rest/system/status)`, api),
		fmt.Sprintf(`VE=$(curl -fsS -m 5 -H "X-API-Key: $KEY" %s/rest/system/version)`, api),
		// grep -o, not sed line-matching: syncthing pretty-prints its JSON, so the
		// field can sit anywhere and whitespace varies.
		`ID=$(printf '%s' "$ST" | grep -o '"myID"[^,]*' | grep -o '[A-Z0-9]\{7\}-[A-Z0-9-]*' | head -n1)`,
		`VER=$(printf '%s' "$VE" | grep -o '"version" *: *"[^"]*"' | cut -d'"' -f4)`,
		`printf '%s\t%s\t%s\n' "$KEY" "$ID" "$VER"`,
	}, "; ")

	out, err := s.Command(ctx, false, script).Output()
	if err != nil {
		return nil, fmt.Errorf("read syncthing identity: %w", err)
	}
	f := strings.Split(strings.TrimSpace(string(out)), "\t")
	if len(f) < 2 || f[0] == "" || f[1] == "" {
		return nil, fmt.Errorf("could not read apikey/deviceID from the node (got %q)", string(out))
	}
	id := &SyncthingIdentity{APIKey: f[0], DeviceID: f[1]}
	if len(f) > 2 {
		id.Version = f[2]
	}
	return id, nil
}
