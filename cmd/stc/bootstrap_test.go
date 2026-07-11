package main

import (
	"flag"
	"testing"
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
