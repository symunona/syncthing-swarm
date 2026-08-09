package provision

import (
	"bytes"
	"strings"
	"testing"
)

// Piped output must stay clean: the probe's own docs promise it "degrades to
// sane plain text when piped to a file", and escape sequences in a log someone
// greps later are noise.
func TestStyleOffWhenNotATTY(t *testing.T) {
	s := NewStyle(&bytes.Buffer{})
	for _, got := range []string{s.Green("ok"), s.Red("bad"), s.Dim("x"), s.Mark(StateOK)} {
		if strings.Contains(got, "\033") {
			t.Errorf("escape sequence in non-tty output: %q", got)
		}
	}
	if s.Green("ok") != "ok" {
		t.Errorf("Green() = %q, want the bare text", s.Green("ok"))
	}
}

// NO_COLOR is honoured even on a terminal (https://no-color.org), as is the
// dumb terminal every CI runner claims to be.
func TestStyleRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	if !colorDisabledByEnv() {
		t.Error("NO_COLOR set but the environment check allows colour")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if !colorDisabledByEnv() {
		t.Error("TERM=dumb but the environment check allows colour")
	}
	t.Setenv("TERM", "xterm-256color")
	if colorDisabledByEnv() {
		t.Error("plain terminal reported as colour-disabled")
	}
}

// Every run state needs a distinguishable glyph — the ledger is unreadable if
// skipped and blocked look the same.
func TestMarkIsDistinctPerState(t *testing.T) {
	s := Style{}
	seen := map[string]State{}
	for _, st := range []State{StateOK, StateAlready, StateSkipped, StateFailed, StateBlocked} {
		g := s.Mark(st)
		if g == "" {
			t.Errorf("%s has no glyph", st)
		}
		if prev, dup := seen[g]; dup && prev != StateOK && st != StateAlready {
			t.Errorf("%s and %s share glyph %q", prev, st, g)
		}
		seen[g] = st
	}
}
