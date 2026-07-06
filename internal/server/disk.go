package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/diskusage"
)

const diskInterval = 60 * time.Second // disk changes slowly; ssh is costly

func (s *Server) diskLoop(ctx context.Context) {
	s.collectDisk(ctx) // prime
	t := time.NewTicker(diskInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.collectDisk(ctx)
		}
	}
}

func (s *Server) collectDisk(ctx context.Context) {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	out := make(map[string]diskusage.Usage, len(s.cfg.Nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range s.cfg.Nodes {
		wg.Add(1)
		go func(n config.Node) {
			defer wg.Done()
			u := diskusage.Collect(dctx, n)
			mu.Lock()
			out[n.Name] = u
			mu.Unlock()
		}(n)
	}
	wg.Wait()

	s.diskMu.Lock()
	s.disk = out
	s.diskMu.Unlock()
}

// GET /api/disk -> {node: Usage}
func (s *Server) handleDisk(w http.ResponseWriter, _ *http.Request) {
	s.diskMu.RLock()
	d := s.disk
	s.diskMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if d == nil {
		w.Write([]byte("{}"))
		return
	}
	json.NewEncoder(w).Encode(d)
}
