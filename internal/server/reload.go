package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
)

// configWatchInterval is how often swarm.yaml is stat'd for changes.
//
// A stat every couple of seconds is free, and it buys the thing that used to
// cost a manual `just deploy`: `stc bootstrap` writes a new node into
// swarm.yaml and the dashboard picks it up on its own. Before this, a node
// added after swarmd started was simply invisible until someone restarted the
// daemon — and nothing on screen said so, because the fleet still rendered
// fine, just one node short.
const configWatchInterval = 2 * time.Second

// stampFile identifies a file version by hashing its contents.
//
// Not size+mtime: the cred store is a couple of kilobytes, so the hash costs
// nothing, and it has no blind spot — two writes inside one mtime tick that
// happen to keep the size (swapping an apikey, fixing an IP) are exactly the
// edits someone makes here, and a stat-based stamp would sleep through them.
// A file that cannot be read yields ok=false, which the watcher treats as "no
// change" rather than "no nodes".
func stampFile(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(raw)
	return string(sum[:]), true
}

// watchConfig reloads the cred store whenever it changes on disk, until ctx is
// cancelled. A file that fails to parse is reported and IGNORED: the running
// fleet keeps the last good config rather than collapsing to zero nodes because
// someone saved a half-written yaml.
func (s *Server) watchConfig(ctx context.Context) {
	last := s.cfgStamp
	t := time.NewTicker(configWatchInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur, ok := stampFile(s.cfgPath)
			if !ok || cur == last {
				continue
			}
			last = cur
			cfg, err := config.Load(s.cfgPath)
			if err != nil {
				log.Printf("config reload: %v — keeping the %d nodes already loaded", err, len(s.config().Nodes))
				continue
			}
			s.applyConfig(cfg)
		}
	}
}

// applyConfig swaps in a freshly loaded cred store and wakes everything that
// reads it.
//
// Listen and the poll interval are handled differently on purpose: the poll
// loop reads its interval fresh every tick, so a changed pollSeconds takes
// effect immediately, but the listen address belongs to a socket that is
// already bound — changing it needs a restart, and saying so is better than
// pretending it applied.
func (s *Server) applyConfig(cfg *config.Config) {
	old := s.config()
	if old != nil && cfg.Listen != old.Listen {
		log.Printf("config: listen %s -> %s takes a restart; still serving on %s",
			old.Listen, cfg.Listen, old.Listen)
	}
	s.cfgPtr.Store(cfg)
	s.setNodes(cfg)
	log.Printf("config reloaded: %d nodes%s", len(cfg.Nodes), nodeDelta(old, cfg))

	s.events.reload() // node event streams follow the new node list
	select {
	case s.pollNow <- struct{}{}: // repoll now, don't make a new node wait a tick
	default:
	}
}

// nodeDelta renders what the reload changed, for the log line.
func nodeDelta(old, cur *config.Config) string {
	if old == nil {
		return ""
	}
	was := map[string]bool{}
	for _, n := range old.Nodes {
		was[n.Name] = true
	}
	now := map[string]bool{}
	var added []string
	for _, n := range cur.Nodes {
		now[n.Name] = true
		if !was[n.Name] {
			added = append(added, n.Name)
		}
	}
	var removed []string
	for _, n := range old.Nodes {
		if !now[n.Name] {
			removed = append(removed, n.Name)
		}
	}
	switch {
	case len(added) == 0 && len(removed) == 0:
		return ""
	case len(removed) == 0:
		return fmt.Sprintf(" (+%v)", added)
	case len(added) == 0:
		return fmt.Sprintf(" (-%v)", removed)
	}
	return fmt.Sprintf(" (+%v -%v)", added, removed)
}
