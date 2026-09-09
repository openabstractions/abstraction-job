package job

import (
	"errors"
	"testing"
	"time"
)

func recallBindings(t *testing.T) []struct {
	name  string
	store Store
	clock *clock
} {
	t.Helper()
	c := &clock{t: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)}
	files, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files.now = c.now
	memory := NewMemoryStore()
	memory.now = c.now
	return []struct {
		name  string
		store Store
		clock *clock
	}{{"files", files, c}, {"memory", memory, c}}
}

func TestARecallShortensTheLeaseAndTheHolderIsEvictedAtTheDeadline(t *testing.T) {
	for _, b := range recallBindings(t) {
		t.Run(b.name, func(t *testing.T) {
			s, c := b.store, b.clock
			id, _ := s.Submit(sampleRecord())
			held, _ := s.Claim(id, "holder", time.Hour)

			r, err := s.Recall(id, held.Lease.Epoch, "yield", "issuer", 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Lease.Recalled() || r.Lease.Recall.Reason != "yield" || r.Wants() != WantRun {
				t.Fatalf("recall not recorded, or it touched the intent: %+v", r.Lease)
			}
			if !r.Lease.ExpiresAt.Equal(c.t.Add(10 * time.Second)) {
				t.Fatalf("expiry = %v, want the deadline", r.Lease.ExpiresAt)
			}
			if !contains(r.Critical, FeatureRecall) {
				t.Fatalf("a recalled record must declare %s critical: %v", FeatureRecall, r.Critical)
			}

			if _, err := s.Renew(id, held.Lease.Epoch, time.Hour); err != nil {
				t.Fatal(err)
			}
			r, _ = s.Load(id)
			if !r.Lease.ExpiresAt.Equal(c.t.Add(10 * time.Second)) {
				t.Fatalf("renew extended a recalled lease to %v", r.Lease.ExpiresAt)
			}
			if _, err := s.Update(id, held.Lease.Epoch, func(r *Record) error { r.Progress.Done = 8; return nil }); err != nil {
				t.Fatalf("a holder winding down must still checkpoint: %v", err)
			}
			if _, err := s.Claim(id, "holder", time.Hour); !errors.Is(err, ErrLeaseHeld) {
				t.Fatalf("the holder shed the recall by re-claiming: %v", err)
			}

			c.add(11 * time.Second)
			if _, err := s.Update(id, held.Lease.Epoch, func(r *Record) error { return nil }); !errors.Is(err, ErrLeaseExpiry) {
				t.Fatalf("evicted holder wrote: %v", err)
			}
			orphans, _ := s.Orphans()
			if len(orphans) != 1 {
				t.Fatalf("an evicted job is not offered to anyone: %d orphans", len(orphans))
			}
			r, _ = s.Load(id)
			if !r.Lease.Recalled() || r.Lease.Owner != "holder" || r.Progress.Done != 8 {
				t.Fatalf("an evicted record should still say who was asked and what they proved: %+v", r.Lease)
			}

			next, err := s.Claim(id, "successor", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if next.Lease.Recalled() || contains(next.Content, FeatureRecall) {
				t.Fatalf("a new holding inherited the old holder's recall: %+v", next.Lease)
			}
		})
	}
}

func TestAReleaseUnderRecallIsComplianceAndStaysReadable(t *testing.T) {
	for _, b := range recallBindings(t) {
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			id, _ := s.Submit(sampleRecord())
			held, _ := s.Claim(id, "holder", time.Hour)
			if _, err := s.Recall(id, held.Lease.Epoch, "yield", "", time.Minute); err != nil {
				t.Fatal(err)
			}
			if err := s.Release(id, held.Lease.Epoch); err != nil {
				t.Fatal(err)
			}
			r, _ := s.Load(id)
			if r.Lease.Owner != "" || !r.Lease.Recalled() || r.State != StatePending {
				t.Fatalf("complied record = %+v", r.Lease)
			}
			if _, err := s.Recall(id, held.Lease.Epoch, "again", "", time.Minute); !errors.Is(err, ErrLeaseExpiry) {
				t.Fatalf("recall with nobody holding = %v", err)
			}
		})
	}
}

func TestARecallIsRefusedWhereItCannotLand(t *testing.T) {
	for _, b := range recallBindings(t) {
		t.Run(b.name, func(t *testing.T) {
			s := b.store
			id, _ := s.Submit(sampleRecord())
			if _, err := s.Recall(id, 0, "yield", "", time.Minute); !errors.Is(err, ErrLeaseExpiry) {
				t.Fatalf("recall of an unclaimed job = %v", err)
			}
			held, _ := s.Claim(id, "holder", time.Hour)
			if _, err := s.Recall(id, held.Lease.Epoch+1, "yield", "", time.Minute); !errors.Is(err, ErrStaleEpoch) {
				t.Fatalf("recall against an epoch nobody observed = %v", err)
			}
			if _, err := s.Recall(id, held.Lease.Epoch, "  ", "", time.Minute); !errors.Is(err, ErrInvalid) {
				t.Fatalf("recall without a reason = %v", err)
			}
			s.Update(id, held.Lease.Epoch, func(r *Record) error { r.State = StateComplete; return nil })
			if _, err := s.Recall(id, held.Lease.Epoch, "yield", "", time.Minute); !errors.Is(err, ErrTerminal) {
				t.Fatalf("recall of a finished job = %v", err)
			}
		})
	}
}

func TestARecallSurvivesTheWire(t *testing.T) {
	for _, b := range bindings(t) {
		if b.name != "service" {
			continue
		}
		id, _ := b.store.Submit(sampleRecord())
		held, _ := b.store.Claim(id, "holder", time.Hour)
		r, err := b.store.Recall(id, held.Lease.Epoch, "yield", "issuer", time.Minute)
		if err != nil || !r.Lease.Recalled() || r.Lease.Recall.By != "issuer" {
			t.Fatalf("recall over the service binding: %v %+v", err, r)
		}
		if _, err := b.store.Recall(id, held.Lease.Epoch+1, "yield", "", time.Minute); !errors.Is(err, ErrStaleEpoch) {
			t.Fatalf("the refusal class did not survive the wire: %v", err)
		}
	}
}
