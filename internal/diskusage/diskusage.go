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

// Usage is the filesystem holding a node's share root (or / as fallback).
type Usage struct {
	Node  string `json:"node"`
	Mount string `json:"mount"` // mount point df reported
	Total int64  `json:"total"` // bytes
	Used  int64  `json:"used"`
	Avail int64  `json:"avail"`
	Pct   int    `json:"pct"` // percent used
	Err   string `json:"err,omitempty"`
}

// df of the node's root dir, falling back to / if that path doesn't exist.
func dfCommand(path string) string {
	if path == "" {
		path = "/"
	}
	q := "'" + strings.ReplaceAll(path, "'", "") + "'"
	base := "df -B1 --output=size,used,avail,pcent,target "
	return base + q + " 2>/dev/null || " + base + "/"
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
	u, perr := parse(out)
	if perr != nil {
		return Usage{Node: n.Name, Err: perr.Error()}
	}
	u.Node = n.Name
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
