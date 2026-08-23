// stc remesh — repair path for folders shared before mesh-by-default
// existed, or shared with `-pairwise`. It applies the same union-of-devices
// fix `share` now does automatically, but ONLY to folders that already exist
// somewhere: it never creates a folder on a node that doesn't have it, and
// never removes anything. See internal/sharing/mesh.go for the incident this
// is repairing (two always-on servers stuck routing through a laptop that's
// not always on).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/sharing"
)

func cmdRemesh(args []string) {
	fs := flag.NewFlagSet("remesh", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	all := fs.Bool("all", false, "remesh every folder found anywhere in the swarm")
	fs.Parse(args)

	if *all && fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "remesh -all takes no <folder> argument")
		os.Exit(2)
	}
	if !*all && fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "remesh needs exactly one <folder> (or -all)")
		os.Exit(2)
	}

	cfg := load(*cfgPath)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if *all {
		results, discoveryFailed := sharing.RemeshAll(ctx, cfg)
		printed := false

		failedNodes := make([]string, 0, len(discoveryFailed))
		for n := range discoveryFailed {
			failedNodes = append(failedNodes, n)
		}
		sort.Strings(failedNodes)
		for _, n := range failedNodes {
			fmt.Printf("could not list folders on %s: %s (its folders may be missing from this sweep)\n", n, discoveryFailed[n])
			printed = true
		}

		if len(results) == 0 {
			if !printed {
				fmt.Println("no folders found anywhere in the swarm")
			}
			return
		}
		ids := make([]string, 0, len(results))
		for id := range results {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if printRemesh(results[id]) {
				printed = true
			}
		}
		if !printed {
			fmt.Println("already meshed: every folder's device list already matches on every node that has it")
		}
		return
	}

	res, err := sharing.Remesh(ctx, cfg, fs.Arg(0))
	die(err)
	if !printRemesh(res) {
		fmt.Printf("%q already meshed: same device list on every node that has it\n", res.Folder)
	}
}

// printRemesh prints one folder's outcome — only the nodes that changed or
// failed, nothing for the (usual, quiet) case where a node already matched.
// Returns whether it printed anything, so callers sweeping many folders (-all)
// can tell "nothing to report anywhere" from "reported per folder already".
func printRemesh(res *sharing.RemeshResult) bool {
	printed := false

	names := make([]string, 0, len(res.Added))
	for n := range res.Added {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("%s: %s +%d device(s)\n", res.Folder, n, len(res.Added[n]))
		printed = true
	}

	failed := make([]string, 0, len(res.Failed))
	for n := range res.Failed {
		failed = append(failed, n)
	}
	sort.Strings(failed)
	for _, n := range failed {
		fmt.Printf("%s: %s unreachable: %s\n", res.Folder, n, res.Failed[n])
		printed = true
	}

	return printed
}
