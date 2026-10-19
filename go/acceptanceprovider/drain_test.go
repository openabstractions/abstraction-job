package acceptanceprovider

import (
	"errors"
	"testing"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

// TestAStoppingHostYieldsItsLeasesAndTheSuccessorTakesTheNextEpoch holds
// JOB-R9: a stopping host's yield request bounds every lease it holds, a
// second request never moves the deadline later, grace zero lapses what is
// still held, and the holder's write at the old epoch is refused once the
// successor acquires.
func TestAStoppingHostYieldsItsLeasesAndTheSuccessorTakesTheNextEpoch(t *testing.T) {
	root := t.TempDir()
	p, err := OpenManaged(root, testExecutor{profile: "drain-v1", prefix: "private/"})
	if err != nil {
		t.Fatal(err)
	}
	running := accept(t, p.Bind("app"), submission(p, "running"))
	finished := accept(t, p.Bind("app"), submission(p, "finished"))
	waiting := accept(t, p.Bind("app"), submission(p, "waiting"))
	held, err := p.store.Claim(running.OperationID, "old-runtime", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	done, err := p.store.Claim(finished.OperationID, "old-runtime", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.store.Update(done.ID, done.Lease.Epoch, func(r *job.Record) error { r.State = job.StateComplete; return nil }); err != nil {
		t.Fatal(err)
	}

	before := time.Now()
	if err := p.YieldHeld(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	record, err := p.store.Load(held.ID)
	if err != nil {
		t.Fatal(err)
	}
	recall := record.Lease.Recall
	if recall == nil || recall.Reason != stoppedReason || recall.By != p.LogicalOwner() || record.Lease.Epoch != held.Lease.Epoch {
		t.Fatalf("no yield request from the provider on the held lease: %+v", record.Lease)
	}
	if record.Lease.ExpiresAt.After(before.Add(3*time.Second+time.Second)) || !record.Lease.Held(time.Now()) {
		t.Fatalf("the yield request did not bound the lease to the drain: %+v", record.Lease)
	}
	if _, err := p.store.Renew(held.ID, held.Lease.Epoch, time.Hour); err != nil {
		t.Fatalf("the holder may renew and checkpoint until the deadline: %v", err)
	}
	if renewed, _ := p.store.Load(held.ID); renewed.Lease.ExpiresAt.After(recall.Until.Time) {
		t.Fatal("a renew moved the lease past the drain deadline")
	}
	for _, id := range []string{finished.OperationID, waiting.OperationID} {
		if other, _ := p.store.Load(id); other.Lease.Recall != nil {
			t.Fatalf("%s is finished or unheld and got a yield request", id)
		}
	}

	// A later, longer request keeps the earlier deadline.
	if err := p.YieldHeld(time.Hour); err != nil {
		t.Fatal(err)
	}
	if again, _ := p.store.Load(held.ID); !again.Lease.Recall.Until.Equal(recall.Until.Time) {
		t.Fatalf("a second yield request moved the deadline from %s to %s", recall.Until, again.Lease.Recall.Until)
	}

	// The bound: whatever is still held lapses, and the successor acquires at
	// the next epoch.
	if err := p.YieldHeld(0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.store.Update(held.ID, held.Lease.Epoch, func(*job.Record) error { return nil }); !errors.Is(err, job.ErrLeaseExpiry) {
		t.Fatalf("a write after the drain bound: %v, want a lapsed lease", err)
	}
	successor, err := OpenManaged(root, testExecutor{profile: "drain-v1", prefix: "private/"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := successor.store.Claim(held.ID, "new-runtime", time.Minute)
	if err != nil || next.Lease.Epoch != held.Lease.Epoch+1 || next.Lease.Recall != nil {
		t.Fatalf("successor acquisition: %+v %v", next, err)
	}
	if _, err := p.store.Update(held.ID, held.Lease.Epoch, func(r *job.Record) error { r.State = job.StateComplete; return nil }); !errors.Is(err, job.ErrStaleEpoch) {
		t.Fatalf("the old runtime's write at epoch %d: %v, want stale epoch", held.Lease.Epoch, err)
	}
	admissionOnly, err := Open(t.TempDir(), "logical-owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := admissionOnly.YieldHeld(0); err == nil {
		t.Fatal("an admission-only root yielded leases other processes may hold")
	}
}
