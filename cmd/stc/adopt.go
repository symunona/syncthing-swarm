package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/provision"
	"github.com/symunona/syncthing-dashboard/internal/stclient"
)

// cmdAdopt — stage 5. Find the syncthing folders already sitting on a node's
// drive, work out which swarm folders they are, and re-attach them.
//
//	stc adopt <node> [-dry-run] [-type sendreceive|receiveonly] [-rescan SECS]
//
// The node must already be in swarm.yaml (i.e. `stc bootstrap -syncthing` ran).
// -dry-run shows the matches and writes nothing.
func cmdAdopt(args []string) {
	fs := flag.NewFlagSet("adopt", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	dryRun := fs.Bool("dry-run", false, "show what matches what, change nothing")
	ftype := fs.String("type", "", "folder type on this node: sendreceive | receiveonly (default: ask per folder)")
	rescan := fs.Int("rescan", 604800, "rescanIntervalS for adopted folders (0 disables periodic scanning)")
	yes := fs.Bool("yes", false, "adopt every exact match without asking")
	idsFrom := fs.String("ids-from", "", "a salvaged config.xml: recovers the folder IDs of ORPHANS "+
		"(folders that exist on this disk and nowhere else in the swarm), so a peer that "+
		"returns later re-links instead of silently duplicating them")
	fs.Parse(args)
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "adopt needs a node name from swarm.yaml, e.g. `stc adopt rue`")
		os.Exit(2)
	}
	name := fs.Arg(0)
	fs.Parse(fs.Args()[1:])

	cfg := load(*cfgPath)
	node := cfg.Node(name)
	if node == nil {
		die(fmt.Errorf("no node %q in %s (run `stc bootstrap %s -syncthing` first)", name, *cfgPath, name))
	}
	if node.SSH == "" {
		die(fmt.Errorf("node %q has no ssh: in swarm.yaml — the drive scan needs it", name))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ssh, err := provision.NewSSH(node.SSH)
	die(err)
	defer ssh.Close()

	// what is on the drive
	fmt.Printf("\n  scanning %s for existing syncthing folders…\n", name)
	p, err := provision.RunProbe(ctx, ssh, provision.ProbeOpts{}, nil)
	die(err)
	if len(p.StFolders) == 0 {
		fmt.Println("  no .stfolder markers on the drive — nothing to adopt.")
		return
	}

	// what the swarm knows. The swarm is the ID authority: the folder ID is NOT
	// on disk (.stfolder is a bare mount marker), so these are the only place the
	// IDs still exist.
	swarm, err := provision.SwarmFolders(ctx, cfg, name)
	die(err)

	cands, err := provision.Match(ctx, cfg, ssh, p.StFolders, swarm, name)
	die(err)

	newID, err := deviceIDOf(ctx, node)
	die(err)

	fmt.Printf("\n  %d directories on %s, matched against %d folders the swarm knows.\n\n",
		len(cands), node.Root, len(swarm))
	fmt.Println("  Two independent signals: the directory NAME against the folder label, and the")
	fmt.Println("  folder's top-level CONTENT from the swarm's index against what is on the disk.")
	fmt.Println("  Names alone are not enough — and a name that matches while the content does")
	fmt.Println("  not is the one genuinely destructive case, so it is never a default.")
	fmt.Println()

	// Orphans: folders on this disk that exist NOWHERE ELSE in the swarm, so the
	// swarm cannot supply their ID. A salvaged config.xml can.
	var recovered map[string]provision.RecoveredFolder
	if *idsFrom != "" {
		recovered, err = provision.RecoveredIDs(*idsFrom)
		die(err)
		fmt.Printf("  recovered %d folder IDs from %s\n\n", len(recovered), *idsFrom)
	}

	for _, c := range cands {
		printCandidate(c, recovered)
	}

	if *dryRun {
		fmt.Println("\n  dry run — nothing written.")
		return
	}

	in := bufio.NewReader(os.Stdin)
	adopted, skipped := 0, 0
	for _, c := range cands {
		if c.Verdict == provision.VerdictOrphan {
			rec, ok := recovered[strings.ToLower(c.Dir.Name)]
			if !ok {
				skipped++
				continue
			}
			if !adoptOrphan(ctx, in, cfg, *node, newID, c, rec, *rescan, *yes) {
				skipped++
				continue
			}
			adopted++
			continue
		}
		fmt.Printf("\n  %s → folder %q (%s)\n", c.Dir.Name, c.Match.Label, c.Match.ID)

		if !c.Adoptable() {
			fmt.Printf("     ⚠ %s — this is NOT a confident match. Adopting under the wrong\n", c.Verdict)
			fmt.Printf("       folder ID makes syncthing treat these files as foreign and, on\n")
			fmt.Printf("       sendreceive, push them into a folder they do not belong to.\n")
			fmt.Printf("     type the folder id (%s) to adopt anyway, anything else skips: ", c.Match.ID)
			line, _ := in.ReadString('\n')
			if strings.TrimSpace(line) != c.Match.ID {
				fmt.Println("     skipped.")
				skipped++
				continue
			}
		} else if !*yes {
			fmt.Print("     adopt? [y/N]: ")
			line, _ := in.ReadString('\n')
			l := strings.ToLower(strings.TrimSpace(line))
			if l != "y" && l != "yes" {
				fmt.Println("     skipped.")
				skipped++
				continue
			}
		}

		t := *ftype
		if t == "" {
			t = askType(in, c)
		}
		rs := *rescan
		if t == "receiveonly" && rs != 0 {
			fmt.Printf("     periodic rescan every %s. On a receive-only folder nothing should\n", dur(rs))
			fmt.Print("     change locally — disable periodic scanning entirely? [y/N]: ")
			line, _ := in.ReadString('\n')
			if l := strings.ToLower(strings.TrimSpace(line)); l == "y" || l == "yes" {
				rs = 0
			}
		}

		if err := provision.Adopt(ctx, cfg, *node, newID, c, t, rs); err != nil {
			fmt.Printf("     ✗ %v\n", err)
			continue
		}
		fmt.Printf("     ✓ adopted as %s (%s, rescan %s)\n", t, c.Match.ID, dur(rs))
		adopted++
	}

	fmt.Printf("\n  adopted %d, skipped %d.\n", adopted, skipped)
	if adopted > 0 {
		fmt.Println("  Syncthing is now hashing what is on disk. Matching blocks just verify —")
		fmt.Println("  nothing is downloaded. Watch it in the dashboard.")
	}
}

// adoptOrphan configures a folder that lives on this disk and nowhere else in the
// swarm, with NO peers, reusing the ID salvaged from the node's old config.
//
// sendreceive, not receiveonly: this node holds the only live copy, so it has to
// be able to SEND. A receive-only folder would never propagate its contents to a
// peer that comes back.
func adoptOrphan(ctx context.Context, in *bufio.Reader, cfg *config.Config, node config.Node,
	newID string, c provision.Candidate, rec provision.RecoveredFolder, rescan int, yes bool) bool {

	fmt.Printf("\n  %s → orphan folder %q (%s), recovered from the old config\n",
		c.Dir.Name, rec.Label, rec.ID)
	fmt.Println("     No other node in the swarm has this folder, so it gets NO peers.")
	fmt.Println("     Reusing its old ID costs nothing and means a peer that comes back later")
	fmt.Println("     re-links to it, instead of the two becoming permanently separate folders.")

	if !yes {
		fmt.Print("     add it (unshared)? [y/N]: ")
		line, _ := in.ReadString('\n')
		if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" {
			fmt.Println("     skipped.")
			return false
		}
	}
	if err := provision.AdoptOrphan(ctx, node, newID, c.Dir, rec, rescan); err != nil {
		fmt.Printf("     ✗ %v\n", err)
		return false
	}
	fmt.Printf("     ✓ added as sendreceive, no peers (%s, rescan %s)\n", rec.ID, dur(rescan))
	return true
}

func printCandidate(c provision.Candidate, recovered map[string]provision.RecoveredFolder) {
	mark, note := "?", ""
	if c.Verdict == provision.VerdictOrphan {
		if rec, ok := recovered[strings.ToLower(c.Dir.Name)]; ok {
			fmt.Printf("  + %-18s %s  (orphan — nobody else has it; old ID %s recovered, will be added UNSHARED)\n",
				c.Dir.Name, rec.Label, rec.ID)
			return
		}
	}
	switch c.Verdict {
	case provision.VerdictExact:
		mark = "✓"
		note = fmt.Sprintf("%s  (name + content agree, %.0f%% of top-level entries match; on %s)",
			c.Match.Label, c.Score*100, strings.Join(c.Match.Nodes, ", "))
	case provision.VerdictRenamed:
		mark = "~"
		note = fmt.Sprintf("%s  (RENAMED? content agrees %.0f%% but the directory name differs; on %s)",
			c.Match.Label, c.Score*100, strings.Join(c.Match.Nodes, ", "))
	case provision.VerdictNameOnly:
		mark = "⚠"
		note = fmt.Sprintf("%s  (name matches but content agrees only %.0f%% — NOT a safe match)",
			c.Match.Label, c.Score*100)
	case provision.VerdictOrphan:
		mark = "-"
		note = "no folder in the swarm resembles this — left alone"
	}
	fmt.Printf("  %s %-18s %s\n", mark, c.Dir.Name, note)
}

func askType(in *bufio.Reader, c provision.Candidate) string {
	fmt.Println("     folder type on this node:")
	fmt.Println("       sendreceive  (default) — a full peer; edits here propagate out")
	fmt.Println("       receiveonly            — this box can NEVER push a local mistake into")
	fmt.Println("                                the swarm. Right for a box that only holds a")
	fmt.Println("                                copy and that you never edit on.")
	fmt.Print("     [sendreceive]/receiveonly: ")
	line, _ := in.ReadString('\n')
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "r") {
		return "receiveonly"
	}
	return "sendreceive"
}

func dur(secs int) string {
	if secs == 0 {
		return "disabled"
	}
	if secs%86400 == 0 {
		return strconv.Itoa(secs/86400) + "d"
	}
	return strconv.Itoa(secs) + "s"
}

// deviceIDOf asks a node for its own device ID.
func deviceIDOf(ctx context.Context, n *config.Node) (string, error) {
	st, err := stclient.New(n.URL, n.APIKey).SystemStatus(ctx)
	if err != nil {
		return "", fmt.Errorf("%s: %w", n.Name, err)
	}
	return st.MyID, nil
}
