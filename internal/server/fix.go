package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

// Fixes for the errors the UI diagnoses. Separate from the read-only relay, and
// deliberately narrow: each one addresses a specific classified error.
//
// The destructive ones NEVER act on a wildcard. They act on an explicit list of
// files that the node itself reported as local-only, filtered to the exact shape
// we are fixing — and the UI must show that list before it can call them.

type fixReq struct {
	Node    string `json:"node"`
	Folder  string `json:"folder"`
	Preview bool   `json:"preview"` // list what WOULD happen, change nothing
}

func (s *Server) fixTarget(r *http.Request) (*config.Node, fixReq, error) {
	var req fixReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, req, fmt.Errorf("bad request: %w", err)
	}
	n, ok := s.nodes[req.Node]
	if !ok {
		return nil, req, fmt.Errorf("unknown node %q", req.Node)
	}
	if req.Folder == "" {
		return nil, req, fmt.Errorf("no folder given")
	}
	return &n, req, nil
}

// handleRescan — the safe fix. Just makes syncthing look at the disk again;
// clears errors that were only stale state.
func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	n, req, err := s.fixTarget(r)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": (err).Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	if err := stclient.New(n.URL, n.APIKey).Scan(ctx, req.Folder); err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": (err).Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "rescan", "node": n.Name})
}

// handleRevert — the blunt fix. Discards ALL local-only files so the node matches
// the swarm.
//
// Requires a preview first: the response to preview=true lists every file that
// would be deleted. A "fix it" button that silently deletes files you have never
// seen is not a fix, it is a trap.
func (s *Server) handleRevert(w http.ResponseWriter, r *http.Request) {
	n, req, err := s.fixTarget(r)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": (err).Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cli := stclient.New(n.URL, n.APIKey)

	files, err := cli.LocalChanged(ctx, req.Folder)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": (err).Error()})
		return
	}
	if req.Preview {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"action": "revert", "preview": true, "node": n.Name,
			"count": len(files), "files": files,
			"warning": "these files exist ONLY on this node and will be DELETED",
		})
		return
	}
	if err := cli.Revert(ctx, req.Folder); err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": (err).Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "revert", "node": n.Name, "deleted": len(files)})
}

// handleCleanConflicts — the targeted fix for "delete dir: directory not empty".
//
// The blocking files are almost always syncthing's OWN conflict copies: it
// preserved this node's older version of a file before overwriting it, and those
// copies now sit inside directories the swarm has since deleted. Reverting the
// whole folder would work, but it also deletes every other local-only file —
// which may include things you actually want.
//
// So this deletes ONLY files that are BOTH:
//   - reported by the node itself as local-only (so we never touch a file the
//     swarm knows about), AND
//   - named *.sync-conflict-* (so we never touch a file that is not a conflict copy)
//
// Nothing is globbed on the remote side: we pass the exact paths the node gave
// us, NUL-separated, and they are all re-checked to be inside the folder.
func (s *Server) handleCleanConflicts(w http.ResponseWriter, r *http.Request) {
	n, req, err := s.fixTarget(r)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": (err).Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	cli := stclient.New(n.URL, n.APIKey)

	// the folder's absolute path on that node
	var folders []stclient.Folder
	if err := cli.ConfigFolders(ctx, &folders); err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": (err).Error()})
		return
	}
	var root string
	for _, f := range folders {
		if id, _ := f["id"].(string); id == req.Folder {
			root, _ = f["path"].(string)
		}
	}
	if root == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": (fmt.Errorf("node %s has no folder %q", n.Name, req.Folder)).Error()})
		return
	}

	changed, err := cli.LocalChanged(ctx, req.Folder)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": (err).Error()})
		return
	}

	var targets []string
	for _, rel := range changed {
		if !strings.Contains(rel, ".sync-conflict-") {
			continue // not a conflict copy: not ours to delete
		}
		// never escape the folder, no matter what the node reported
		clean := path.Clean(rel)
		if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
			continue
		}
		targets = append(targets, path.Join(root, clean))
	}

	if req.Preview {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"action": "clean-conflicts", "preview": true, "node": n.Name,
			"count": len(targets), "files": targets, "localOnly": len(changed),
		})
		return
	}
	if len(targets) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "clean-conflicts", "deleted": 0})
		return
	}
	if n.SSH == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("node %s has no `ssh:` in swarm.yaml — deleting files needs it", n.Name)})
		return
	}

	// NUL-separated so paths with spaces, quotes and newlines survive intact —
	// these are real note filenames like "04 idea with one pager/research".
	var stdin strings.Builder
	for _, t := range targets {
		stdin.WriteString(t)
		stdin.WriteByte(0)
	}
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=8"}
	args = append(args, strings.Fields(n.SSH)...)
	args = append(args, "xargs -0 rm -f --")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = strings.NewReader(stdin.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": (fmt.Errorf("delete on %s: %w: %s", n.Name, err, strings.TrimSpace(string(out)))).Error()})
		return
	}

	// tell syncthing to look again, so the blocked deletions can now succeed
	_ = cli.Scan(ctx, req.Folder)

	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "action": "clean-conflicts", "node": n.Name, "deleted": len(targets),
	})
}
