package provision

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

// SwarmFolder is a folder the swarm already knows, gathered from every node's
// config. The swarm is the ID authority: the folder ID is NOT on disk.
//
// .stfolder is a bare marker whose only job is to prove the drive is mounted.
// Syncthing keeps the folder ID in its index database — which is exactly what
// dies with the boot media. So a node whose SD card failed has all its files and
// none of its folder IDs, and the only place to recover them is the other nodes.
type SwarmFolder struct {
	ID    string
	Label string
	Nodes []string // nodes that carry this folder
	Type  string   // sendreceive | receiveonly | …
}

// SwarmFolders collects every folder the swarm knows about.
func SwarmFolders(ctx context.Context, cfg *config.Config, exclude string) (map[string]*SwarmFolder, error) {
	out := map[string]*SwarmFolder{}
	var reached int
	for i := range cfg.Nodes {
		n := cfg.Nodes[i]
		if n.Name == exclude {
			continue
		}
		c, err := stclient.New(n.URL, n.APIKey).Config(ctx)
		if err != nil {
			continue // an unreachable node just contributes nothing
		}
		reached++
		for _, f := range c.Folders {
			sf, ok := out[f.ID]
			if !ok {
				label := f.Label
				if label == "" {
					label = f.ID
				}
				sf = &SwarmFolder{ID: f.ID, Label: label}
				out[f.ID] = sf
			}
			sf.Nodes = append(sf.Nodes, n.Name)
		}
	}
	if reached == 0 {
		return nil, fmt.Errorf("could not reach any node: the swarm is the only place the folder IDs exist")
	}
	return out, nil
}

// Verdict is how confident we are that a directory on disk IS a given folder.
type Verdict string

const (
	// VerdictExact — name AND content agree. Safe to pre-select.
	VerdictExact Verdict = "exact"
	// VerdictRenamed — content agrees but the directory was renamed. Worth
	// offering: pure name-matching would have silently dropped this folder.
	VerdictRenamed Verdict = "renamed"
	// VerdictNameOnly — the name matches but the CONTENT DOES NOT. This is the
	// dangerous one: adopting under the wrong folder ID makes syncthing treat the
	// files as foreign and, on sendreceive, push them into a folder they do not
	// belong to. Never a default.
	VerdictNameOnly Verdict = "name-only"
	// VerdictOrphan — nothing in the swarm resembles it.
	VerdictOrphan Verdict = "orphan"
)

// Candidate is one directory found on the new node's drive, and our best guess
// at which swarm folder it is.
type Candidate struct {
	Dir      StFolder
	Match    *SwarmFolder
	Score    float64 // Jaccard over top-level entry names, 0..1
	Verdict  Verdict
	LocalTop []string
}

// Adoptable reports whether the wizard is willing to pre-select this.
func (c Candidate) Adoptable() bool { return c.Verdict == VerdictExact }

const (
	// structureAgrees is the Jaccard score at which two listings are "the same
	// folder". Deliberately not high: a receive-only node, an .stignore, or a
	// partially-synced folder legitimately differ at the top level.
	structureAgrees = 0.5
	// structureDisagrees is where a name match becomes SUSPICIOUS rather than
	// merely unconfirmed.
	structureDisagrees = 0.1
)

// Match pairs each directory found on the drive with the swarm folder it most
// likely is, using two INDEPENDENT signals.
//
//  1. name — the directory name against the folder's label. `sharing.Share`
//     creates folders at <root>/<label>, so on a drive our own tooling populated
//     the directory name IS the label.
//  2. content — the folder's top-level entries from the swarm's global index vs
//     the directory's actual top-level entries on disk.
//
// Name alone is not enough, and a real drive proves it: the folder labelled
// "Music" lives in a directory called "music", and "Music Resources" in
// "music_resources". And a COINCIDENTAL name match (two different `backup`
// folders) would cause an adoption under the wrong folder ID — the one genuinely
// destructive mistake available here.
//
// The score is evidence shown to the user, never an automatic decision: a folder
// can legitimately look empty at top level, or be heavily .stignore'd.
func Match(ctx context.Context, cfg *config.Config, ssh *SSH, dirs []StFolder,
	swarm map[string]*SwarmFolder, exclude string) ([]Candidate, error) {

	// top-level listing of each candidate directory on the new node. One `ls` per
	// directory — a single directory read, not a walk.
	local := map[string][]string{}
	for _, d := range dirs {
		names, err := listTop(ctx, ssh, d.Path)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", d.Path, err)
		}
		local[d.Path] = names
	}

	// fingerprint each swarm folder once, from whichever node answers
	fp := map[string][]string{}
	for id, sf := range swarm {
		for _, nodeName := range sf.Nodes {
			n := cfg.Node(nodeName)
			if n == nil || n.Name == exclude {
				continue
			}
			names, err := stclient.New(n.URL, n.APIKey).BrowseTop(ctx, id)
			if err == nil {
				fp[id] = names
				break
			}
		}
	}

	var out []Candidate
	for _, d := range dirs {
		c := Candidate{Dir: d, Verdict: VerdictOrphan, LocalTop: local[d.Path]}

		var best *SwarmFolder
		var bestScore float64
		for _, sf := range swarm {
			s := jaccard(local[d.Path], fp[sf.ID])
			if s > bestScore {
				bestScore, best = s, sf
			}
		}
		nameMatch := findByName(swarm, d.Name)

		switch {
		case nameMatch != nil && best != nil && best.ID == nameMatch.ID && bestScore >= structureAgrees:
			c.Match, c.Score, c.Verdict = nameMatch, bestScore, VerdictExact
		case nameMatch != nil && bestScore < structureDisagrees:
			// The name matches, the content does not. THE dangerous case.
			c.Match, c.Score, c.Verdict = nameMatch, jaccard(local[d.Path], fp[nameMatch.ID]), VerdictNameOnly
		case nameMatch != nil:
			c.Match, c.Score, c.Verdict = nameMatch, jaccard(local[d.Path], fp[nameMatch.ID]), VerdictNameOnly
		case best != nil && bestScore >= structureAgrees:
			// Renamed directory: content agrees, name does not. Pure name-matching
			// would have silently dropped this.
			c.Match, c.Score, c.Verdict = best, bestScore, VerdictRenamed
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir.Name < out[j].Dir.Name })
	return out, nil
}

// findByName matches a directory name against folder labels, case-insensitively.
// "Music" lives in "music/", "Music Resources" in "music_resources/".
func findByName(swarm map[string]*SwarmFolder, dir string) *SwarmFolder {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.ReplaceAll(s, " ", "")
		s = strings.ReplaceAll(s, "_", "")
		s = strings.ReplaceAll(s, "-", "")
		return s
	}
	want := norm(dir)
	for _, sf := range swarm {
		if norm(sf.Label) == want || norm(sf.ID) == want {
			return sf
		}
	}
	return nil
}

// listTop reads a directory's top-level entry names: one directory read, and on
// a spinning disk one spin-up. Not a walk.
func listTop(ctx context.Context, s *SSH, dir string) ([]string, error) {
	cmd := s.Command(ctx, false, "ls -1A "+shellQuote(dir)+" 2>/dev/null || true")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		l = strings.TrimSpace(l)
		// syncthing's own bookkeeping is not part of the folder's content
		if l == "" || l == ".stfolder" || l == ".stignore" || l == ".stversions" {
			continue
		}
		names = append(names, l)
	}
	return names, nil
}

// jaccard = |A ∩ B| / |A ∪ B| over entry names. 1.0 = identical top level.
//
// Two empty listings score 0, not 1: "both look empty" is not evidence that two
// folders are the same folder, and treating it as a match would be exactly the
// wrong direction to err in.
func jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	set := make(map[string]bool, len(a))
	for _, s := range a {
		set[s] = true
	}
	var inter int
	seen := map[string]bool{}
	for _, s := range b {
		if seen[s] {
			continue
		}
		seen[s] = true
		if set[s] {
			inter++
		}
	}
	union := len(set) + len(seen) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Adopt creates the folder on the new node with its EXISTING id at its EXISTING
// path, then adds the new device to that folder on every node that already has
// it. Syncthing rescans, hashes what is on disk, finds it already matches, and
// transfers ~nothing. That is the whole point.
//
// ftype is the folder type on the NEW node (sendreceive | receiveonly).
// rescanSecs 0 disables periodic scanning entirely.
func Adopt(ctx context.Context, cfg *config.Config, newNode config.Node, newID string,
	c Candidate, ftype string, rescanSecs int) error {

	if c.Match == nil {
		return fmt.Errorf("%s: no folder to adopt it as", c.Dir.Name)
	}
	newCli := stclient.New(newNode.URL, newNode.APIKey)

	// devices that should share this folder: everyone who has it, plus us
	devs := []any{map[string]any{"deviceID": newID}}
	for _, nodeName := range c.Match.Nodes {
		n := cfg.Node(nodeName)
		if n == nil {
			continue
		}
		st, err := stclient.New(n.URL, n.APIKey).SystemStatus(ctx)
		if err != nil {
			continue
		}
		devs = append(devs, map[string]any{"deviceID": st.MyID})
	}

	f := stclient.Folder{
		"id":               c.Match.ID,
		"label":            c.Match.Label,
		"path":             c.Dir.Path,
		"type":             ftype,
		"devices":          devs,
		"fsWatcherEnabled": true,
		"rescanIntervalS":  rescanSecs,
	}
	if err := newCli.PutFolder(ctx, f); err != nil {
		return fmt.Errorf("create %s on %s: %w", c.Match.Label, newNode.Name, err)
	}

	// and every node that has the folder must now share it with us
	for _, nodeName := range c.Match.Nodes {
		n := cfg.Node(nodeName)
		if n == nil {
			continue
		}
		cli := stclient.New(n.URL, n.APIKey)
		folder, found, err := cli.GetFolder(ctx, c.Match.ID)
		if err != nil || !found {
			continue
		}
		if addDevice(folder, newID) {
			if err := cli.PutFolder(ctx, folder); err != nil {
				return fmt.Errorf("share %s from %s: %w", c.Match.Label, n.Name, err)
			}
		}
	}
	return nil
}

// addDevice adds deviceID to folder["devices"] if missing; returns true if changed.
func addDevice(f stclient.Folder, deviceID string) bool {
	devs, _ := f["devices"].([]any)
	for _, d := range devs {
		if m, ok := d.(map[string]any); ok {
			if id, _ := m["deviceID"].(string); id == deviceID {
				return false
			}
		}
	}
	f["devices"] = append(devs, map[string]any{"deviceID": deviceID})
	return true
}
