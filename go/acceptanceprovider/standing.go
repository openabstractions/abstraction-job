package acceptanceprovider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

// Accepted work lives only while its submitter's rule does (CONTRACT.md
// JOB-A18). Submit decides the rule once, through the method policy. A
// StandingDecider decides it again, asking nobody, for the subject the
// journal recorded at admission: before the executor claims the work
// (a first start and every resume), before each lease renewal and each
// progress write, on every committed rights change (Recheck) and on the
// provider's own clock, which covers a rule that expires.

// A StandingDecider decides again, asking nobody, whether subject may still
// have work of kind run on its behalf. It answers "" while the rule permits,
// a refusal word (rights:<word>) once it refuses, and an error when no
// decision could be obtained.
type StandingDecider func(ctx context.Context, subject AuthenticatedSubject, kind string) (refusal string, err error)

// standingInterval is how often Execute decides every unfinished operation
// again. A running executor's own renewals decide more often than this; the
// clock is for queued work and for work a remote runner holds.
const standingInterval = 30 * time.Second

// standingOwner names the provider when it claims, fails or cancels work on
// its own behalf for a refused rule.
const standingOwner = "abstraction.job/standing"

// standingErrorPrefix marks a record failed for a refused rule; the snapshot
// reads the refusal after it (operationSnapshot).
const standingErrorPrefix = "standing: "

// ErrStandingRefused reports work whose submitter's rule no longer permits
// it. The error an executor sees also matches job.ErrStaleEpoch: the
// executor no longer holds work it may act on, and stops.
var ErrStandingRefused = errors.New("acceptance: the submitter's rule no longer permits this work")

// The calls a standing decision is asked for, which StandingCallOf reads
// from the decision's context.
const (
	StandingClaim    = "claim"
	StandingRenew    = "renew"
	StandingProgress = "progress"
	StandingRecheck  = "recheck"
	StandingResubmit = "resubmit"
)

type standingCallKey struct{}

func withStandingCall(ctx context.Context, call string) context.Context {
	return context.WithValue(ctx, standingCallKey{}, call)
}

// StandingCallOf names the call a StandingDecider was asked for: a claim, a
// lease renewal, a progress write, a recheck or a resubmission (JOB-A19). It
// reads "" outside one.
func StandingCallOf(ctx context.Context) string {
	call, _ := ctx.Value(standingCallKey{}).(string)
	return call
}

// errStandingUnavailable is a standing decision that could not be obtained.
var errStandingUnavailable = errors.New("acceptance: the submitter's rule could not be decided")

type standingRefusal struct{ refusal string }

func (e *standingRefusal) Error() string {
	return "acceptance: work ended, " + e.refusal
}

func (e *standingRefusal) Is(target error) bool {
	return target == ErrStandingRefused || target == job.ErrStaleEpoch
}

// standingState is the provider's standing configuration and what it has
// already seen end.
type standingState struct {
	decide StandingDecider
	mu     sync.Mutex
	ended  map[string]bool
	// running is a RecheckSoon pass in flight; again asks it for one more.
	// stopped refuses new passes once StopRechecks began, and passes counts
	// the goroutine StopRechecks waits for. Add happens under mu while not
	// stopped, so no Add follows the Wait.
	running, again, stopped bool
	passes                  sync.WaitGroup
	// interval replaces standingInterval when positive; tests shorten it.
	interval time.Duration
}

// SetStanding configures the decision every accepted operation is decided
// again by (JOB-A18). Set it before Execute; nil decides nothing again.
func (p *Provider) SetStanding(decide StandingDecider) {
	p.standing.decide = decide
}

// standingOf decides operation id again for the subject and kind its journal
// recorded. An operation whose journal recorded no subject was admitted under
// no subject's rule and is not decided again.
func (p *Provider) standingOf(ctx context.Context, id string) (string, error) {
	if p.standing.decide == nil {
		return "", nil
	}
	p.subjectsMu.RLock()
	record, found := p.subjects[id]
	p.subjectsMu.RUnlock()
	if !found || record.subject == nil {
		return "", nil
	}
	refusal, err := p.standing.decide(ctx, *record.subject, record.kind)
	if err != nil || ctx.Err() != nil {
		return "", errStandingUnavailable
	}
	return refusal, nil
}

// failRefused is the terminal write for a refused rule.
func failRefused(refusal string) func(*job.Record) error {
	return func(r *job.Record) error {
		r.State, r.Error = job.StateFailed, standingErrorPrefix+refusal
		return nil
	}
}

// standingFailure is the snapshot failure of a record failed for a refused
// rule, or nil.
func standingFailure(record *job.Record) *api.WorkFailure {
	refusal, ok := strings.CutPrefix(record.Error, standingErrorPrefix)
	if record.State != job.StateFailed || !ok {
		return nil
	}
	return &api.WorkFailure{Classification: api.FailureClassPermanent, Message: refusal, Cause: api.FailureCauseOther}
}

// Recheck decides every unfinished operation again and ends each one whose
// submitter's rule refuses it (JOB-A18). Work nobody holds is claimed and
// failed with the refusal at once. Work a runner holds is asked to stop with
// a cancel intent, which every runner honours, a remote one included; a local
// executor's next store call fails it with the refusal first. A decision that
// cannot be obtained ends nothing here: a running executor's renewals stop
// extending its lease, so it lapses at its expiry. Execute runs it on its
// clock, and a composition runs it through RecheckSoon on each committed
// rights change (abstraction-rights RIGHTS-R23).
func (p *Provider) Recheck(ctx context.Context) {
	if p.standing.decide == nil {
		return
	}
	p.subjectsMu.RLock()
	ids := make([]string, 0, len(p.subjects))
	for id, record := range p.subjects {
		if record.subject != nil {
			ids = append(ids, id)
		}
	}
	p.subjectsMu.RUnlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		p.standing.mu.Lock()
		ended := p.standing.ended[id]
		p.standing.mu.Unlock()
		if ended {
			continue
		}
		record, err := p.store.Load(id)
		if err != nil {
			continue
		}
		if record.State.Terminal() {
			p.standing.mu.Lock()
			if p.standing.ended == nil {
				p.standing.ended = map[string]bool{}
			}
			p.standing.ended[id] = true
			p.standing.mu.Unlock()
			continue
		}
		refusal, err := p.standingOf(withStandingCall(ctx, StandingRecheck), id)
		if err != nil || refusal == "" {
			continue
		}
		p.endRefused(record, refusal)
	}
}

// RecheckSoon runs Recheck on its own goroutine and returns at once, so a
// rights edit never waits on a slow store. Calls while one runs coalesce into
// one more pass after it; the clock in Execute is the backstop. A
// composition calls it on each committed rights change (RIGHTS-R23).
func (p *Provider) RecheckSoon(ctx context.Context) {
	if p.standing.decide == nil {
		return
	}
	p.standing.mu.Lock()
	if p.standing.stopped {
		p.standing.mu.Unlock()
		return
	}
	if p.standing.running {
		p.standing.again = true
		p.standing.mu.Unlock()
		return
	}
	p.standing.running = true
	p.standing.passes.Add(1)
	p.standing.mu.Unlock()
	go func() {
		defer p.standing.passes.Done()
		for {
			p.Recheck(ctx)
			p.standing.mu.Lock()
			if !p.standing.again || ctx.Err() != nil {
				p.standing.running, p.standing.again = false, false
				p.standing.mu.Unlock()
				return
			}
			p.standing.again = false
			p.standing.mu.Unlock()
		}
	}()
}

// StopRechecks refuses every later RecheckSoon and waits for the pass in
// flight to return, so nothing it started writes to the store afterwards. A
// host calls it on close, after cancelling the pass's context, and before it
// releases the store's root.
func (p *Provider) StopRechecks() {
	p.standing.mu.Lock()
	p.standing.stopped = true
	p.standing.mu.Unlock()
	p.standing.passes.Wait()
}

// endRefused ends one unfinished operation its submitter's rule refuses.
func (p *Provider) endRefused(record *job.Record, refusal string) {
	if p.store.Claimable(record) {
		held, err := p.store.Claim(record.ID, standingOwner, time.Minute)
		if err == nil {
			if _, err = p.store.Update(record.ID, held.Lease.Epoch, failRefused(refusal)); err == nil {
				return
			}
		}
	}
	if _, err := p.store.SetIntent(record.ID, job.WantCancel, standingOwner); err != nil {
		p.reportError(err)
	}
}

// standingClock runs Recheck every standingInterval until ctx ends.
func (p *Provider) standingClock(ctx context.Context) {
	every := p.standing.interval
	if every <= 0 {
		every = standingInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Recheck(ctx)
		}
	}
}

// standingStore is the store an executor receives when a StandingDecider is
// configured. Claim, Renew and Update decide the operation again before the
// executor may act on it (JOB-A18).
type standingStore struct {
	job.Store
	p   *Provider
	ctx context.Context
}

// standingScratchStore keeps a local store's scratch area visible through
// the wrapper.
type standingScratchStore struct {
	standingStore
	job.Scratch
}

func (p *Provider) executorStore(ctx context.Context) job.Store {
	if p.standing.decide == nil {
		return p.store
	}
	s := standingStore{Store: p.store, p: p, ctx: ctx}
	var store job.Store = p.store
	if scratch, ok := store.(job.Scratch); ok {
		return standingScratchStore{standingStore: s, Scratch: scratch}
	}
	return s
}

// Claim decides before a start or a resume: a refusal claims the work only
// to fail it with the refusal, and a decision that cannot be obtained claims
// nothing, so nothing runs.
func (s standingStore) Claim(id, owner string, ttl time.Duration) (*job.Record, error) {
	refusal, err := s.p.standingOf(withStandingCall(s.ctx, StandingClaim), id)
	if err != nil {
		return nil, err
	}
	if refusal == "" {
		return s.Store.Claim(id, owner, ttl)
	}
	held, err := s.Store.Claim(id, owner, ttl)
	if err != nil {
		return nil, err
	}
	if _, err := s.Store.Update(id, held.Lease.Epoch, failRefused(refusal)); err != nil {
		return nil, err
	}
	return nil, &standingRefusal{refusal: refusal}
}

// Renew decides before it extends the lease: a refusal fails the work with
// the refusal under the executor's own epoch. A decision that cannot be
// obtained answers the record as it stands, its lease unextended, with no
// error, so one transient outage interrupts nothing; the store's own checks
// still fence a lapsed or stale lease, and an outage that lasts the lease's
// lifetime ends it at its expiry.
func (s standingStore) Renew(id string, epoch int64, ttl time.Duration) (*job.Record, error) {
	refusal, err := s.p.standingOf(withStandingCall(s.ctx, StandingRenew), id)
	if err != nil {
		// An Update that changes nothing applies the store's terminal, epoch
		// and expiry checks without moving the lease.
		return s.Store.Update(id, epoch, func(*job.Record) error { return nil })
	}
	if refusal != "" {
		if _, err := s.Store.Update(id, epoch, failRefused(refusal)); err != nil {
			return nil, err
		}
		return nil, &standingRefusal{refusal: refusal}
	}
	return s.Store.Renew(id, epoch, ttl)
}

// Update decides before each progress write: a refusal writes the failure in
// its place. A decision that cannot be obtained lets the write through, since
// the renewal that follows refuses to extend the lease.
func (s standingStore) Update(id string, epoch int64, mutate func(*job.Record) error) (*job.Record, error) {
	refusal, err := s.p.standingOf(withStandingCall(s.ctx, StandingProgress), id)
	if err == nil && refusal != "" {
		if _, err := s.Store.Update(id, epoch, failRefused(refusal)); err != nil {
			return nil, err
		}
		return nil, &standingRefusal{refusal: refusal}
	}
	return s.Store.Update(id, epoch, mutate)
}
