package acceptanceprovider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// standingRules is a stand-in StandingDecider: a refusal word, an outage
// switch, and the subjects and kinds it was asked about.
type standingRules struct {
	mu      sync.Mutex
	refusal string
	outage  bool
	asked   []string
}

func (r *standingRules) decide(_ context.Context, subject AuthenticatedSubject, kind string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, subject.Program+" "+kind)
	if r.outage {
		return "", errors.New("rights unavailable")
	}
	return r.refusal, nil
}

func (r *standingRules) set(refusal string, outage bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusal, r.outage = refusal, outage
}

// runningExecutor claims queued work and keeps it running: it renews the
// lease and writes progress every few milliseconds until a store call fences
// it. It records the fences it saw and whether it could claim.
type runningExecutor struct {
	testExecutor
	mu      sync.Mutex
	fenced  map[string]error
	claimed map[string]bool
	steps   map[string]int64
	refused map[string]error
}

func newRunningExecutor() *runningExecutor {
	return &runningExecutor{testExecutor: testExecutor{profile: "running-v1"}, fenced: map[string]error{},
		claimed: map[string]bool{}, steps: map[string]int64{}, refused: map[string]error{}}
}

func (e *runningExecutor) Serve(ctx context.Context, store job.Store) error {
	var running sync.WaitGroup
	defer running.Wait()
	for {
		records, _ := store.List()
		for _, r := range records {
			e.mu.Lock()
			busy := e.claimed[r.ID]
			e.mu.Unlock()
			if busy || r.State.Terminal() || !store.Claimable(r) {
				continue
			}
			held, err := store.Claim(r.ID, "runner", time.Minute)
			if err != nil {
				e.mu.Lock()
				e.refused[r.ID] = err
				e.mu.Unlock()
				continue
			}
			e.mu.Lock()
			e.claimed[r.ID] = true
			e.mu.Unlock()
			running.Add(1)
			go func(id string, epoch int64) {
				defer running.Done()
				e.run(ctx, store, id, epoch)
			}(held.ID, held.Lease.Epoch)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Millisecond):
		}
	}
}

func (e *runningExecutor) run(ctx context.Context, store job.Store, id string, epoch int64) {
	for step := int64(1); ctx.Err() == nil; step++ {
		_, err := store.Renew(id, epoch, time.Minute)
		if err == nil {
			_, err = store.Update(id, epoch, func(r *job.Record) error {
				r.State, r.Progress.Done, r.Progress.Total = job.StateRunning, step, 1000
				return nil
			})
		}
		if errors.Is(err, job.ErrStaleEpoch) || errors.Is(err, job.ErrTerminal) {
			e.mu.Lock()
			e.fenced[id] = err
			e.mu.Unlock()
			return
		}
		e.mu.Lock()
		e.steps[id] = step
		e.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
}

func (e *runningExecutor) progress(id string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.steps[id]
}

func standingFixture(t *testing.T) (*Provider, *runningExecutor, *standingRules, Binding) {
	t.Helper()
	executor := newRunningExecutor()
	p, err := OpenWithExecutor(t.TempDir(), "logical-owner", executor)
	if err != nil {
		t.Fatal(err)
	}
	rules := &standingRules{}
	p.SetStanding(rules.decide)
	return p, executor, rules, localBinding(t)
}

func execute(t *testing.T, p *Provider) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		//unchecked: the executor stops when the test ends
		_ = p.Execute(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func snapshotOf(t *testing.T, p *Provider, binding Binding, id api.RequestIdentity) *api.OperationSnapshot {
	t.Helper()
	observed, err := p.BindOperationsBinding(binding).ObserveWork(id)
	if err != nil || observed.Snapshot == nil {
		t.Fatalf("observe: %+v %v", observed, err)
	}
	return observed.Snapshot
}

// JOB-A18: a running job is decided again on its renewals and progress
// writes. After its submitter's rule is revoked, the change notification
// (Recheck) and the executor's next store call end it: failed with the
// refusal as its reason, and the executor is fenced and stops.
func TestARevokedSubmitterEndsItsRunningJob(t *testing.T) {
	p, executor, rules, binding := standingFixture(t)
	s := submission(p, "running")
	receipt := accept(t, p.BindBinding(binding), s)
	execute(t, p)
	waitFor(t, "the job to make progress", func() bool { return executor.progress(receipt.OperationID) > 2 })

	rules.set("rights:denied", false)
	p.Recheck(context.Background())
	waitFor(t, "the executor to be fenced", func() bool {
		executor.mu.Lock()
		defer executor.mu.Unlock()
		return executor.fenced[receipt.OperationID] != nil
	})
	record, err := p.store.Load(receipt.OperationID)
	if err != nil || record.State != job.StateFailed || record.Error != standingErrorPrefix+"rights:denied" {
		t.Fatalf("revoked job record %+v %v", record, err)
	}
	if snap := snapshotOf(t, p, binding, s.Identity); snap.State != api.WorkStateFailed || snap.Failure == nil || snap.Failure.Message != "rights:denied" {
		t.Fatalf("revoked job snapshot %+v", snap)
	}
	stopped := executor.progress(receipt.OperationID)
	time.Sleep(20 * time.Millisecond)
	if executor.progress(receipt.OperationID) != stopped {
		t.Fatal("the executor kept making progress after the revoke")
	}
	rules.mu.Lock()
	asked := rules.asked
	rules.mu.Unlock()
	if len(asked) == 0 || asked[0] != binding.Subject.Program+" download" {
		t.Fatalf("the standing decision named %v", asked)
	}
}

// JOB-A18: a queued job nobody holds ends at the change notification itself,
// and one refused before it starts is never run: the executor's claim fails
// it with the refusal instead.
func TestARevokedSubmitterEndsItsQueuedJob(t *testing.T) {
	p, executor, rules, binding := standingFixture(t)
	queued := submission(p, "queued")
	first := accept(t, p.BindBinding(binding), queued)
	rules.set("rights:not_granted", false)
	p.Recheck(context.Background())
	if record, err := p.store.Load(first.OperationID); err != nil || record.State != job.StateFailed || record.Error != standingErrorPrefix+"rights:not_granted" {
		t.Fatalf("queued job after the change %+v %v", record, err)
	}

	later := submission(p, "later")
	second := accept(t, p.BindBinding(binding), later)
	execute(t, p)
	waitFor(t, "the claim to be refused", func() bool {
		executor.mu.Lock()
		defer executor.mu.Unlock()
		return errors.Is(executor.refused[second.OperationID], ErrStandingRefused)
	})
	if record, err := p.store.Load(second.OperationID); err != nil || record.State != job.StateFailed {
		t.Fatalf("job refused at its claim %+v %v", record, err)
	}
	if executor.progress(second.OperationID) != 0 {
		t.Fatal("a refused job ran")
	}
}

// JOB-A18: a decision that cannot be obtained claims nothing and extends no
// lease, and is no fence: a running job keeps its lease until it lapses, and
// once decisions answer again the work goes on.
func TestAStandingOutageExtendsNothingAndEndsNothing(t *testing.T) {
	p, executor, rules, binding := standingFixture(t)
	s := submission(p, "outage")
	receipt := accept(t, p.BindBinding(binding), s)
	rules.set("", true)
	execute(t, p)
	time.Sleep(30 * time.Millisecond)
	executor.mu.Lock()
	claimErr, claimed := executor.refused[receipt.OperationID], executor.claimed[receipt.OperationID]
	executor.mu.Unlock()
	if claimed || claimErr == nil || errors.Is(claimErr, job.ErrStaleEpoch) {
		t.Fatalf("an outage claimed the job (%v) or fenced it (%v)", claimed, claimErr)
	}
	p.Recheck(context.Background())
	if record, err := p.store.Load(receipt.OperationID); err != nil || record.State.Terminal() {
		t.Fatalf("an outage ended the job: %+v %v", record, err)
	}

	rules.set("", false)
	waitFor(t, "the job to run once decisions answer", func() bool { return executor.progress(receipt.OperationID) > 2 })
	record, err := p.store.Load(receipt.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	renewBy := record.Lease.ExpiresAt.Time
	rules.set("", true)
	store := p.executorStore(context.Background())
	// A renewal without a decision answers the record unextended, with no
	// error: the executor's lease stands until its own expiry.
	kept, err := store.Renew(receipt.OperationID, record.Lease.Epoch, time.Hour)
	if err != nil || kept == nil || kept.Lease.Epoch != record.Lease.Epoch || kept.Lease.ExpiresAt.After(renewBy.Add(time.Minute)) {
		t.Fatalf("a renewal without a decision read %+v %v", kept, err)
	}
	if after, err := p.store.Load(receipt.OperationID); err != nil || after.Lease.ExpiresAt.After(renewBy.Add(time.Minute)) {
		t.Fatalf("a renewal without a decision extended the lease: %+v %v", after, err)
	}
	// Past its expiry the store's own check still fences the holder.
	short, err := p.store.Renew(receipt.OperationID, record.Lease.Epoch, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(short.Lease.ExpiresAt.Time) + 20*time.Millisecond)
	if _, err := store.Renew(receipt.OperationID, record.Lease.Epoch, time.Hour); !errors.Is(err, job.ErrLeaseExpiry) {
		t.Fatalf("a renewal without a decision past the lease's expiry read %v", err)
	}
}

// JOB-A18: the rights change hook returns at once, however slow the store,
// and change notices that arrive while a pass runs coalesce into one more.
func TestRecheckSoonRunsOffTheEditPathAndCoalesces(t *testing.T) {
	p, _, rules, binding := standingFixture(t)
	accept(t, p.BindBinding(binding), submission(p, "slow"))
	entered, release := make(chan struct{}, 8), make(chan struct{})
	var passes int
	var passesMu sync.Mutex
	p.SetStanding(func(ctx context.Context, subject AuthenticatedSubject, kind string) (string, error) {
		passesMu.Lock()
		passes++
		passesMu.Unlock()
		entered <- struct{}{}
		<-release
		return rules.decide(ctx, subject, kind)
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	start := time.Now()
	for range 5 {
		p.RecheckSoon(ctx)
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("five change notices waited %s on the store", took)
	}
	<-entered
	close(release)
	waitFor(t, "the passes to finish", func() bool {
		p.standing.mu.Lock()
		defer p.standing.mu.Unlock()
		return !p.standing.running
	})
	passesMu.Lock()
	defer passesMu.Unlock()
	if passes != 2 {
		t.Fatalf("five notices during one pass ran %d passes, want 2", passes)
	}
}
