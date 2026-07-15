// stc list devices — the swarm connection graph.
//
// swarm.yaml names the nodes we HOLD KEYS FOR (fiona, taskbot, …). But each of
// those nodes may know devices we do not manage, and — the point of this command
// — a device can be powered on yet linked to nobody. There is no single API that
// answers "what is everything talking to what"; syncthing only tells one node
// about its own peers. So we ask every node we can reach and stitch the answers
// together into one picture.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

func cmdList(args []string) {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	fs.Parse(args)

	switch fs.Arg(0) {
	case "devices", "":
		listDevices(load(*cfgPath))
	default:
		fmt.Fprintf(os.Stderr, "list: unknown subject %q (only \"devices\")\n", fs.Arg(0))
		os.Exit(2)
	}
}

// nodeView is what one managed node reports about itself and its peers.
type nodeView struct {
	node  *config.Node
	myID  string                         // this node's own device ID
	names map[string]string              // device ID -> name from this node's config
	conns map[string]stclient.Connection // device ID -> live connection state
	stats map[string]stclient.DeviceStat // device ID -> last-seen stats
	err   error                          // set if the node was unreachable
}

func listDevices(cfg *config.Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Gather every node's view. Sequential is fine: a handful of nodes, each
	// capped at the client's 8s timeout.
	views := make([]nodeView, 0, len(cfg.Nodes))
	for i := range cfg.Nodes {
		views = append(views, gather(ctx, &cfg.Nodes[i]))
	}

	// Managed IDs: the device IDs of nodes we hold keys for, so we can label a
	// peer as "one of ours" vs an outside device.
	managed := map[string]string{} // ID -> managed node name
	for _, v := range views {
		if v.myID != "" {
			managed[v.myID] = v.node.Name
		}
	}

	// Global name book: best display name for any ID seen anywhere.
	name := func(id string) string {
		if n, ok := managed[id]; ok {
			return n
		}
		for _, v := range views {
			if n, ok := v.names[id]; ok && n != "" {
				return n
			}
		}
		return "?"
	}

	// Per-node detail: what each of our nodes sees right now.
	for _, v := range views {
		if v.err != nil {
			fmt.Printf("%s  (%s)\n  UNREACHABLE: %v\n\n", v.node.Name, v.node.URL, v.err)
			continue
		}
		online := 0
		for id, c := range v.conns {
			if id != v.myID && c.Connected {
				online++
			}
		}
		fmt.Printf("%s  %s  %d/%d peers online\n", v.node.Name, short(v.myID), online, len(v.names)-1)

		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		ids := make([]string, 0, len(v.names))
		for id := range v.names {
			if id != v.myID {
				ids = append(ids, id)
			}
		}
		sort.Slice(ids, func(a, b int) bool { return name(ids[a]) < name(ids[b]) })
		for _, id := range ids {
			c := v.conns[id]
			dot, state := "○", "offline"
			if c.Paused {
				dot, state = "‖", "paused"
			} else if c.Connected {
				dot, state = "●", "ONLINE"
			}
			addr := c.Address
			if addr != "" {
				addr = c.Type + " " + addr
			}
			tag := ""
			if _, ours := managed[id]; !ours {
				tag = "  (outside device)"
			}
			fmt.Fprintf(w, "  %s %s\t%s\t%s\t%s%s\n", dot, name(id), short(id), state, addr, tag)
		}
		w.Flush()
		fmt.Println()
	}

	summary(views, managed, name)
}

func gather(ctx context.Context, n *config.Node) nodeView {
	v := nodeView{node: n, names: map[string]string{}}
	cl := stclient.New(n.URL, n.APIKey)

	st, err := cl.SystemStatus(ctx)
	if err != nil {
		v.err = err
		return v
	}
	v.myID = st.MyID

	cfg, err := cl.Config(ctx)
	if err != nil {
		v.err = err
		return v
	}
	for _, d := range cfg.Devices {
		v.names[d.DeviceID] = d.Name
	}
	conns, err := cl.Connections(ctx)
	if err != nil {
		v.err = err
		return v
	}
	v.conns = conns
	// Stats are a nice-to-have (last-seen); a node that answered everything else
	// but lacks stats should still show up, so don't fail the view over them.
	if stats, err := cl.DeviceStats(ctx); err == nil {
		v.stats = stats
	}
	return v
}

// summary flags the two things you actually came here to learn: managed nodes
// nobody can currently reach, and devices that are configured somewhere yet
// online nowhere (a box that is powered on but not linked into the swarm).
func summary(views []nodeView, managed map[string]string, name func(string) string) {
	// reachable[id] = at least one node has a live connection to it.
	reachable := map[string]bool{}
	// known collects every non-self device ID seen in any node's config.
	known := map[string]bool{}
	for _, v := range views {
		for id, c := range v.conns {
			if c.Connected {
				reachable[id] = true
			}
		}
		for id := range v.names {
			known[id] = true
		}
	}

	var orphans, unreachableMine []string
	for id := range known {
		if reachable[id] {
			continue
		}
		// self IDs live in `known` too; a node that reported OK is obviously up.
		if n, ours := managed[id]; ours {
			// It's one of ours and offline everywhere → only worth flagging if
			// its own node also failed to answer.
			for _, v := range views {
				if v.myID == id && v.err != nil {
					unreachableMine = append(unreachableMine, n)
				}
			}
			continue
		}
		orphans = append(orphans, name(id)+" "+short(id))
	}
	sort.Strings(orphans)
	sort.Strings(unreachableMine)

	fmt.Println("summary")
	if len(orphans) == 0 && len(unreachableMine) == 0 {
		fmt.Println("  every configured device is online for at least one node.")
		return
	}
	for _, o := range orphans {
		fmt.Printf("  ⚠ %s — configured but online for no node (powered off, or not linked)\n", o)
	}
	for _, m := range unreachableMine {
		fmt.Printf("  ⚠ %s — managed node did not answer its own API\n", m)
	}
}

// short renders a device ID as its first block (7 chars), how the syncthing UI
// abbreviates them. Empty stays empty.
func short(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}
