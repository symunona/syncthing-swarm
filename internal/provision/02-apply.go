package provision

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// Apply runs one step on the box, as root, streaming its output.
//
// Privilege comes from `ssh -t … sudo`, so the sudo prompt lands in the user's
// own terminal: the password never enters this process's memory, a config file,
// or a log. It is also why we do not require passwordless sudo — a fresh box has
// none, and demanding it would defeat the point of a bootstrap wizard.
func Apply(ctx context.Context, s *SSH, step Step, out io.Writer) error {
	script := buildScript(step)
	cmd := s.Command(ctx, true, "sudo sh -c "+shellQuote(script))

	// Hand stdin to the remote ONLY when it is a real terminal — that is the case
	// where sudo needs to prompt, and where the tty is shared rather than consumed.
	//
	// When stdin is a pipe (scripted run, CI), the remote must NOT get it: ssh
	// forwards stdin to the remote command, so an earlier step would swallow the
	// input meant for a LATER prompt. That is not hypothetical — it ate the typed
	// "ufw" confirmation on the first real run, silently skipping the step. The
	// mirror of that bug is worse: a queued "y" being fed into a remote program.
	if isTerminal(os.Stdin) {
		cmd.Stdin = os.Stdin
	}
	// One writer for both streams: with `ssh -t` the remote's stderr comes back
	// down the same pty as its stdout, and two writers would each track their own
	// mid-line state and interleave prefixes mid-word.
	pw := prefixWriter(out, "    │ ")
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("step %q failed: %w", step.ID, err)
	}
	return nil
}

// buildScript chains a step's commands with `&&`, not `;`.
//
// This is load-bearing for the ufw step: its commands are ordered "allow ssh,
// allow tailscale0, THEN enable". With `;` a failed allow rule would still let
// the enable run — and the box would come up firewalled with no ssh rule, i.e.
// you are locked out of a headless machine. With `&&` a failed rule aborts before
// the enable. The ordering is a safety property, so the shell must enforce it.
//
// `set -e` covers the same ground for anything that slips through.
func buildScript(step Step) string {
	// DEBIAN_FRONTEND=noninteractive: these run on a headless box over ssh, and a
	// debconf prompt with nobody to answer it hangs the step forever.
	return "export DEBIAN_FRONTEND=noninteractive; set -e; " + strings.Join(step.Cmds, " && ")
}

// prefixWriter indents remote output so it is visibly the box talking, not us.
//
// It writes THROUGH, byte for byte, and never waits for a line to end. The
// line-buffered version this replaces swallowed the one piece of output that
// matters most: sudo's "[sudo: authenticate] Password:" carries no trailing
// newline, so it sat in the scanner's buffer until the command finished. The
// step looked hung, the password the user typed went to a prompt nobody could
// see, and the wizard appeared not to forward sudo at all.
type prefixW struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	mid    bool // a prefix has already been emitted for the line in progress
}

func prefixWriter(w io.Writer, prefix string) io.Writer {
	return &prefixW{w: w, prefix: prefix}
}

func (p *prefixW) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := 0
	for len(b) > 0 {
		if !p.mid {
			if _, err := io.WriteString(p.w, p.prefix); err != nil {
				return n, err
			}
			p.mid = true
		}
		chunk := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			chunk, p.mid = b[:i+1], false
		}
		m, err := p.w.Write(chunk)
		n += m
		if err != nil {
			return n, err
		}
		b = b[m:]
	}
	return n, nil
}
