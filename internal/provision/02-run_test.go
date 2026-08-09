package provision

import (
	"context"
	"errors"
	"testing"
)

// fakeExec answers Satisfied from a set of step IDs and records what ran.
// checkAfter lets a test model the step that "succeeds" while changing nothing.
type fakeExec struct {
	satisfied  map[string]bool // step ID -> current answer
	applyErr   map[string]error
	applied    []string
	checkAfter map[string]bool // step ID -> what Satisfied returns AFTER Apply

	// checkErr models ssh dying instead of the predicate answering, keyed by
	// step ID. By default it fires on the very first Satisfied call for that
	// ID — the box was already unreachable before anything ran. Setting the
	// same ID in checkErrAfterApply delays it until AFTER Apply has run for
	// that ID instead, modeling a connection that dies partway through a run:
	// the pre-Check must still succeed (so Apply gets a chance to run), and
	// only the post-Check sees the transport failure.
	checkErr           map[string]error
	checkErrAfterApply map[string]bool

	checks int
}

// The fake keys checks by the step ID smuggled through Check[0], so the test
// does not have to write real shell.
func (f *fakeExec) Satisfied(_ context.Context, check []string) (bool, error) {
	f.checks++
	id := check[0]
	if err, ok := f.checkErr[id]; ok {
		if !f.checkErrAfterApply[id] || f.wasApplied(id) {
			return false, err
		}
	}
	return f.satisfied[id], nil
}

func (f *fakeExec) wasApplied(id string) bool {
	for _, a := range f.applied {
		if a == id {
			return true
		}
	}
	return false
}

func (f *fakeExec) Apply(_ context.Context, s Step) error {
	f.applied = append(f.applied, s.ID)
	if err := f.applyErr[s.ID]; err != nil {
		return err
	}
	if after, ok := f.checkAfter[s.ID]; ok {
		f.satisfied[s.ID] = after
	} else {
		f.satisfied[s.ID] = true
	}
	return nil
}

func step(id string, needs ...string) Step {
	return Step{ID: id, Title: id, Check: []string{id}, Needs: needs}
}

func alwaysYes(Step) bool { return true }

// A box that is already in the wanted state must cost nothing: no commands, no
// prompts. This is what makes re-running the wizard the recovery mechanism.
func TestRunSkipsAlreadySatisfiedSteps(t *testing.T) {
	f := &fakeExec{satisfied: map[string]bool{"inotify": true}}
	led := Run(context.Background(), f, []Step{step("inotify")}, RunOpts{Confirm: alwaysYes})

	if len(f.applied) != 0 {
		t.Errorf("ran commands for an already-satisfied step: %v", f.applied)
	}
	if led[0].State != StateAlready {
		t.Errorf("state = %q, want %q", led[0].State, StateAlready)
	}
}

// The half-applied step: every command exits 0, the box does not change. This
// used to be indistinguishable from success.
func TestRunFailsWhenPostCheckDoesNotConfirm(t *testing.T) {
	f := &fakeExec{
		satisfied:  map[string]bool{"hd-idle": false},
		checkAfter: map[string]bool{"hd-idle": false},
	}
	led := Run(context.Background(), f, []Step{step("hd-idle")}, RunOpts{Confirm: alwaysYes})

	if led[0].State != StateFailed {
		t.Fatalf("state = %q, want %q", led[0].State, StateFailed)
	}
	if led[0].Err == nil {
		t.Error("failed step carries no error to show the user")
	}
}

// A box that cannot be reached before anything ran is a failure, not a
// "no" — and must never get an Apply attempt against a connection that is
// already down.
func TestRunFailsWhenCheckCannotReachTheBox(t *testing.T) {
	f := &fakeExec{
		satisfied: map[string]bool{},
		checkErr:  map[string]error{"ufw": errors.New("ssh: connection refused")},
	}
	led := Run(context.Background(), f, []Step{step("ufw")}, RunOpts{Confirm: alwaysYes})

	if led[0].State != StateFailed {
		t.Fatalf("state = %q, want %q", led[0].State, StateFailed)
	}
	if led[0].Err == nil {
		t.Error("failed step carries no error to show the user")
	}
	if len(f.applied) != 0 {
		t.Errorf("applied the step despite being unable to check it first: %v", f.applied)
	}
}

// A connection that dies BETWEEN the pre- and post-Check must not read as
// "the box did not change" — that would tell the wizard to propose
// re-applying a step it can no longer even verify, on a box it can no longer
// reach at all.
func TestRunFailsWhenPostCheckCannotReachTheBox(t *testing.T) {
	f := &fakeExec{
		satisfied:          map[string]bool{},
		checkErr:           map[string]error{"ufw": errors.New("ssh: connection reset by peer")},
		checkErrAfterApply: map[string]bool{"ufw": true},
	}
	led := Run(context.Background(), f, []Step{step("ufw")}, RunOpts{Confirm: alwaysYes})

	if led[0].State != StateFailed {
		t.Fatalf("state = %q, want %q", led[0].State, StateFailed)
	}
	if led[0].Err == nil {
		t.Error("failed step carries no error to show the user")
	}
	if len(f.applied) != 1 {
		t.Errorf("applied = %v, want exactly one apply before the connection died", f.applied)
	}
}

// The fiona deadlock, in miniature: declining one step must not cost you the
// steps that do not depend on it.
func TestRunBlocksOnlyDependents(t *testing.T) {
	f := &fakeExec{satisfied: map[string]bool{}}
	steps := []Step{step("apt-update"), step("hd-idle", "apt-update"), step("inotify")}

	led := Run(context.Background(), f, steps, RunOpts{
		Confirm: func(s Step) bool { return s.ID != "apt-update" },
	})

	want := map[string]State{"apt-update": StateSkipped, "hd-idle": StateBlocked, "inotify": StateOK}
	for _, r := range led {
		if want[r.Step.ID] != r.State {
			t.Errorf("%s: state = %q, want %q", r.Step.ID, r.State, want[r.Step.ID])
		}
	}
	for _, id := range f.applied {
		if id == "hd-idle" {
			t.Error("blocked step ran anyway")
		}
	}
}

// A failure propagates the same way a decline does.
func TestRunBlocksDependentsOfAFailure(t *testing.T) {
	f := &fakeExec{
		satisfied: map[string]bool{},
		applyErr:  map[string]error{"apt-update": errors.New("dpkg lock held")},
	}
	led := Run(context.Background(), f, []Step{step("apt-update"), step("ufw", "apt-update")},
		RunOpts{Confirm: alwaysYes})

	if led[0].State != StateFailed || led[1].State != StateBlocked {
		t.Errorf("states = %q, %q; want failed, blocked", led[0].State, led[1].State)
	}
}

// A Needs entry naming a step that was never planned means the requirement is
// already met — the planner only emits steps for work that is outstanding.
func TestRunTreatsUnplannedNeedsAsSatisfied(t *testing.T) {
	f := &fakeExec{satisfied: map[string]bool{}}
	led := Run(context.Background(), f, []Step{step("hd-idle", "apt-update")},
		RunOpts{Confirm: alwaysYes})

	if led[0].State != StateOK {
		t.Errorf("state = %q, want %q — apt-update was never planned, so nothing blocks", led[0].State, StateOK)
	}
}

func TestLedgerReportsFailures(t *testing.T) {
	led := Ledger{
		{Step: step("a"), State: StateOK},
		{Step: step("b"), State: StateFailed, Err: errors.New("boom")},
	}
	if len(led.Failed()) != 1 || led.Failed()[0].Step.ID != "b" {
		t.Errorf("Failed() = %+v", led.Failed())
	}
	if !led.Satisfied("a") || led.Satisfied("b") {
		t.Error("Satisfied() disagrees with the ledger")
	}
	if !led.Satisfied("never-planned") {
		t.Error("an unplanned ID must count as satisfied")
	}
}
