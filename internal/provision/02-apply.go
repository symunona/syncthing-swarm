package provision

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
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
	cmd.Stdout = prefixWriter(out, "    │ ")
	cmd.Stderr = prefixWriter(out, "    │ ")
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
func prefixWriter(w io.Writer, prefix string) io.Writer {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			fmt.Fprintf(w, "%s%s\n", prefix, sc.Text())
		}
	}()
	return pw
}

// Verify re-probes the box and reports which steps actually took effect. The box
// is the state — there is no state file and no resume bookkeeping, so a failed
// or interrupted run is recovered by simply running the wizard again.
func Verify(ctx context.Context, s *SSH, user string) (remaining []Step, err error) {
	p, err := RunProbe(ctx, s, ProbeOpts{}, nil)
	if err != nil {
		return nil, err
	}
	steps, _ := Plan(p, user)
	return steps, nil
}
