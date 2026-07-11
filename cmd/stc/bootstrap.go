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

	"github.com/symunona/syncthing-dashboard/internal/config"
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
	syncthing := fs.Bool("syncthing", false, "after hardening: install syncthing and join the swarm (shares no folders)")
	node := fs.String("node", "", "node name in swarm.yaml (default: the box's hostname)")
	fs.Parse(args)

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

	in := bufio.NewReader(os.Stdin)

	if *dryRun {
		// Show the syncthing plan too, so a dry run covers the whole flow rather
		// than stopping at the hardening.
		if *syncthing {
			printSyncthingPlan(p, *node)
		}
		return
	}

	// A box that is already hardened must still go on to stage 3 — returning here
	// because "there is nothing to harden" would mean the wizard could never
	// install syncthing on a node it had already prepared.
	if len(steps) == 0 {
		if *syncthing {
			stageSyncthing(ctx, ssh, p, in, *cfgPath, dest, *node, *yes)
		} else {
			fmt.Println("\n  (pass -syncthing to install syncthing and join the swarm)")
		}
		return
	}

	// --- stage 2: apply ------------------------------------------------------
	fmt.Println("  applying — sudo will prompt in this terminal; the password never")
	fmt.Println("  touches this program, a config file, or a log.")
	fmt.Println()

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
	p2, err := provision.RunProbe(ctx, ssh, provision.ProbeOpts{}, nil)
	die(err)
	remaining, _ := provision.Plan(p2, p2.Box.User)
	if len(remaining) > 0 {
		fmt.Printf("  %d step(s) still outstanding — fix these before syncthing:\n", len(remaining))
		for _, s := range remaining {
			fmt.Printf("  • %s\n", s.Title)
		}
		return
	}
	fmt.Println("  ✓ verified: hardening complete.")

	if !*syncthing {
		fmt.Println("\n  (pass -syncthing to install syncthing and join the swarm)")
		return
	}
	stageSyncthing(ctx, ssh, p2, in, *cfgPath, dest, *node, *yes)
}

// layoutFor decides where syncthing's pieces live on this node: config on the
// boot media (identity survives the drive dying), database on the drive (the
// write-heavy part, and SD wear is what kills these boxes).
func layoutFor(p *provision.Probe) (provision.SyncthingLayout, error) {
	drives := p.Drives()
	if len(drives) == 0 {
		return provision.SyncthingLayout{},
			fmt.Errorf("no data drive mounted: syncthing folders would land on the boot media")
	}
	d := drives[0]
	return provision.SyncthingLayout{
		User:      p.Box.User,
		ConfigDir: "/home/" + p.Box.User + "/.config/syncthing",
		DataDir:   d.Mountpoint + "/syncthing-db",
		FolderDir: d.Mountpoint + "/syncthing",
		Mount:     d.Mountpoint,
		TailnetIP: p.Tailscale.IP4,
	}, nil
}

func printSyncthingPlan(p *provision.Probe, nodeName string) {
	l, err := layoutFor(p)
	die(err)
	steps, err := provision.PlanSyncthing(p, l)
	die(err)
	if nodeName == "" {
		nodeName = p.Box.Hostname
	}
	fmt.Printf("\n  ── syncthing on %s ──\n\n", nodeName)
	for i, s := range steps {
		fmt.Printf("  %d. %s\n", i+1, s.Title)
		fmt.Printf("     %s\n", strings.ReplaceAll(s.Why, "\n    ", "\n     "))
		for _, c := range s.Cmds {
			fmt.Printf("       $ %s\n", c)
		}
		fmt.Println()
	}
	fmt.Printf("  then: register %s in swarm.yaml and mesh its device with every node.\n", nodeName)
	fmt.Println("  NO folders are created or shared — that stays a deliberate act.")
}

// stageSyncthing is stages 3 and 4: install syncthing, then register the node in
// swarm.yaml and mesh its device with every other node.
//
// It creates NO folders. Sharing is a separate, deliberate act — done from the
// dashboard, or with `stc share`.
func stageSyncthing(ctx context.Context, ssh *provision.SSH, p *provision.Probe,
	in *bufio.Reader, cfgPath, dest, nodeName string, yes bool) {

	if nodeName == "" {
		nodeName = p.Box.Hostname
	}
	layout, err := layoutFor(p)
	die(err)

	steps, err := provision.PlanSyncthing(p, layout)
	die(err)

	fmt.Printf("\n  ── syncthing on %s ──\n\n", nodeName)
	if len(steps) == 0 {
		fmt.Println("  syncthing already installed and running.")
	}
	for i, s := range steps {
		fmt.Printf("  %d. %s\n", i+1, s.Title)
		fmt.Printf("     %s\n", strings.ReplaceAll(s.Why, "\n    ", "\n     "))
		for _, c := range s.Cmds {
			fmt.Printf("       $ %s\n", c)
		}
		fmt.Println()
	}
	for i, s := range steps {
		fmt.Printf("  [%d/%d] %s\n", i+1, len(steps), s.Title)
		if !confirm(in, s, yes) {
			fmt.Println("     skipped — stopping (the later steps depend on this one).")
			return
		}
		if err := provision.Apply(ctx, ssh, s, os.Stdout); err != nil {
			fmt.Printf("     ✗ %v\n", err)
			os.Exit(1)
		}
		fmt.Println("     ✓ done")
	}

	// --- stage 4: join the swarm --------------------------------------------
	id, err := provision.HarvestIdentity(ctx, ssh, layout)
	die(err)
	fmt.Printf("\n  node identity\n")
	fmt.Printf("    version    %s\n", id.Version)
	fmt.Printf("    device ID  %s\n", id.DeviceID)
	fmt.Printf("    GUI/API    http://%s:8384  (tailnet only)\n", layout.TailnetIP)

	newNode := config.Node{
		Name:   nodeName,
		URL:    fmt.Sprintf("http://%s:8384", layout.TailnetIP),
		APIKey: id.APIKey,
		Root:   layout.FolderDir,
		Mount:  layout.Mount,
		SSH:    dest,
	}

	fmt.Printf("\n  add %s to %s and to every node in the swarm?\n", nodeName, cfgPath)
	fmt.Println("  (this shares NO folders — it only makes the nodes know each other,")
	fmt.Println("   so you can share from the dashboard whenever you want)")
	if !confirmYN(in, yes) {
		fmt.Println("  skipped. The node is installed but not registered.")
		return
	}

	die(provision.AppendNode(cfgPath, newNode))
	fmt.Printf("  ✓ appended to %s\n", cfgPath)

	cfg, err := config.Load(cfgPath)
	die(err)
	res := provision.MeshDevice(ctx, cfg, newNode, id.DeviceID)

	if len(res.AddedTo) > 0 {
		fmt.Printf("  ✓ device added to: %s\n", strings.Join(res.AddedTo, ", "))
	}
	if len(res.AlreadyHad) > 0 {
		fmt.Printf("  · already known by: %s\n", strings.Join(res.AlreadyHad, ", "))
	}
	for node, e := range res.Failed {
		fmt.Printf("  ✗ %s: %s (re-run to finish — a partial mesh is incomplete, not corrupt)\n", node, e)
	}
	fmt.Printf("\n  %s is in the swarm. No folders shared — do that from the dashboard.\n", nodeName)
}

func confirmYN(in *bufio.Reader, yes bool) bool {
	if yes {
		return true
	}
	fmt.Print("  [y/N]: ")
	line, _ := in.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
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
