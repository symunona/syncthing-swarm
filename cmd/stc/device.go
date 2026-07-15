// stc device — inventory and membership of the swarm's devices.
//
//	stc device list                              what devices exist, last seen, gaps
//	stc device add    <id|name> [name] [scope]   teach node(s) a device
//	stc device remove <id|name>        [scope]   forget a device on node(s)
//
// "the mesh" here is the set of nodes we hold keys for (swarm.yaml). A device is
// only reachable from a node that carries it in config, so add/remove is per
// node. Scope defaults to the hub (the local/127.0.0.1 node); -all fans out to
// every managed node, -on NODE[,NODE] targets specific ones. Hub-and-spoke by
// default, full mesh only when you ask — deliberately, since everything-to-
// everything is the topology that breeds conflict and rescan storms.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

const yearDays = 365 // last-seen past this many days is shown red

func cmdDevice(args []string) {
	if len(args) == 0 {
		deviceUsage()
	}
	switch args[0] {
	case "list", "ls":
		deviceList(args[1:])
	case "add":
		deviceAdd(args[1:])
	case "remove", "rm":
		deviceRemove(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "device: unknown action %q\n\n", args[0])
		deviceUsage()
	}
}

func deviceUsage() {
	fmt.Fprint(os.Stderr, `stc device — swarm device inventory & membership

  stc device list                                  [-config FILE]
  stc device add    <id|name> [name] [-on N|-all]  [-config FILE]
  stc device remove <id|name>        [-on N|-all]  [-config FILE]

  <id|name>   full device ID, or the name of a device the swarm already knows
  [name]      label for a brand-new device (required if the swarm can't supply one)
  -on         comma-separated node names to act on (default: the hub node)
  -all        act on every managed node
  add/remove touch device config only; folder sharing stays `+"`stc share`"+`.
`)
	os.Exit(2)
}

// --- list ---

func deviceList(args []string) {
	fs := flag.NewFlagSet("device list", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	fs.Parse(args)
	cfg := load(*cfgPath)

	views := gatherAll(cfg)

	// Roll every node's view up into one row per device ID.
	type row struct {
		id       string
		name     string
		online   bool
		lastSeen time.Time
		hasNever bool     // some node reports a zero last-seen
		on       []string // managed node names that carry this device
	}
	rows := map[string]*row{}
	get := func(id string) *row {
		r := rows[id]
		if r == nil {
			r = &row{id: id}
			rows[id] = r
		}
		return r
	}
	for _, v := range views {
		if v.err != nil {
			continue
		}
		for id, nm := range v.names {
			if id == v.myID {
				continue // a node's own entry is not a peer device
			}
			r := get(id)
			if r.name == "" && nm != "" {
				r.name = nm
			}
			r.on = append(r.on, v.node.Name)
			if c, ok := v.conns[id]; ok && c.Connected {
				r.online = true
			}
			if t, ok := lastSeen(v.stats[id]); ok {
				if t.After(r.lastSeen) {
					r.lastSeen = t
				}
			} else {
				r.hasNever = true
			}
		}
	}

	// For "missing" we only count nodes we could actually read (an unreachable
	// node isn't missing a device — we just don't know), and we never fault a
	// node for lacking its OWN device (a node never lists itself as a peer).
	var reachable []string
	home := map[string]string{} // device ID -> the managed node it IS
	for _, v := range views {
		if v.err != nil {
			continue
		}
		reachable = append(reachable, v.node.Name)
		if v.myID != "" {
			home[v.myID] = v.node.Name
		}
	}

	ordered := make([]*row, 0, len(rows))
	for _, r := range rows {
		ordered = append(ordered, r)
	}
	// Online first, then most-recently-seen, then name.
	sort.Slice(ordered, func(a, b int) bool {
		if ordered[a].online != ordered[b].online {
			return ordered[a].online
		}
		if !ordered[a].lastSeen.Equal(ordered[b].lastSeen) {
			return ordered[a].lastSeen.After(ordered[b].lastSeen)
		}
		return ordered[a].name < ordered[b].name
	})

	fmt.Printf("device inventory — %d devices across %d managed nodes (%d reachable)\n\n", len(ordered), len(cfg.Nodes), len(reachable))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	var suggestions []string
	now := time.Now()
	for _, r := range ordered {
		dot := "○"
		if r.online {
			dot = "●"
		}
		name := r.name
		if name == "" {
			name = "?"
		}

		seen := "never"
		if !r.lastSeen.IsZero() {
			seen = humanAgo(now.Sub(r.lastSeen))
		}
		if r.online {
			seen = "now"
		} else if !r.lastSeen.IsZero() && now.Sub(r.lastSeen) > yearDays*24*time.Hour {
			seen = colr(seen, red)
		}

		have := r.on
		if h := home[r.id]; h != "" {
			have = append(append([]string{}, r.on...), h) // don't fault a node for lacking itself
		}
		missing := missingNodes(reachable, have)
		gap := ""
		if len(missing) > 0 {
			gap = colr("missing: "+strings.Join(missing, " "), yellow)
			suggestions = append(suggestions,
				fmt.Sprintf("stc device add %s %s -on %s", short(r.id), quoteName(name), strings.Join(missing, ",")))
		}
		fmt.Fprintf(w, "  %s %s\t%s\t%s\t%s\n", dot, name, short(r.id), seen, gap)
	}
	w.Flush()

	if len(suggestions) > 0 {
		fmt.Printf("\n%d device(s) not on every managed node. To close the gaps:\n", len(suggestions))
		for _, s := range suggestions {
			fmt.Println("  " + s)
		}
		fmt.Println("  (or -all to add everywhere; hub-only if you drop -on)")
	}
}

// --- add ---

func deviceAdd(args []string) {
	fs := flag.NewFlagSet("device add", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	on := fs.String("on", "", "comma-separated node names (default: hub)")
	all := fs.Bool("all", false, "add to every managed node")
	pos := parseInterspersed(fs, args)
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "device add needs <id|name> [name]")
		os.Exit(2)
	}
	cfg := load(*cfgPath)
	views := gatherAll(cfg)

	id, knownName := resolveID(views, pos[0])
	name := knownName
	if len(pos) >= 2 {
		name = pos[1] // explicit name wins
	}
	if !looksLikeID(id) {
		die(fmt.Errorf("%q is not a known device name and not a device ID — paste the full ID to add a new device", pos[0]))
	}
	if name == "" {
		die(fmt.Errorf("device %s is new to the swarm — give it a name: stc device add %s <name>", short(id), short(id)))
	}

	nodes := targets(cfg, *on, *all)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, n := range nodes {
		cl := stclient.New(n.URL, n.APIKey)
		if has, err := cl.HasDevice(ctx, id); err != nil {
			fmt.Printf("  ~ %s  unreachable: %v\n", n.Name, err)
			continue
		} else if has {
			fmt.Printf("  = %s  already had it\n", n.Name)
			continue
		}
		d := stclient.Device{
			"deviceID":  id,
			"name":      name,
			"addresses": []any{"dynamic"},
		}
		if err := cl.PutDevice(ctx, d); err != nil {
			fmt.Printf("  ! %s  add failed: %v\n", n.Name, err)
			continue
		}
		fmt.Printf("  + %s  added %q (%s)\n", n.Name, name, short(id))
	}
}

// --- remove ---

func deviceRemove(args []string) {
	fs := flag.NewFlagSet("device remove", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	on := fs.String("on", "", "comma-separated node names (default: hub)")
	all := fs.Bool("all", false, "remove from every managed node")
	pos := parseInterspersed(fs, args)
	if len(pos) < 1 {
		fmt.Fprintln(os.Stderr, "device remove needs <id|name>")
		os.Exit(2)
	}
	cfg := load(*cfgPath)
	views := gatherAll(cfg)

	id, name := resolveID(views, pos[0])
	if !looksLikeID(id) {
		die(fmt.Errorf("%q matched no known device — pass a name the swarm knows or the full device ID", pos[0]))
	}
	if name == "" {
		name = "?"
	}

	nodes := targets(cfg, *on, *all)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, n := range nodes {
		cl := stclient.New(n.URL, n.APIKey)
		if has, err := cl.HasDevice(ctx, id); err != nil {
			fmt.Printf("  ~ %s  unreachable: %v\n", n.Name, err)
			continue
		} else if !has {
			fmt.Printf("  = %s  didn't have it\n", n.Name)
			continue
		}
		if err := cl.DeleteDevice(ctx, id); err != nil {
			// Syncthing refuses while the device still shares a folder.
			fmt.Printf("  ! %s  remove failed: %v\n", n.Name, err)
			continue
		}
		fmt.Printf("  - %s  removed %q (%s)\n", n.Name, name, short(id))
	}
}

// --- shared helpers ---

// parseInterspersed parses a flag set that has positionals mixed in with flags,
// e.g. `add STDRFF2 xayah -on fiona`. Go's flag package stops at the first
// non-flag, so it would otherwise silently drop `-on`. We peel positionals off
// one at a time, re-parsing the remainder, until nothing is left.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for {
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return pos
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// gatherAll polls every node once, sequentially. Reused by list/add/remove so
// they all see the same picture (names, membership, stats).
func gatherAll(cfg *config.Config) []nodeView {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	views := make([]nodeView, 0, len(cfg.Nodes))
	for i := range cfg.Nodes {
		views = append(views, gather(ctx, &cfg.Nodes[i]))
	}
	return views
}

// resolveID turns a user argument into a device ID + best-known name. A full
// device ID passes through; otherwise it's treated as a name and matched
// (case-insensitively) against every device the swarm knows.
func resolveID(views []nodeView, arg string) (id, name string) {
	if looksLikeID(arg) {
		id = normalizeID(arg)
		// still try to recover a name for nicer output
		for _, v := range views {
			if n, ok := v.names[id]; ok && n != "" {
				return id, n
			}
		}
		return id, ""
	}
	// name match
	var hits []string
	seen := map[string]bool{}
	nameOf := map[string]string{}
	for _, v := range views {
		for did, nm := range v.names {
			if strings.EqualFold(nm, arg) && !seen[did] {
				seen[did] = true
				hits = append(hits, did)
				nameOf[did] = nm
			}
		}
	}
	if len(hits) == 1 {
		return hits[0], nameOf[hits[0]]
	}
	if len(hits) > 1 {
		die(fmt.Errorf("%q is ambiguous — %d devices share that name; use the full device ID", arg, len(hits)))
	}
	return arg, "" // no match: hand back as-is so caller reports "not an ID"
}

// targets resolves the scope flags to the nodes to act on. Default is the hub.
func targets(cfg *config.Config, on string, all bool) []*config.Node {
	if all {
		out := make([]*config.Node, len(cfg.Nodes))
		for i := range cfg.Nodes {
			out[i] = &cfg.Nodes[i]
		}
		return out
	}
	if on != "" {
		var out []*config.Node
		for nm := range strings.SplitSeq(on, ",") {
			nm = strings.TrimSpace(nm)
			n := cfg.Node(nm)
			if n == nil {
				die(fmt.Errorf("no node named %q in swarm.yaml", nm))
			}
			out = append(out, n)
		}
		return out
	}
	hub := cfg.LocalNode()
	if hub == nil {
		die(fmt.Errorf("no hub node found (mark one local: true, or pass -on)"))
	}
	return []*config.Node{hub}
}

func missingNodes(all, have []string) []string {
	has := map[string]bool{}
	for _, h := range have {
		has[h] = true
	}
	var out []string
	for _, n := range all {
		if !has[n] {
			out = append(out, n)
		}
	}
	return out
}

// lastSeen parses a DeviceStat's timestamp; ok=false for never-seen / unparseable.
func lastSeen(s stclient.DeviceStat) (time.Time, bool) {
	if s.LastSeen == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s.LastSeen)
	// syncthing renders "never seen" inconsistently — year 1 (0001-01-01) on some
	// versions, the Unix epoch (1970) on others. Anything from before syncthing
	// existed is never, not a real sighting.
	if err != nil || t.Year() < 2010 {
		return time.Time{}, false
	}
	return t, true
}

// humanAgo renders a duration as a compact "3d ago" / "5h ago" / "now".
func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// looksLikeID reports whether s is plausibly a syncthing device ID (7 groups of
// 7 chars, dashes optional). Cheap structural check, not a checksum.
func looksLikeID(s string) bool {
	stripped := strings.ReplaceAll(strings.ToUpper(s), "-", "")
	return len(stripped) == 56
}

func normalizeID(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

func quoteName(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// --- color ---

const (
	red    = "31"
	yellow = "33"
)

func colr(s, code string) string {
	if !isTTY() {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
