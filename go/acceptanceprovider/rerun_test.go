package acceptanceprovider

import (
	"context"
	"sync"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// endingExecutor claims each queued job once and ends it as end says: it
// counts the claims, so a replay shows as no new claim.
type endingExecutor struct {
	testExecutor
	end    func(*job.Record)
	mu     sync.Mutex
	claims map[string]int
}

func (e *endingExecutor) Serve(ctx context.Context, store job.Store) error {
	for ctx.Err() == nil {
		records, _ := store.List()
		for _, r := range records {
			if r.State.Terminal() || !store.Claimable(r) {
				continue
			}
			held, err := store.Claim(r.ID, "runner", time.Minute)
			if err != nil {
				continue
			}
			e.mu.Lock()
			e.claims[r.ID]++
			e.mu.Unlock()
			//unchecked: a refused write leaves the record to the next pass
			_, _ = store.Update(r.ID, held.Lease.Epoch, func(rec *job.Record) error {
				e.end(rec)
				return nil
			})
		}
		time.Sleep(3 * time.Millisecond)
	}
	return nil
}

func (e *endingExecutor) claimed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.claims {
		n += c
	}
	return n
}

func terminal(t *testing.T, p *Provider, binding Binding, id api.RequestIdentity) *api.OperationSnapshot {
	t.Helper()
	var snap *api.OperationSnapshot
	waitFor(t, "a terminal state", func() bool {
		snap = snapshotOf(t, p, binding, id)
		return snap.State == api.WorkStateComplete || snap.State == api.WorkStateFailed || snap.State == api.WorkStateCancelled
	})
	return snap
}

// JOB-A19: an operation a rights refusal ended is no result of the work. The
// same request submitted again after the person unblocks is decided again and
// runs once more under the same identity, as a new operation; while the rule
// still refuses, the resubmission is refused and nothing runs.
func TestARightsRefusalIsNotReplayedToTheSameRequest(t *testing.T) {
	p, executor, rules, binding := standingFixture(t)
	s := submission(p, "blocked-then-unblocked")
	first := accept(t, p.BindBinding(binding), s)
	execute(t, p)
	waitFor(t, "the first run", func() bool { return executor.progress(first.OperationID) > 2 })

	rules.set("rights:denied", false)
	p.Recheck(context.Background())
	if snap := terminal(t, p, binding, s.Identity); snap.State != api.WorkStateFailed || snap.Failure == nil || snap.Failure.Message != "rights:denied" {
		t.Fatalf("blocked run ended %+v", snap)
	}

	// Still blocked: refused, and the identity still names the refused run.
	if v, err := p.BindBinding(binding).Submit(s); err != nil || v.Outcome != api.AcceptanceOutcomeForbidden || v.Reason != "rights:denied" {
		t.Fatalf("resubmission while blocked %+v %v", v, err)
	}

	rules.set("", false)
	v, err := p.BindBinding(binding).Submit(s)
	if err != nil || v.Outcome != api.AcceptanceOutcomeAccepted || v.Receipt == nil || v.Receipt.OperationID == first.OperationID || v.Receipt.Identity != s.Identity {
		t.Fatalf("resubmission after the unblock %+v %v", v, err)
	}
	second := v.Receipt.OperationID
	waitFor(t, "the second run", func() bool { return executor.progress(second) > 2 })
	if snap := snapshotOf(t, p, binding, s.Identity); snap.State != api.WorkStateRunning || snap.Receipt.OperationID != second {
		t.Fatalf("the identity does not name the new run: %+v", snap)
	}
	// The new run is the request's one live operation: a duplicate returns it.
	if again, err := p.BindBinding(binding).Submit(s); err != nil || again.Receipt == nil || again.Receipt.OperationID != second {
		t.Fatalf("duplicate of the new run %+v %v", again, err)
	}
	if record, err := p.store.Load(first.OperationID); err != nil || record.State != job.StateFailed {
		t.Fatalf("the refused run changed: %+v %v", record, err)
	}
}

// JOB-A19 keeps JOB-A1's idempotency for every other result: a completed
// operation and a failure of the work itself replay to the same request, and
// nothing runs again.
func TestOtherResultsStillReplay(t *testing.T) {
	for _, c := range []struct {
		name  string
		end   func(*job.Record)
		state api.WorkState
	}{
		{"complete", func(r *job.Record) { r.State = job.StateComplete }, api.WorkStateComplete},
		{"failed work", func(r *job.Record) { r.State, r.Error = job.StateFailed, "upstream refused the transfer" }, api.WorkStateFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			executor := &endingExecutor{testExecutor: testExecutor{profile: "ending-v1"}, end: c.end, claims: map[string]int{}}
			p, err := OpenWithExecutor(t.TempDir(), "logical-owner", executor)
			if err != nil {
				t.Fatal(err)
			}
			rules := &standingRules{}
			p.SetStanding(rules.decide)
			binding := localBinding(t)
			s := submission(p, c.name)
			first := accept(t, p.BindBinding(binding), s)
			execute(t, p)
			if snap := terminal(t, p, binding, s.Identity); snap.State != c.state {
				t.Fatalf("ended %+v", snap)
			}
			again := accept(t, p.BindBinding(binding), s)
			if again.OperationID != first.OperationID {
				t.Fatalf("a %s result was not replayed: %s then %s", c.name, first.OperationID, again.OperationID)
			}
			time.Sleep(30 * time.Millisecond)
			if n := executor.claimed(); n != 1 {
				t.Fatalf("the replayed request ran %d times", n)
			}
		})
	}
}
