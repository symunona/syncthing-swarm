package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// State is what became of one step in a run.
type State string

const (
	StateAlready State = "already" // Check passed before anything ran
	StateOK      State = "ok"      // applied, and the post-Check confirmed it
	StateSkipped State = "skipped" // the user declined
	StateFailed  State = "failed"  // a command failed, or the post-Check did not confirm
	StateBlocked State = "blocked" // something in Needs is not satisfied
)

// Result is one row of the ledger.
type Result struct {
	Step  Step
	State State
	Err   error
}

// Ledger is the outcome of a run: one Result per step, in plan order.
type Ledger []Result

// Satisfied reports whether a step ID ended the run in the wanted state.
//
// An ID with no row counts as satisfied. The planner only emits steps for work
// that is OUTSTANDING, so a dependency that never appeared is a dependency that
// was already met — a node whose drive is mounted plans no mount step, and the
// steps that need a mounted drive must not block on its absence.
func (l Ledger) Satisfied(id string) bool {
	for _, r := range l {
		if r.Step.ID == id {
			return r.State == StateOK || r.State == StateAlready
		}
	}
	return true
}

func (l Ledger) Failed() []Result {
	var out []Result
	for _, r := range l {
		if r.State == StateFailed {
			out = append(out, r)
		}
	}
	return out
}

func (l Ledger) Count(s State) int {
	n := 0
	for _, r := range l {
		if r.State == s {
			n++
		}
	}
	return n
}

// Executor is the box, as far as the runner is concerned. Splitting it out is
// what lets the run logic be tested without an ssh connection or a Pi.
type Executor interface {
	// Satisfied evaluates a Check predicate. It runs UNPRIVILEGED.
	Satisfied(ctx context.Context, check []string) (bool, error)
	// Apply runs the step's commands as root.
	Apply(ctx context.Context, s Step) error
}

// RunOpts carries the caller's UI. Both hooks are optional: a nil Confirm
// applies everything, a nil Report prints nothing.
type RunOpts struct {
	Confirm func(Step) bool
	Report  func(Result)
}

// Run walks the steps in order and returns a ledger.
//
// It does NOT abort on failure. The wizard used to stop dead at the first
// problem, which meant one unwanted step — an SD card offered as a data drive,
// say — permanently prevented everything after it, including the syncthing
// install the user actually came for. A failure now propagates only along
// Needs; independent work still gets done, and the ledger says exactly what did
// not.
//
// Recovery is re-running the command. State lives on the box, Check reads it,
// and a second run therefore skips what succeeded and retries what did not.
// There is no journal file to go stale.
func Run(ctx context.Context, ex Executor, steps []Step, opts RunOpts) Ledger {
	led := make(Ledger, 0, len(steps))

	record := func(r Result) {
		led = append(led, r)
		if opts.Report != nil {
			opts.Report(r)
		}
	}

	for _, s := range steps {
		if blocker, ok := firstUnsatisfied(led, s.Needs); !ok {
			record(Result{Step: s, State: StateBlocked,
				Err: fmt.Errorf("needs %q, which did not succeed", blocker)})
			continue
		}

		// Before: is the box already there? A re-run must cost nothing.
		done, err := ex.Satisfied(ctx, s.Check)
		if err != nil {
			record(Result{Step: s, State: StateFailed, Err: fmt.Errorf("check: %w", err)})
			continue
		}
		if done {
			record(Result{Step: s, State: StateAlready})
			continue
		}

		if opts.Confirm != nil && !opts.Confirm(s) {
			record(Result{Step: s, State: StateSkipped})
			continue
		}

		if err := ex.Apply(ctx, s); err != nil {
			record(Result{Step: s, State: StateFailed, Err: err})
			continue
		}

		// After: commands exiting 0 is not evidence the box changed.
		done, err = ex.Satisfied(ctx, s.Check)
		if err != nil {
			record(Result{Step: s, State: StateFailed, Err: fmt.Errorf("verify: %w", err)})
			continue
		}
		if !done {
			record(Result{Step: s, State: StateFailed,
				Err: fmt.Errorf("commands succeeded but the box did not change (check still fails: %v)", s.Check)})
			continue
		}
		record(Result{Step: s, State: StateOK})
	}
	return led
}

// firstUnsatisfied returns the first unmet dependency, if any.
func firstUnsatisfied(led Ledger, needs []string) (string, bool) {
	for _, n := range needs {
		if !led.Satisfied(n) {
			return n, false
		}
	}
	return "", true
}

// SSHExecutor runs steps on a real box.
type SSHExecutor struct {
	S   *SSH
	Out io.Writer
}

func (e SSHExecutor) Apply(ctx context.Context, s Step) error {
	return Apply(ctx, e.S, s, e.Out)
}

// Satisfied runs the predicate as the login user, with no tty and no sudo: a
// password prompt in the middle of a read-only verification would be both
// surprising and, on a fresh box, unanswerable.
//
// A non-zero exit in the range 0-254 means "not satisfied", not "error" —
// that is the whole point of a predicate. Only a transport failure is an
// error, and the caller turns that into a failed step rather than pretending
// the box is fine. See isPredicateNo for why exit 255 is excluded from that
// range and treated as an error instead.
func (e SSHExecutor) Satisfied(ctx context.Context, check []string) (bool, error) {
	if len(check) == 0 {
		return false, fmt.Errorf("step has no Check")
	}
	script := "set -e; " + joinAnd(check)
	cmd := e.S.Command(ctx, false, "sh -c "+shellQuote(script))
	if err := cmd.Run(); err != nil {
		if isPredicateNo(err) {
			return false, nil
		}
		return false, fmt.Errorf("could not reach the box to run the check: %w", err)
	}
	return true, nil
}

func joinAnd(cmds []string) string {
	return strings.Join(cmds, " && ")
}

// isPredicateNo distinguishes "the predicate said no" from "ssh could not run
// it at all" — the box being unreachable, not the check answering false.
//
// ssh(1), EXIT STATUS: "ssh exits with the exit status of the remote command
// or with 255 if an error occurred." 255 is RESERVED for ssh's own failures —
// unreachable host, a connection dropped mid-run, authentication failure —
// and is never produced by the remote command finishing normally. So an exit
// code of 255 must NOT be read as "not satisfied": if a connection dies
// halfway through a run, every remaining Check would come back false, and the
// wizard would conclude the box needs everything applied again — including
// steps that already succeeded and are simply unreachable to re-verify right
// now. That is worse than reporting nothing: it proposes re-running installs
// and firewall rules against a box you cannot currently confirm the state of.
//
// The trade runs the other way for a REMOTE command that itself happens to
// exit 255: it is now misreported as a transport failure rather than "no".
// Accepted on purpose — every Check in this codebase is test/grep/systemctl
// is-active, none of which exit 255, so the only failure mode this can ever
// hit in practice is the one it exists to catch. Wrong in the "cannot verify"
// direction is safe; wrong in the "not done" direction corrupts the ledger.
func isPredicateNo(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	return ee.ExitCode() != 255
}
