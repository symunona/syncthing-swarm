// stc — syncthing-swarm CLI. Share and unshare folders across the swarm using
// the same swarm.yaml + sharing logic as the dashboard.
//
//	stc share   <folder> <target> [-path DIR] [-from NODE] [-config swarm.yaml]
//	stc unshare <folder> <target>            [-from NODE] [-config swarm.yaml]
//
// <folder> is a folder id or label (resolved on the source node).
// Unshare removes the share and deletes the folder from the target config;
// it never deletes files on disk. No confirmation prompt (scriptable).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/config"
	"github.com/symunona/syncthing-dashboard/internal/sharing"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "share":
		cmdShare(os.Args[2:])
	case "unshare":
		cmdUnshare(os.Args[2:])
	case "bootstrap":
		cmdBootstrap(os.Args[2:])
	case "adopt":
		cmdAdopt(os.Args[2:])
	case "list":
		cmdList(os.Args[2:])
	case "device":
		cmdDevice(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `stc — syncthing-swarm folder sharing

usage:
  stc share     <folder> <target> [-path DIR] [-from NODE] [-config FILE]
  stc unshare   <folder> <target>            [-from NODE] [-config FILE]
  stc bootstrap <ssh-dest>                   [-no-bench]  [-config FILE]
  stc list      devices                                   [-config FILE]
  stc device    list | add <id> [name] | remove <id>      [-on N|-all] [-config FILE]

  <folder>    folder id or label (looked up on the source node)
  <target>    node name to share to / unshare from
  <ssh-dest>  ssh destination of a new node, e.g. rue (uses your ~/.ssh/config)
  -from       source node (default: the local/127.0.0.1 node in swarm.yaml)
  -path       target dir for the new folder (default: <target root>/<label>)
  -no-bench   bootstrap: skip the sha256 benchmark (loses the initial-scan ETA)
  -config     swarm.yaml path (default: swarm.yaml)

unshare never deletes files on disk. No confirmation prompt.
bootstrap surveys the box read-only first and changes nothing without a prompt.
`)
	os.Exit(2)
}

func cmdShare(args []string) {
	fs := flag.NewFlagSet("share", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	from := fs.String("from", "", "source node")
	path := fs.String("path", "", "target dir override")
	fs.Parse(args)
	folder, target := two(fs, "share")

	cfg := load(*cfgPath)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	res, err := sharing.Share(ctx, cfg, folder, *from, target, *path)
	die(err)
	fmt.Printf("shared %q: %s -> %s at %s\n", res.Folder, res.Source, res.Target, res.TargetPath)
	if res.Note != "" {
		fmt.Printf("  note: %s\n", res.Note)
	}
}

func cmdUnshare(args []string) {
	fs := flag.NewFlagSet("unshare", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	from := fs.String("from", "", "source node")
	fs.Parse(args)
	folder, target := two(fs, "unshare")

	cfg := load(*cfgPath)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	res, err := sharing.Unshare(ctx, cfg, folder, *from, target)
	die(err)
	fmt.Printf("unshared %q from %s (%s)\n", res.Folder, res.Target, res.Note)
}

func two(fs *flag.FlagSet, cmd string) (folder, target string) {
	if fs.NArg() < 2 {
		fmt.Fprintf(os.Stderr, "%s needs <folder> <target>\n", cmd)
		os.Exit(2)
	}
	return fs.Arg(0), fs.Arg(1)
}

func load(path string) *config.Config {
	cfg, err := config.Load(path)
	die(err)
	return cfg
}

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
