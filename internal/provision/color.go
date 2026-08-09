package provision

import (
	"io"
	"os"
)

// Style paints terminal output, and knows when not to.
//
// Colour is a convenience for a human watching a wizard work through a box; it
// is actively harmful in a log file, where escape sequences survive into
// whatever greps the file later. So it switches itself off for anything that is
// not a terminal, and honours NO_COLOR (https://no-color.org) even on one.
type Style struct{ on bool }

func NewStyle(w io.Writer) Style {
	f, ok := w.(*os.File)
	if !ok {
		return Style{}
	}
	return Style{on: colorEnabled(f)}
}

func colorEnabled(f *os.File) bool {
	return !colorDisabledByEnv() && f != nil && isTerminal(f)
}

// colorDisabledByEnv is split out so it can be tested without a terminal.
func colorDisabledByEnv() bool {
	return os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb"
}

func (s Style) paint(code, text string) string {
	if !s.on {
		return text
	}
	return "\033[" + code + "m" + text + "\033[0m"
}

func (s Style) Green(t string) string  { return s.paint("32", t) }
func (s Style) Red(t string) string    { return s.paint("31", t) }
func (s Style) Yellow(t string) string { return s.paint("33", t) }
func (s Style) Cyan(t string) string   { return s.paint("36", t) }
func (s Style) Dim(t string) string    { return s.paint("2", t) }
func (s Style) Bold(t string) string   { return s.paint("1", t) }

// Mark is the glyph for a run state. ok and already share ✓ on purpose: both
// mean the box is in the wanted state, and only the reason differs.
func (s Style) Mark(st State) string {
	switch st {
	case StateOK, StateAlready:
		return s.Green("✓")
	case StateFailed:
		return s.Red("✗")
	case StateSkipped:
		return s.Yellow("⊘")
	case StateBlocked:
		return s.Dim("⋯")
	}
	return " "
}
