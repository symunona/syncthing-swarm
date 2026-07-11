// Package aggregate fans queries out to every managed node and merges the
// results into one folders×devices matrix snapshot.
package aggregate

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

// Cell is one folder's local state on one node.
type Cell struct {
	Present    bool     `json:"present"`    // node has this folder configured
	State      string   `json:"state"`      // idle | scanning | syncing | error | offline
	Completion float64  `json:"completion"` // 0..100 (this node's local copy)
	NeedBytes  int64    `json:"needBytes"`
	NeedItems  int64    `json:"needItems"`
	Errors     []string `json:"errors"`
}

// Device is a column: one managed syncthing node.
type Device struct {
	Name         string   `json:"name"`
	DeviceID     string   `json:"deviceID"`
	Version      string   `json:"version"`
	Online       bool     `json:"online"`
	SystemErrors []string `json:"systemErrors"`
	URL          string   `json:"url"`  // syncthing GUI base (for a direct "open GUI" link)
	Root         string   `json:"root"` // base dir for new shared folders (from swarm.yaml)
}

// Folder is a row.
type Folder struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Snapshot is the whole matrix at one point in time.
type Snapshot struct {
	GeneratedAt time.Time                  `json:"generatedAt"`
	Source      string                     `json:"source"` // local node we share folders FROM
	Devices     []Device                   `json:"devices"`
	Folders     []Folder                   `json:"folders"`
	Cells       map[string]map[string]Cell `json:"cells"` // folderID -> nodeName -> cell
}

// Poll queries all nodes concurrently and builds a Snapshot. Unreachable
// nodes become offline columns; the snapshot is always returned.
func Poll(ctx context.Context, cfg *config.Config, now time.Time) *Snapshot {
	snap := &Snapshot{
		GeneratedAt: now,
		Cells:       map[string]map[string]Cell{},
	}
	if l := cfg.LocalNode(); l != nil {
		snap.Source = l.Name
	}

	type nodeResult struct {
		dev     Device
		folders []stclient.ConfigFolder
		cells   map[string]Cell // folderID -> cell
	}

	results := make([]nodeResult, len(cfg.Nodes))
	var wg sync.WaitGroup
	for i, n := range cfg.Nodes {
		wg.Add(1)
		go func(i int, n config.Node) {
			defer wg.Done()
			results[i] = pollNode(ctx, n)
		}(i, n)
	}
	wg.Wait()

	// merge
	folderLabels := map[string]string{}
	for _, r := range results {
		snap.Devices = append(snap.Devices, r.dev)
		for _, f := range r.folders {
			label := f.Label
			if label == "" {
				label = f.ID
			}
			if folderLabels[f.ID] == "" {
				folderLabels[f.ID] = label
			}
		}
		for fid, cell := range r.cells {
			if snap.Cells[fid] == nil {
				snap.Cells[fid] = map[string]Cell{}
			}
			snap.Cells[fid][r.dev.Name] = cell
		}
	}

	for id, label := range folderLabels {
		snap.Folders = append(snap.Folders, Folder{ID: id, Label: label})
	}
	sort.Slice(snap.Folders, func(i, j int) bool {
		return snap.Folders[i].Label < snap.Folders[j].Label
	})
	return snap
}

func pollNode(ctx context.Context, n config.Node) (r struct {
	dev     Device
	folders []stclient.ConfigFolder
	cells   map[string]Cell
}) {
	r.dev = Device{Name: n.Name, URL: n.URL, Root: n.Root}
	r.cells = map[string]Cell{}
	c := stclient.New(n.URL, n.APIKey)

	ver, err := c.Version(ctx)
	if err != nil {
		r.dev.Online = false
		return r // node down: offline column, no cells
	}
	r.dev.Online = true
	r.dev.Version = ver.Version

	if st, err := c.SystemStatus(ctx); err == nil {
		r.dev.DeviceID = st.MyID
	}
	if errs, err := c.SystemErrors(ctx); err == nil {
		for _, e := range errs {
			r.dev.SystemErrors = append(r.dev.SystemErrors, e.Message)
		}
	}

	cfg, err := c.Config(ctx)
	if err != nil {
		return r
	}
	r.folders = cfg.Folders
	for _, f := range cfg.Folders {
		cell := Cell{Present: true, State: "unknown", Completion: 100}
		if f.Paused {
			cell.State = "paused"
		}
		if st, err := c.DBStatus(ctx, f.ID); err == nil {
			cell.State = st.State
			cell.NeedBytes = st.NeedBytes
			cell.NeedItems = st.NeedItems
			cell.Completion = completion(st.GlobalBytes, st.NeedBytes)
			// The folder-level error — "folder marker missing" when the drive is
			// gone. /rest/folder/errors carries only per-file pull failures, so a
			// folder whose whole disk vanished reports none of those: without this
			// the UI showed a red cell with no reason.
			if st.Error != "" {
				cell.Errors = append(cell.Errors, st.Error)
				cell.State = "error"
			}
		}
		if ferrs, err := c.FolderErrors(ctx, f.ID); err == nil {
			for _, fe := range ferrs {
				cell.Errors = append(cell.Errors, fe.Path+": "+fe.Error)
			}
			if len(cell.Errors) > 0 {
				cell.State = "error"
			}
		}
		r.cells[f.ID] = cell
	}
	return r
}

func completion(global, need int64) float64 {
	if global <= 0 {
		return 100
	}
	if need <= 0 {
		return 100
	}
	pct := float64(global-need) / float64(global) * 100
	if pct < 0 {
		return 0
	}
	return pct
}
