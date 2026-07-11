package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/provision"
)

// cmdBootstrap — stage 0 (probe) + stage 1 (report). Read-only: it cannot change
// the box. The hardening plan, syncthing install, swarm join and folder adoption
// stages gate on this and land next.
func cmdBootstrap(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	noBench := fs.Bool("no-bench", false, "skip the sha256 benchmark (loses the initial-scan ETA)")
	fs.Parse(args)
	_ = cfgPath // stages 4-5 need it

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "bootstrap needs an ssh destination, e.g. `stc bootstrap rue`")
		os.Exit(2)
	}
	dest := fs.Arg(0)

	// Ctrl-C cancels cleanly rather than leaving an ssh master socket behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ssh, err := provision.NewSSH(dest)
	die(err)
	defer ssh.Close()

	r := provision.NewRenderer(os.Stdout)
	fmt.Printf("\n  probing %s — read-only, no sudo, nothing written to the box\n\n", dest)

	t0 := time.Now()
	p, err := provision.RunProbe(ctx, ssh, provision.ProbeOpts{Hash: !*noBench}, r.Event)
	die(err)
	r.Summary(p)
	fmt.Printf("  probe took %.1fs\n\n", time.Since(t0).Seconds())
}
