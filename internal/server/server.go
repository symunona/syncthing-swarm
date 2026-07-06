// Package server runs the background poller and serves the API + web UI.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/aggregate"
	"github.com/symunona/syncthing-dashboard/internal/config"
)

// nowFunc is injectable so tests avoid the wall clock.
var nowFunc = time.Now

type Server struct {
	cfg   *config.Config
	web   fs.FS
	nodes map[string]config.Node // name -> node, for the relay proxy
	proxy *http.Client

	mu   sync.RWMutex
	snap *aggregate.Snapshot
}

func New(cfg *config.Config, web fs.FS) *Server {
	nodes := make(map[string]config.Node, len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		nodes[n.Name] = n
	}
	return &Server{
		cfg:   cfg,
		web:   web,
		nodes: nodes,
		proxy: &http.Client{Timeout: 12 * time.Second},
	}
}

// Run polls forever and serves HTTP until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.pollOnce(ctx) // prime immediately
	go s.pollLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/matrix", s.handleMatrix)
	mux.HandleFunc("/api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	// read-only relay: GET /api/node/{name}/rest/... -> that node's syncthing API
	mux.HandleFunc("GET /api/node/{name}/{path...}", s.handleNodeProxy)
	// write actions (guarded, explicit — not via the relay)
	mux.HandleFunc("POST /api/share", s.handleShare)
	mux.HandleFunc("POST /api/unshare", s.handleUnshare)
	if s.web != nil {
		mux.Handle("/", http.FileServer(http.FS(s.web)))
	}

	srv := &http.Server{Addr: s.cfg.Listen, Handler: mux}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sd)
	}()
	log.Printf("dashboard on http://%s  (%d nodes, poll %ds)", s.cfg.Listen, len(s.cfg.Nodes), s.cfg.PollSeconds)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) pollLoop(ctx context.Context) {
	t := time.NewTicker(time.Duration(s.cfg.PollSeconds) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollOnce(ctx)
		}
	}
}

func (s *Server) pollOnce(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snap := aggregate.Poll(pctx, s.cfg, nowFunc())
	s.mu.Lock()
	s.snap = snap
	s.mu.Unlock()
	log.Printf("polled: %d devices, %d folders", len(snap.Devices), len(snap.Folders))
}

func (s *Server) handleMatrix(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	snap := s.snap
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*") // dev: vite on another port
	if snap == nil {
		w.Write([]byte(`{"devices":[],"folders":[],"cells":{}}`))
		return
	}
	json.NewEncoder(w).Encode(snap)
}
