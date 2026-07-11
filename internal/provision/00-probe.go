package provision

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

// probeScript is piped to the box over ssh stdin — never copied there, so it
// leaves nothing behind and there is no version skew between the script on disk
// and the binary parsing it.
//
//go:embed 00-probe.sh
var probeScript []byte

// Event is one NDJSON line from 00-probe.sh. The script emits these as each
// check finishes rather than one blob at the end, so the caller can render
// partial results instead of staring at a dead cursor.
type Event struct {
	Check string          `json:"check"`
	State string          `json:"state"` // start | ok | skip | err
	MS    int             `json:"ms"`
	Hint  string          `json:"hint,omitempty"` // "~2s" — declared cost, shown before it is paid
	Note  string          `json:"note,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// ProbeOpts tunes what the probe collects.
type ProbeOpts struct {
	// Hash runs the sha256 throughput benchmark (~2s). It is the input to the
	// initial-scan ETA, and the scan is hash-bound on a small ARM core, so this
	// one number predicts the whole adoption.
	Hash bool
	// ScanRoots overrides which directories are scanned for .stfolder markers.
	// Empty = every mounted non-OS filesystem the box has.
	ScanRoots []string
}

// RunProbe surveys the box read-only. It cannot change anything: every command
// in the script is a read, and it never uses sudo.
//
// onEvent fires as each check completes, on the caller's goroutine, with the
// probe built so far — so a renderer can stream partial results live. The event's
// data is already absorbed into the probe by the time onEvent sees it.
func RunProbe(ctx context.Context, s *SSH, opts ProbeOpts, onEvent func(Event, *Probe)) (*Probe, error) {
	env := ""
	if opts.Hash {
		env += "PROBE_BENCH=hash "
	}
	if len(opts.ScanRoots) > 0 {
		env += "PROBE_SCAN_ROOTS=" + shellQuote(strings.Join(opts.ScanRoots, " ")) + " "
	}

	cmd := s.Command(ctx, false, env+"bash -s")
	cmd.Stdin = bytes.NewReader(probeScript)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh %s: %w", s.Dest, err)
	}

	p := &Probe{Checks: map[string]Check{}}
	sc := bufio.NewScanner(stdout)
	// lsblk on a box with many disks produces a long line; the default 64KiB
	// scanner limit is not obviously enough.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var parseErrs []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			// A malformed line loses one check, not the run. Report it, keep going.
			parseErrs = append(parseErrs, truncate(line, 120))
			continue
		}
		if ev.State == "start" {
			if onEvent != nil {
				onEvent(ev, p) // progress only; the result line follows
			}
			continue
		}
		p.Checks[ev.Check] = Check{State: ev.State, MS: ev.MS, Note: ev.Note}
		if ev.State == "ok" {
			if err := p.absorb(ev); err != nil {
				parseErrs = append(parseErrs, fmt.Sprintf("%s: %v", ev.Check, err))
			}
		}
		// absorb first: the renderer summarizes from the probe, not the raw event.
		if onEvent != nil {
			onEvent(ev, p)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read probe output: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("probe on %s: %w: %s", s.Dest, err, strings.TrimSpace(stderr.String()))
	}
	if len(p.Checks) == 0 {
		return nil, fmt.Errorf("probe on %s produced no output: %s", s.Dest, strings.TrimSpace(stderr.String()))
	}
	if len(parseErrs) > 0 {
		p.Checks["_parse"] = Check{
			State: "err",
			Note:  fmt.Sprintf("%d unparseable line(s): %s", len(parseErrs), strings.Join(parseErrs, "; ")),
		}
	}
	return p, nil
}

// absorb decodes one ok event's payload into the Probe.
func (p *Probe) absorb(ev Event) error {
	switch ev.Check {
	case "box":
		return json.Unmarshal(ev.Data, &p.Box)
	case "inotify":
		return json.Unmarshal(ev.Data, &p.Inotify)
	case "disks":
		var w struct {
			BlockDevices []BlockDevice `json:"blockdevices"`
		}
		if err := json.Unmarshal(ev.Data, &w); err != nil {
			return err
		}
		p.Disks = w.BlockDevices
	case "mounts":
		var w struct {
			Filesystems []Mount `json:"filesystems"`
		}
		if err := json.Unmarshal(ev.Data, &w); err != nil {
			return err
		}
		p.Mounts = flattenMounts(w.Filesystems)
	case "fstab":
		return json.Unmarshal(ev.Data, &p.Fstab)
	case "security":
		return json.Unmarshal(ev.Data, &p.Security)
	case "spindown":
		return json.Unmarshal(ev.Data, &p.Spindown)
	case "syncthing":
		return json.Unmarshal(ev.Data, &p.Syncthing)
	case "power":
		return json.Unmarshal(ev.Data, &p.Power)
	case "tailscale":
		return json.Unmarshal(ev.Data, &p.Tailscale)
	case "capacity":
		return json.Unmarshal(ev.Data, &p.Capacity)
	case "stfolders":
		return json.Unmarshal(ev.Data, &p.StFolders)
	case "hash":
		var h HashBench
		if err := json.Unmarshal(ev.Data, &h); err != nil {
			return err
		}
		p.Hash = &h
	}
	return nil
}

// flattenMounts walks findmnt's tree into a flat list. findmnt -J nests
// submounts under their parent, so the external drive shows up as a child of /
// — a naive top-level read would miss every mount that matters here.
func flattenMounts(in []Mount) []Mount {
	var out []Mount
	var walk func([]Mount)
	walk = func(ms []Mount) {
		for _, m := range ms {
			kids := m.Children
			m.Children = nil
			out = append(out, m)
			walk(kids)
		}
	}
	walk(in)
	return out
}

// jsonUnmarshal keeps the test's fixture parsing on the same decoder as RunProbe.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
