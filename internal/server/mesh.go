package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

// meshNode is one device in the connection graph. Managed nodes are the ones we
// hold keys for (swarm.yaml columns); everything else is a peer we only learn
// about because a managed node is configured to talk to it.
type meshNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Managed  bool   `json:"managed"`
	Online   bool   `json:"online"`
	Error    bool   `json:"error"`
	LastSeen string `json:"lastSeen,omitempty"`
}

// meshEdge is an undirected link between two devices. Connected reflects the
// live state (at least one end reports a live connection); a configured-but-
// down link is still returned with Connected=false so the UI can draw it faded.
type meshEdge struct {
	A         string `json:"a"`
	B         string `json:"b"`
	Connected bool   `json:"connected"`
	Type      string `json:"type,omitempty"`
	Addr      string `json:"addr,omitempty"`
}

type meshGraph struct {
	GeneratedAt time.Time  `json:"generatedAt"`
	Nodes       []meshNode `json:"nodes"`
	Edges       []meshEdge `json:"edges"`
}

// meshPoll is one managed node's answer, gathered concurrently.
type meshPoll struct {
	node   config.Node
	myID   string
	online bool
	hasErr bool
	names  map[string]string
	conns  map[string]stclient.Connection
	stats  map[string]stclient.DeviceStat
}

func (s *Server) handleMesh(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	polls := make([]meshPoll, len(s.config().Nodes))
	var wg sync.WaitGroup
	for i, n := range s.config().Nodes {
		wg.Add(1)
		go func(i int, n config.Node) {
			defer wg.Done()
			polls[i] = pollMesh(ctx, n)
		}(i, n)
	}
	wg.Wait()

	g := buildMesh(polls, nowFunc())

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(g)
}

func pollMesh(ctx context.Context, n config.Node) meshPoll {
	p := meshPoll{node: n, names: map[string]string{}}
	c := stclient.New(n.URL, n.APIKey)

	st, err := c.SystemStatus(ctx)
	if err != nil {
		return p // node down: myID empty, online false
	}
	p.myID = st.MyID
	p.online = true

	if errs, err := c.SystemErrors(ctx); err == nil && len(errs) > 0 {
		p.hasErr = true
	}
	if cfg, err := c.Config(ctx); err == nil {
		for _, d := range cfg.Devices {
			p.names[d.DeviceID] = d.Name
		}
	}
	if conns, err := c.Connections(ctx); err == nil {
		p.conns = conns
	}
	if stats, err := c.DeviceStats(ctx); err == nil {
		p.stats = stats
	}
	return p
}

// buildMesh merges the per-node polls into one undirected graph. Split out from
// the handler so it is unit-testable without HTTP or live nodes.
func buildMesh(polls []meshPoll, now time.Time) meshGraph {
	g := meshGraph{GeneratedAt: now}

	// managed IDs and a global name book (best name for any device).
	managed := map[string]string{} // ID -> managed node name
	for _, p := range polls {
		if p.myID != "" {
			managed[p.myID] = p.node.Name
		}
	}
	nameOf := func(id string) string {
		if n, ok := managed[id]; ok {
			return n
		}
		for _, p := range polls {
			if n, ok := p.names[id]; ok && n != "" {
				return n
			}
		}
		return short(id)
	}

	// Nodes: every managed node, plus every peer any managed node knows.
	nodes := map[string]*meshNode{}
	touch := func(id string) *meshNode {
		if id == "" {
			return nil
		}
		mn := nodes[id]
		if mn == nil {
			_, isManaged := managed[id]
			mn = &meshNode{ID: id, Name: nameOf(id), Managed: isManaged}
			nodes[id] = mn
		}
		return mn
	}
	for _, p := range polls {
		if p.myID != "" {
			mn := touch(p.myID)
			mn.Online = p.online
			mn.Error = p.hasErr
		}
		for id := range p.names {
			touch(id)
		}
		// last-seen (newest across nodes) and live-online for peers
		for id, c := range p.conns {
			mn := touch(id)
			if mn == nil {
				continue
			}
			if c.Connected {
				mn.Online = true
			}
		}
		for id, stat := range p.stats {
			mn := touch(id)
			if mn == nil {
				continue
			}
			if t := newestSeen(mn.LastSeen, stat.LastSeen); t != "" {
				mn.LastSeen = t
			}
		}
	}

	// Edges: one undirected link per configured pair, keyed by ordered ids.
	edges := map[string]*meshEdge{}
	key := func(a, b string) (string, string, string) {
		if a > b {
			a, b = b, a
		}
		return a + "\x00" + b, a, b
	}
	for _, p := range polls {
		if p.myID == "" {
			continue
		}
		for id := range p.names { // configured peers of this managed node
			if id == p.myID {
				continue
			}
			k, a, b := key(p.myID, id)
			e := edges[k]
			if e == nil {
				e = &meshEdge{A: a, B: b}
				edges[k] = e
			}
			if c, ok := p.conns[id]; ok && c.Connected {
				e.Connected = true
				e.Type = c.Type
				e.Addr = c.Address
			}
		}
	}

	for _, mn := range nodes {
		g.Nodes = append(g.Nodes, *mn)
	}
	for _, e := range edges {
		g.Edges = append(g.Edges, *e)
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Name < g.Nodes[j].Name })
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].A != g.Edges[j].A {
			return g.Edges[i].A < g.Edges[j].A
		}
		return g.Edges[i].B < g.Edges[j].B
	})
	return g
}

// newestSeen returns whichever of the two RFC3339 timestamps is later, ignoring
// syncthing's never-seen sentinels (year 1 / the Unix epoch). Empty if neither
// is a real sighting.
func newestSeen(cur, next string) string {
	pick := func(s string) (time.Time, bool) {
		if s == "" {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil || t.Year() < 2010 {
			return time.Time{}, false
		}
		return t, true
	}
	ct, cok := pick(cur)
	nt, nok := pick(next)
	switch {
	case cok && nok:
		if nt.After(ct) {
			return next
		}
		return cur
	case nok:
		return next
	case cok:
		return cur
	default:
		return ""
	}
}

// short abbreviates a device ID to its first block, matching the CLI/UI style.
func short(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}
