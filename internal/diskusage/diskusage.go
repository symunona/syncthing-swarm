// Package diskusage collects filesystem usage per node via `df` — locally for
// the hub's own node, over ssh for the rest (syncthing exposes no disk stats).
package diskusage

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/symunona/syncthing-dashboard/internal/config"
)

// Usage is the filesystem holding a node's share root.
type Usage struct {
	Node    string `json:"node"`
	Mount   string `json:"mount"` // mount point df reported
	Total   int64  `json:"total"` // bytes
	Used    int64  `json:"used"`
	Avail   int64  `json:"avail"`
	Pct     int    `json:"pct"` // percent used
	Missing bool   `json:"missing,omitempty"`
	Err     string `json:"err,omitempty"`
}

const missingSentinel = "STC_NO_ROOT"

// dfCommand reports the filesystem holding the node's root dir.
//
// It deliberately does NOT fall back to `/`. It used to, and that made the
// dashboard lie in the one scenario the disk bar exists for: when an external
// drive dies, `df /media/hdd/syncthing` fails, the fallback reports the SD card
// instead, and the UI draws a healthy bar for a drive that is GONE.
//
// A missing root is a finding, not something to paper over.
func dfCommand(path string) string {
	if path == "" {
		path = "/"
	}
	q := "'" + strings.ReplaceAll(path, "'", "") + "'"
	return "test -d " + q + " || { echo " + missingSentinel + "; exit 0; }; " +
		"df -B1 --output=size,used,avail,pcent,target " + q
}

// Collect returns disk usage for one node.
func Collect(ctx context.Context, n config.Node) Usage {
	cmdStr := dfCommand(n.Root)
	var cmd *exec.Cmd
	if n.IsLocal() {
		cmd = exec.CommandContext(ctx, "sh", "-c", cmdStr)
	} else if n.SSH == "" {
		return Usage{Node: n.Name, Err: "no ssh configured for disk stats"}
	} else {
		args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=6"}
		args = append(args, strings.Fields(n.SSH)...) // e.g. "-p 2222 taskbot"
		args = append(args, cmdStr)                   // ssh runs it via the remote shell
		cmd = exec.CommandContext(ctx, "ssh", args...)
	}
	out, err := cmd.Output()
	if err != nil {
		return Usage{Node: n.Name, Err: strings.TrimSpace(errText(err))}
	}
	if strings.Contains(string(out), missingSentinel) {
		// Loud only when a drive is genuinely expected. A node whose root simply
		// hasn't been created yet (no `mount:` declared, root on the main
		// filesystem) is a missing directory, not a dead disk — crying "DRIVE
		// MISSING" at that would train you to ignore the alarm that matters.
		u := Usage{Node: n.Name, Err: fmt.Sprintf("%s does not exist", n.Root)}
		if n.Mount != "" {
			u.Missing = true
			u.Err = fmt.Sprintf("%s does not exist — drive not mounted at %s?", n.Root, n.Mount)
		}
		return u
	}
	u, perr := parse(out)
	if perr != nil {
		return Usage{Node: n.Name, Err: perr.Error()}
	}
	u.Node = n.Name

	// The mountpoint usually outlives the drive: /mnt/hdd stays behind as an
	// empty dir on the boot media, so df silently reports the SD card and the bar
	// looks healthy. If the node told us where its drive belongs, hold df to it.
	if n.Mount != "" && u.Mount != n.Mount {
		return Usage{
			Node:    n.Name,
			Mount:   u.Mount,
			Missing: true,
			Err: fmt.Sprintf("drive not mounted: expected %s, but %s is on %s",
				n.Mount, n.Root, u.Mount),
		}
	}
	return u
}

func errText(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return string(ee.Stderr)
	}
	return err.Error()
}

// parse the second line of `df --output=size,used,avail,pcent,target`.
func parse(out []byte) (Usage, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return Usage{}, fmt.Errorf("unexpected df output")
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 5 {
		return Usage{}, fmt.Errorf("unexpected df columns")
	}
	total, _ := strconv.ParseInt(f[0], 10, 64)
	used, _ := strconv.ParseInt(f[1], 10, 64)
	avail, _ := strconv.ParseInt(f[2], 10, 64)
	pct, _ := strconv.Atoi(strings.TrimSuffix(f[3], "%"))
	return Usage{Mount: f[4], Total: total, Used: used, Avail: avail, Pct: pct}, nil
}
