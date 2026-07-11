package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/symunona/syncthing-dashboard/internal/provision"
)

// cmdBootstrap — stages 0-2. The probe is read-only; nothing changes on the box
// until you approve a step. Stages 3-5 (syncthing install, swarm join, folder
// adoption) land next.
func cmdBootstrap(args []string) {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	cfgPath := fs.String("config", "swarm.yaml", "swarm.yaml path")
	noBench := fs.Bool("no-bench", false, "skip the sha256 benchmark (loses the initial-scan ETA)")
	dryRun := fs.Bool("dry-run", false, "probe and print the plan, then stop — change nothing")
	yes := fs.Bool("yes", false, "skip prompts for steps that cannot lock you out (ufw still asks)")
	fs.Parse(args)
	_ = cfgPath // stages 4-5

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "bootstrap needs an ssh destination, e.g. `stc bootstrap rue`")
		os.Exit(2)
	}
	dest := fs.Arg(0)
	// flag stops parsing at the first positional, so `bootstrap rue -dry-run`
	// would silently DROP -dry-run and go straight to applying. Parse what
	// follows the destination too, so flags work on either side of it.
	fs.Parse(fs.Args()[1:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	ssh, err := provision.NewSSH(dest)
	die(err)
	defer ssh.Close()

	// --- stage 0: probe (read-only, no sudo) --------------------------------
	r := provision.NewRenderer(os.Stdout)
	fmt.Printf("\n  probing %s — read-only, no sudo, nothing written to the box\n\n", dest)

	t0 := time.Now()
	p, err := provision.RunProbe(ctx, ssh, provision.ProbeOpts{Hash: !*noBench}, r.Event)
	die(err)

	// --- stage 1: report -----------------------------------------------------
	r.Summary(p)
	fmt.Printf("  probe took %.1fs\n", time.Since(t0).Seconds())

	// --- stage 2: plan -------------------------------------------------------
	steps, done := provision.Plan(p, p.Box.User)
	findings := provision.Findings(p)

	fmt.Printf("\n  ── plan for %s ──\n\n", dest)
	for _, d := range done {
		fmt.Printf("  ✓ %s\n", d)
	}
	if len(done) > 0 && len(steps) > 0 {
		fmt.Println()
	}
	if len(steps) == 0 {
		fmt.Println("  nothing to do — this box is already provisioned.")
	}
	for i, s := range steps {
		fmt.Printf("  %d. %s\n", i+1, s.Title)
		fmt.Printf("     %s\n", strings.ReplaceAll(s.Why, "\n    ", "\n     "))
		for _, c := range s.Cmds {
			fmt.Printf("       $ %s\n", c)
		}
		fmt.Println()
	}
	if len(findings) > 0 {
		fmt.Println("  reported, NOT changed (the wizard never edits the ssh access path):")
		for _, f := range findings {
			fmt.Printf("  • %s\n", f)
		}
		fmt.Println()
	}

	if *dryRun || len(steps) == 0 {
		return
	}

	// --- stage 2: apply ------------------------------------------------------
	fmt.Println("  applying — sudo will prompt in this terminal; the password never")
	fmt.Println("  touches this program, a config file, or a log.")
	fmt.Println()

	in := bufio.NewReader(os.Stdin)
	applied, skipped := 0, 0
	for i, s := range steps {
		fmt.Printf("  [%d/%d] %s\n", i+1, len(steps), s.Title)
		if s.Warn != "" {
			fmt.Printf("     ⚠ %s\n", s.Warn)
		}
		if !confirm(in, s, *yes) {
			fmt.Println("     skipped.")
			skipped++
			continue
		}
		if err := provision.Apply(ctx, ssh, s, os.Stdout); err != nil {
			fmt.Printf("     ✗ %v\n", err)
			fmt.Println("     stopping. The box is the state — fix the cause and re-run;")
			fmt.Println("     the probe will pick up from wherever reality actually is.")
			os.Exit(1)
		}
		fmt.Println("     ✓ done")
		applied++
	}

	fmt.Printf("\n  applied %d, skipped %d. Re-probing to verify…\n", applied, skipped)
	remaining, err := provision.Verify(ctx, ssh, p.Box.User)
	die(err)
	if len(remaining) == 0 {
		fmt.Println("  ✓ verified: nothing left to do.")
		return
	}
	fmt.Printf("  %d step(s) still outstanding:\n", len(remaining))
	for _, s := range remaining {
		fmt.Printf("  • %s\n", s.Title)
	}
}

// confirm gates a step. Steps that can sever your own access demand a typed word
// rather than a keystroke — and -yes deliberately does NOT bypass those.
func confirm(in *bufio.Reader, s provision.Step, yes bool) bool {
	if s.Confirm != "" {
		fmt.Printf("     type %q to proceed (anything else skips): ", s.Confirm)
		line, _ := in.ReadString('\n')
		return strings.TrimSpace(line) == s.Confirm
	}
	if yes {
		return true
	}
	fmt.Print("     apply? [y/N]: ")
	line, _ := in.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}
