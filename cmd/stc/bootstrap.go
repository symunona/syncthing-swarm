package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
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

	st := provision.NewStyle(os.Stdout)
	ex := provision.SSHExecutor{S: ssh, Out: os.Stdout}

	led := provision.Run(ctx, ex, steps, provision.RunOpts{
		Confirm: func(s provision.Step) bool {
			fmt.Printf("  %s %s\n", st.Cyan("→"), s.Title)
			if s.Warn != "" {
				fmt.Printf("     %s %s\n", st.Yellow("⚠"), s.Warn)
			}
			return confirm(in, s, *yes)
		},
		Report: stepReporter(st),
	})

	printLedger(os.Stdout, st, led)

	if !*syncthing {
		fmt.Println("\n  (pass -syncthing to install syncthing and join the swarm)")
		return
	}

	// Re-probe: the hardening just changed the box, and the syncthing layout is
	// derived from what is mounted NOW.
	p2, err := provision.RunProbe(ctx, ssh, provision.ProbeOpts{}, nil)
	die(err)

	// Advisory, not blocking. Declining a step you do not want must never cost
	// you the install — that is exactly the deadlock this replaces: an SD card
	// wrongly offered as a data drive, declined, and syncthing never installed.
	// The one hard requirement is a mounted data drive, which layoutFor enforces.
	for _, r := range led {
		if r.State == provision.StateSkipped || r.State == provision.StateFailed || r.State == provision.StateBlocked {
			fmt.Printf("  %s %s did not complete — continuing to syncthing anyway\n",
				st.Yellow("⚠"), r.Step.Title)
		}
	}

	stageSyncthing(ctx, ssh, p2, in, *cfgPath, dest, *node, *yes)
}

// stepReporter narrates a run as it happens. The ledger printed afterwards is
// the receipt; this is the live commentary, and both use the same glyphs so a
// line means the same thing wherever it appears.
func stepReporter(st provision.Style) func(provision.Result) {
	return func(r provision.Result) {
		switch r.State {
		case provision.StateAlready:
			fmt.Printf("  %s %s %s\n", st.Mark(r.State), r.Step.Title, st.Dim("(already done)"))
		case provision.StateOK:
			fmt.Printf("  %s %s\n", st.Mark(r.State), r.Step.Title)
		case provision.StateSkipped:
			fmt.Printf("  %s %s %s\n", st.Mark(r.State), r.Step.Title, st.Dim("(skipped)"))
		case provision.StateBlocked:
			fmt.Printf("  %s %s %s\n", st.Mark(r.State), r.Step.Title, st.Dim(r.Err.Error()))
		case provision.StateFailed:
			fmt.Printf("  %s %s\n     %s\n", st.Mark(r.State), r.Step.Title, st.Red(r.Err.Error()))
		}
	}
}

// printLedger is the run's receipt. A skipped or failed step is not a reason to
// hide everything else that worked — the wizard's old behaviour of stopping
// dead meant a run's only output was its first problem.
func printLedger(w io.Writer, st provision.Style, led provision.Ledger) {
	fmt.Fprintf(w, "\n  ── result ──\n\n")
	for _, r := range led {
		fmt.Fprintf(w, "  %s %-12s %s\n", st.Mark(r.State), r.State, r.Step.Title)
	}
	fmt.Fprintf(w, "\n  %d ok, %d already, %d skipped, %d failed, %d blocked\n",
		led.Count(provision.StateOK), led.Count(provision.StateAlready),
		led.Count(provision.StateSkipped), led.Count(provision.StateFailed),
		led.Count(provision.StateBlocked))
	if f := led.Failed(); len(f) > 0 {
		fmt.Fprintf(w, "\n  %s the box is the state — fix the cause and re-run; the probe picks up\n"+
			"    from wherever reality actually is, and finished steps cost nothing.\n", st.Yellow("⚠"))
	}
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
	st := provision.NewStyle(os.Stdout) // stageSyncthing has no style of its own yet

	led := provision.Run(ctx, provision.SSHExecutor{S: ssh, Out: os.Stdout}, steps, provision.RunOpts{
		Confirm: func(s provision.Step) bool { return confirm(in, s, yes) },
		Report:  stepReporter(st),
	})
	printLedger(os.Stdout, st, led)

	if len(led.Failed()) > 0 || !led.Satisfied("syncthing-service") {
		fmt.Printf("\n  %s syncthing is not running — not joining the swarm. Re-run when it is.\n", st.Red("✗"))
		return
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

	changes, exists, err := provision.DiffNode(cfgPath, newNode)
	die(err)
	if exists && len(changes) == 0 {
		fmt.Printf("  %s %s already in %s, unchanged\n", st.Mark(provision.StateAlready), nodeName, cfgPath)
	} else {
		if exists {
			fmt.Printf("\n  %s is already in %s and this run found different values:\n\n", nodeName, cfgPath)
			if !confirmDiff(in, changes, yes) {
				fmt.Println("  skipped. The node is installed but swarm.yaml still describes the old box.")
				return
			}
		}
		die(provision.UpsertNode(cfgPath, newNode))
		fmt.Printf("  %s wrote %s to %s\n", st.Mark(provision.StateOK), nodeName, cfgPath)
	}

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

// confirmDiff prints a field-by-field diff of what UpsertNode would rewrite
// and asks once before doing it — a node outlives its hardware (fiona came
// back from an SD-card rebuild with a new drive path and a new API key), and
// swarm.yaml should not be silently overwritten just because the box answers
// to the same name it used to.
func confirmDiff(in *bufio.Reader, changes []provision.FieldChange, yes bool) bool {
	st := provision.NewStyle(os.Stdout)
	for _, c := range changes {
		old := c.Old
		if old == "" {
			old = "(unset)"
		}
		fmt.Printf("    %-7s %s → %s\n", c.Field, st.Dim(old), st.Green(c.New))
	}
	fmt.Println("\n  rewrite these values? (comments and other nodes are untouched)")
	return confirmYN(in, yes)
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
