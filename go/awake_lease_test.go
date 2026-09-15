package job

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// countedHolder grants every hold and counts how many were given back. It is
// this test's own observer: the platform inhibitor is machine-wide, and any
// other process holding it (a download runner in a parallel test package) made
// a released hold indistinguishable from a live one.
type countedHolder struct {
	mu             sync.Mutex
	held, released int
}

type countedHeld struct{ by *countedHolder }

func (c *countedHolder) Hold(who, why string) (Held, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held++
	return countedHeld{c}, nil
}

func (c *countedHolder) counts() (held, released int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held, c.released
}

// Done never closes: nobody takes these holds away.
func (countedHeld) Done() <-chan struct{} { return nil }

func (h countedHeld) Release() error {
	h.by.mu.Lock()
	defer h.by.mu.Unlock()
	h.by.released++
	return nil
}

// leaseEvent bounds how long a hold may take to notice its lease ended. It is a
// deadline on an event, not a sleep: a correct hold closes Done as soon as the
// store changes or the lease's own expiry passes, however loaded the machine.
const leaseEvent = 15 * time.Second

// ends waits for the hold's own end event and checks the holder got its hold back.
func ends(t *testing.T, by *countedHolder, h *Hold, what string) {
	t.Helper()
	select {
	case <-h.Done():
	case <-time.After(leaseEvent):
		t.Fatalf("%s: still held after %s", what, leaseEvent)
	}
	if h.Held() || h.Why() != nil {
		t.Fatalf("%s: held=%v, why=%v", what, h.Held(), h.Why())
	}
	if held, released := by.counts(); held != released {
		t.Fatalf("%s: %d holds taken, %d given back", what, held, released)
	}
}

func TestHoldFollowsLease(t *testing.T) {
	s := NewMemoryStore()
	by := &countedHolder{}

	r := claimed(t, s, time.Minute)
	h := KeepAwakeVia(by, s, r)
	if !h.Held() {
		t.Fatal("claimed and running: not held")
	}
	h.Release()
	ends(t, by, h, "released by the holder")

	r = claimed(t, s, time.Minute)
	h = KeepAwakeVia(by, s, r)
	s.Release(r.ID, r.Lease.Epoch)
	ends(t, by, h, "lease released in the store")

	r = claimed(t, s, time.Minute)
	h = KeepAwakeVia(by, s, r)
	s.Update(r.ID, r.Lease.Epoch, func(rr *Record) error { rr.State = StateFailed; return nil })
	ends(t, by, h, "job terminal")

	// No store change ends this one: only the lease's own expiry does.
	r = claimed(t, s, 300*time.Millisecond)
	h = KeepAwakeVia(by, s, r)
	ends(t, by, h, "lease lapsed")

	r = claimed(t, s, time.Minute)
	s.Release(r.ID, r.Lease.Epoch)
	r, _ = s.Load(r.ID)
	if h = KeepAwakeVia(by, s, r); h.Held() || !errors.Is(h.Why(), ErrNoLease) {
		t.Fatalf("no live lease: held=%v, why=%v", h.Held(), h.Why())
	}
	if held, released := by.counts(); held != 4 || released != 4 {
		t.Fatalf("holds taken %d, given back %d; want 4 and 4", held, released)
	}
}

// The platform path still takes the machine's inhibitor. Only the positive is
// observable: the inhibitor is shared, so its release cannot be attributed.
func TestPlatformHoldTakesTheInhibitor(t *testing.T) {
	needsAnInhibitor(t)
	s := NewMemoryStore()
	h := KeepAwake(s, claimed(t, s, time.Minute))
	defer h.Release()
	if !h.Held() || !inhibited() {
		t.Fatalf("claimed and running: held=%v, inhibited=%v", h.Held(), inhibited())
	}
}
