package acceptanceprovider

import (
	"errors"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

// stoppedReason is the yield request a stopping service issues; the inference
// executor lets its jobs go with the same word (abstraction-inference INF-G9).
const stoppedReason = "service:stopped"

// YieldHeld sends a yield request to every lease held on this provider's
// records, with grace as the deadline, and is how a stopping service drains
// [JOB-R9]. Grace zero lapses every lease still held: the store then refuses
// each of the holders' writes at the old epoch, and a successor acquires at
// the next one. The provider issues it as its logical owner.
//
// Only an execution root may yield. Its records belong to this provider's
// executor alone, so every live lease on them is this host's; an
// admission-only root may hold leases of other processes.
//
// An earlier yield request with a deadline at or before grace's is kept, so a
// second call never moves a deadline later. A lease released, lapsed or
// finished since it was read is left alone.
func (p *Provider) YieldHeld(grace time.Duration) error {
	if p.executor == nil {
		return errors.New("acceptance: yielding needs an execution root")
	}
	if grace < 0 {
		grace = 0
	}
	records, err := p.store.List()
	var unread *job.ErrUnreadable
	if err != nil && !errors.As(err, &unread) {
		return err
	}
	problems := []error{}
	if unread != nil {
		problems = append(problems, unread)
	}
	for _, record := range records {
		now := time.Now()
		if record.State.Terminal() || !record.Lease.Held(now) {
			continue
		}
		if recall := record.Lease.Recall; recall != nil && !recall.Until.After(now.Add(grace)) {
			continue
		}
		_, err := p.store.Recall(record.ID, record.Lease.Epoch, stoppedReason, p.config.Owner, grace)
		if err == nil || errors.Is(err, job.ErrStaleEpoch) || errors.Is(err, job.ErrLeaseExpiry) ||
			errors.Is(err, job.ErrTerminal) || errors.Is(err, job.ErrNotFound) {
			continue
		}
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}
