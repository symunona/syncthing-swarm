package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
)

const twoNodes = `listen: "127.0.0.1:8888"
nodes:
  - name: pandora
    url: http://127.0.0.1:8384
    apikey: AAA
    local: true
  - name: fiona
    url: http://100.86.131.51:8384
    apikey: BBB
`

func writeCfg(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadCfg(t *testing.T, path string) *config.Config {
	t.Helper()
	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A node appended to swarm.yaml while swarmd runs must become visible without a
// restart — the whole point of the watcher. This is the bug it fixes: chloe was
// added 100 seconds after the daemon started and stayed invisible for two hours.
func TestWatchConfigPicksUpANewNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarm.yaml")
	writeCfg(t, path, twoNodes)

	s := New(loadCfg(t, path), nil)
	s.WatchConfig(path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.watchConfig(ctx)

	writeCfg(t, path, twoNodes+`  - name: chloe
    url: http://100.77.238.79:8384
    apikey: CCC
`)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.node("chloe"); ok {
			if got := len(s.config().Nodes); got != 3 {
				t.Fatalf("config has %d nodes, want 3", got)
			}
			return // the relay proxy can reach it too, not just the poller
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("chloe never appeared after swarm.yaml changed")
}

// A half-written or broken yaml must not take the fleet down with it: the last
// good config keeps serving.
func TestWatchConfigKeepsTheLastGoodConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarm.yaml")
	writeCfg(t, path, twoNodes)

	s := New(loadCfg(t, path), nil)
	s.WatchConfig(path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.watchConfig(ctx)

	writeCfg(t, path, "nodes: [ this is not\n")
	time.Sleep(3 * configWatchInterval)

	if got := len(s.config().Nodes); got != 2 {
		t.Fatalf("broken yaml left %d nodes, want the 2 from before", got)
	}
	if _, ok := s.node("fiona"); !ok {
		t.Fatal("relay index lost fiona after a failed reload")
	}
}

// Reload must not swap the listen address out from under a bound socket; it
// reports and keeps serving where it is.
func TestReloadKeepsServingTheBoundListenAddr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarm.yaml")
	writeCfg(t, path, twoNodes)
	s := New(loadCfg(t, path), nil)

	next := loadCfg(t, path)
	next.Listen = "127.0.0.1:9999"
	s.applyConfig(next)

	if got := s.config().Listen; got != "127.0.0.1:9999" {
		t.Fatalf("config not swapped: listen %q", got)
	}
}

// A removed node must disappear from the relay index, or the proxy keeps
// answering for a machine that is no longer in the swarm.
func TestReloadDropsARemovedNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "swarm.yaml")
	writeCfg(t, path, twoNodes)
	s := New(loadCfg(t, path), nil)

	writeCfg(t, path, `listen: "127.0.0.1:8888"
nodes:
  - name: pandora
    url: http://127.0.0.1:8384
    apikey: AAA
    local: true
`)
	s.applyConfig(loadCfg(t, path))

	if _, ok := s.node("fiona"); ok {
		t.Fatal("fiona still in the relay index after being removed from swarm.yaml")
	}
}
