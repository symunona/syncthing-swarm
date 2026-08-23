package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/sharing"
)

type shareReq struct {
	Folder string `json:"folder"` // id or label
	Target string `json:"target"` // target node name
	Source string `json:"source"` // optional; defaults to local node
	Path   string `json:"path"`   // optional; overrides target root/label
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func decodeShareReq(r *http.Request) (shareReq, error) {
	var req shareReq
	err := json.NewDecoder(r.Body).Decode(&req)
	return req, err
}

// POST /api/share {folder,target,source?,path?}
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) {
	req, err := decodeShareReq(r)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	// mesh by default here too (pairwise=false) — the dashboard's one-click
	// share should not leave a folder any more pairwise than `stc share` does.
	res, err := sharing.Share(ctx, s.config(), req.Folder, req.Source, req.Target, req.Path, false)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// POST /api/unshare {folder,target,source?}
func (s *Server) handleUnshare(w http.ResponseWriter, r *http.Request) {
	req, err := decodeShareReq(r)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res, err := sharing.Unshare(ctx, s.config(), req.Folder, req.Source, req.Target)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}
