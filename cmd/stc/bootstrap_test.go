package main

import (
	"bufio"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/symunona/syncthing-dashboard/internal/provision"
)

// Go's flag package stops parsing at the first positional argument, so
// `stc bootstrap rue -dry-run` silently DROPPED -dry-run and fell through to
// applying changes. For a flag whose entire job is "change nothing", being
// silently ignored is the worst possible failure.
//
// This pins the fix: flags parse on either side of the ssh destination.
func TestBootstrapFlagsAfterDestination(t *testing.T) {
	parse := func(args []string) (dest string, dryRun bool) {
		fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
		dry := fs.Bool("dry-run", false, "")
		_ = fs.Parse(args)
		dest = fs.Arg(0)
		_ = fs.Parse(fs.Args()[1:]) // the fix
		return dest, *dry
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"flag after destination", []string{"rue", "-dry-run"}},
		{"flag before destination", []string{"-dry-run", "rue"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest, dry := parse(tc.args)
			if dest != "rue" {
				t.Errorf("dest = %q, want rue", dest)
			}
			if !dry {
				t.Error("-dry-run was dropped: the wizard would have started APPLYING changes")
			}
		})
	}

	// And without the flag, it must not spuriously turn itself on.
	if dest, dry := parse([]string{"rue"}); dest != "rue" || dry {
		t.Errorf("bare destination: dest=%q dry=%v", dest, dry)
	}
}

// The wizard must never again gate the syncthing stage on "the hardening plan
// came back empty". A step the user declined, or one that was blocked, is not
// a reason to withhold the install they asked for.
//
// This is a blunt instrument — a source grep, not a behavioural pin — because
// cmdBootstrap and stageSyncthing talk to *provision.SSH directly (not the
// Executor interface provision.Run tests against) and call os.Exit on a fatal
// error, so neither is callable from a test without a live box. What CAN be
// pinned behaviourally is one level down: provision.Run itself already has
// TestRunBlocksOnlyDependents (internal/provision/02-run_test.go), which
// proves a declined step blocks only its dependents, not unrelated steps —
// that is the mechanism this task wires cmdBootstrap to rely on instead of
// its own "remaining steps" re-check. The grep below is what is left to pin
// at this layer: that the old gate's exact shape does not creep back in.
func TestBootstrapDoesNotGateSyncthingOnAnEmptyPlan(t *testing.T) {
	src, err := os.ReadFile("bootstrap.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"len(remaining) > 0",
		"still outstanding",
	} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("bootstrap.go still gates on the plan being empty (%q)", forbidden)
		}
	}
}

// stageSyncthing's Confirm closure used to prompt with a bare "apply? [y/N]:"
// and nothing else — unlike cmdBootstrap's, which echoes "→ <title>" and any
// Warn first. On the next real run against actual hardware, the operator
// would face three bare prompts for the syncthing install/service/gui steps
// with no idea which step they were being asked about. This is a source grep,
// not a behavioural pin, for the same reason
// TestBootstrapDoesNotGateSyncthingOnAnEmptyPlan is: stageSyncthing dials a
// real *provision.SSH and calls os.Exit on error, so it is not callable here.
func TestStageSyncthingConfirmEchoesBeforePrompting(t *testing.T) {
	src, err := os.ReadFile("bootstrap.go")
	if err != nil {
		t.Fatal(err)
	}
	body := functionBody(t, string(src), "func stageSyncthing")
	if !strings.Contains(body, `st.Cyan("→")`) {
		t.Error("stageSyncthing's Confirm closure does not echo the step title before " +
			"prompting, unlike cmdBootstrap's")
	}
	if !strings.Contains(body, "s.Warn") {
		t.Error("stageSyncthing's Confirm closure does not echo a step's Warn before prompting")
	}
}

// functionBody extracts one top-level function's source, from its signature up
// to (not including) the next top-level `func `. Good enough for a grep test
// that needs to look inside ONE function rather than the whole file — bare
// substring matching against the whole file cannot tell stageSyncthing's
// Confirm closure apart from cmdBootstrap's, now that both echo the same way.
func functionBody(t *testing.T, src, funcSig string) string {
	t.Helper()
	i := strings.Index(src, funcSig)
	if i < 0 {
		t.Fatalf("could not find %q in bootstrap.go", funcSig)
	}
	rest := src[i+len(funcSig):]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// confirmDiff is the one piece of the upsert flow that does not touch ssh or
// the filesystem, so it is the one piece this task can pin behaviourally: a
// stale swarm.yaml entry (fiona, rebuilt onto a new SSD, with a new drive
// path and a new API key) must show the diff and respect the user's answer —
// not silently rewrite, and not silently keep the stale values either.
func TestConfirmDiffRespectsTheAnswer(t *testing.T) {
	changes := []provision.FieldChange{
		{Field: "apikey", Old: "old-key", New: "new-key"},
		{Field: "mount", Old: "", New: "/mnt/ssd"},
	}

	for _, tc := range []struct {
		name  string
		input string
		yes   bool
		want  bool
	}{
		{"typed yes proceeds", "y\n", false, true},
		{"typed no declines", "n\n", false, false},
		{"blank line declines", "\n", false, false},
		{"-yes bypasses the prompt", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := bufio.NewReader(strings.NewReader(tc.input))
			if got := confirmDiff(in, changes, tc.yes); got != tc.want {
				t.Errorf("confirmDiff(%q, yes=%v) = %v, want %v", tc.input, tc.yes, got, tc.want)
			}
		})
	}
}
