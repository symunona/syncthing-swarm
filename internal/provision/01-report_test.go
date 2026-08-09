package provision

import (
	"bytes"
	"strings"
	"testing"
)

// TestRendererEventGlyphs pins both the glyph vocabulary and the no-escape-when-piped
// promise against future drift. Non-TTY output (bytes.Buffer) must be plain text.
func TestRendererEventGlyphs(t *testing.T) {
	// Create a minimal Probe for summarize calls
	p := &Probe{
		Box: Box{
			Model:    "test",
			Arch:     "amd64",
			Cores:    4,
			MemBytes: 8_000_000_000,
		},
	}

	// Test ok event
	{
		var buf bytes.Buffer
		r := NewRenderer(&buf)
		r.Event(Event{State: "ok", Check: "test", MS: 100}, p)
		output := buf.String()
		if !strings.Contains(output, "✓") {
			t.Errorf("ok event should contain ✓, got: %q", output)
		}
		if strings.Contains(output, "\033") {
			t.Errorf("ok event output should not contain escape sequences: %q", output)
		}
	}

	// Test skip event
	{
		var buf bytes.Buffer
		r := NewRenderer(&buf)
		r.Event(Event{State: "skip", Check: "test", Note: "skipped by user", MS: 10}, p)
		output := buf.String()
		if !strings.Contains(output, "⊘") {
			t.Errorf("skip event should contain ⊘, got: %q", output)
		}
		if !strings.Contains(output, "skipped by user") {
			t.Errorf("skip event should contain the note, got: %q", output)
		}
		if strings.Contains(output, "\033") {
			t.Errorf("skip event output should not contain escape sequences: %q", output)
		}
	}

	// Test err event
	{
		var buf bytes.Buffer
		r := NewRenderer(&buf)
		r.Event(Event{State: "err", Check: "test", Note: "something failed", MS: 50}, p)
		output := buf.String()
		if !strings.Contains(output, "✗") {
			t.Errorf("err event should contain ✗, got: %q", output)
		}
		if !strings.Contains(output, "something failed") {
			t.Errorf("err event should contain the note, got: %q", output)
		}
		if strings.Contains(output, "\033") {
			t.Errorf("err event output should not contain escape sequences: %q", output)
		}
	}
}
