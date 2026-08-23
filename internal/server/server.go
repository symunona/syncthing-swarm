// Package server runs the background poller and serves the API + web UI.
package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/aggregate"
	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/diskusage"
)

// nowFunc is injectable so tests avoid the wall clock.
var nowFunc = time.Now

type Server struct {
	// cfgPtr holds the live cred store. It is swapped wholesale on reload
	// (see reload.go) rather than mutated, so every reader either sees the
	// old config or the new one and never a half-updated one.
	cfgPtr  atomic.Pointer[config.Config]
	cfgPath  string // watched for changes; empty = no hot reload
	cfgStamp string // contents hash of the cred store as last loaded

	web   fs.FS
	proxy *http.Client

	nodesMu sync.RWMutex
	nodes   map[string]config.Node // name -> node, for the relay proxy

	pollNow chan struct{} // reload pokes this to repoll without waiting a tick

	mu   sync.RWMutex
	snap *aggregate.Snapshot

	diskMu sync.RWMutex
	disk   map[string]diskusage.Usage

	events *eventHub // on-demand relay of node event streams to browsers
}

func New(cfg *config.Config, web fs.FS) *Server {
	s := &Server{
		web:     web,
		proxy:   &http.Client{Timeout: 12 * time.Second},
		pollNow: make(chan struct{}, 1),
	}
	s.cfgPtr.Store(cfg)
	s.setNodes(cfg)
	s.events = newEventHub(s.config)
	return s
}

// config is the cred store as of right now. Callers must not hold it across a
// long operation and expect it to stay current — they should just re-read.
func (s *Server) config() *config.Config { return s.cfgPtr.Load() }

// setNodes rebuilds the name -> node index the relay proxy and the fix
// endpoints look up by name.
func (s *Server) setNodes(cfg *config.Config) {
	nodes := make(map[string]config.Node, len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		nodes[n.Name] = n
	}
	s.nodesMu.Lock()
	s.nodes = nodes
	s.nodesMu.Unlock()
}

// node looks one node up by name in the live config.
func (s *Server) node(name string) (config.Node, bool) {
	s.nodesMu.RLock()
	defer s.nodesMu.RUnlock()
	n, ok := s.nodes[name]
	return n, ok
}

// WatchConfig makes the server reload path whenever it changes on disk, so a
// node added by `stc bootstrap` shows up without a restart.
func (s *Server) WatchConfig(path string) {
	s.cfgPath = path
	// Baseline taken HERE, not in the watcher goroutine: a swarm.yaml written
	// between New and the goroutine's first tick would otherwise become the
	// baseline itself and never register as a change.
	s.cfgStamp, _ = stampFile(path)
}

// Run polls forever and serves HTTP until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.pollOnce(ctx) // prime immediately
	go s.pollLoop(ctx)
	go s.diskLoop(ctx) // disk stats on a slower cadence
	if s.cfgPath != "" {
		go s.watchConfig(ctx) // pick up nodes added while we run
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/matrix", s.handleMatrix)
	mux.HandleFunc("/api/mesh", s.handleMesh)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/disk", s.handleDisk)
	mux.HandleFunc("/api/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	// read-only relay: GET /api/node/{name}/rest/... -> that node's syncthing API
	mux.HandleFunc("GET /api/node/{name}/{path...}", s.handleNodeProxy)
	// write actions (guarded, explicit — not via the relay)
	mux.HandleFunc("POST /api/share", s.handleShare)
	mux.HandleFunc("POST /api/unshare", s.handleUnshare)
	// fixes for diagnosed errors. The destructive ones take preview=true first and
	// the UI shows exactly which files disappear before it will call them.
	mux.HandleFunc("POST /api/fix/rescan", s.handleRescan)
	mux.HandleFunc("POST /api/fix/clean-conflicts", s.handleCleanConflicts)
	mux.HandleFunc("POST /api/fix/revert", s.handleRevert)
	if s.web != nil {
		mux.Handle("/", http.FileServer(http.FS(s.web)))
	}

	srv := &http.Server{Addr: s.config().Listen, Handler: mux}
	go func() {
		<-ctx.Done()
		sd, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sd)
	}()
	cfg := s.config()
	log.Printf("dashboard on http://%s  (%d nodes, poll %ds)", cfg.Listen, len(cfg.Nodes), cfg.PollSeconds)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) pollLoop(ctx context.Context) {
	for {
		// Interval re-read every round: a reloaded pollSeconds applies at once.
		wait := time.NewTimer(time.Duration(s.config().PollSeconds) * time.Second)
		select {
		case <-ctx.Done():
			wait.Stop()
			return
		case <-wait.C:
		case <-s.pollNow: // config reloaded: show the new node now, not in 15s
			wait.Stop()
		}
		s.pollOnce(ctx)
	}
}

func (s *Server) pollOnce(ctx context.Context) {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snap := aggregate.Poll(pctx, s.config(), nowFunc())
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
