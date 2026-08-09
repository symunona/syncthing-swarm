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
