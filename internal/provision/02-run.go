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
// A non-zero exit means "not satisfied", not "error" — that is the whole point
// of a predicate. Only a transport failure is an error, and the caller turns
// that into a failed step rather than pretending the box is fine.
func (e SSHExecutor) Satisfied(ctx context.Context, check []string) (bool, error) {
	if len(check) == 0 {
		return false, fmt.Errorf("step has no Check")
	}
	script := "set -e; " + joinAnd(check)
	cmd := e.S.Command(ctx, false, "sh -c "+shellQuote(script))
	if err := cmd.Run(); err != nil {
		if isExitError(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func joinAnd(cmds []string) string {
	return strings.Join(cmds, " && ")
}

// isExitError distinguishes "the predicate said no" from "ssh could not run it".
func isExitError(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}
