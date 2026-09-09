package job

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNoLease is why a hold took nothing when the record it was given carried no
// live lease of its own to borrow a lifetime from.
var ErrNoLease = errors.New("no live lease to hold the machine awake for")

// CanKeepAwake reports whether this machine can be kept awake at all, and says
// what refused when it cannot. A supervisor asks once at startup, exactly as it
// asks what identity this platform can ever prove: a headless container, a VM
// with no logind seat and a locked-down desktop all answer no, and a caller that
// learns this only from a hold that quietly does nothing lets a 40 GB transfer
// sleep.
func CanKeepAwake() error {
	free, err := keepAwake("abstraction", "checking whether this machine can be kept awake")
	if err != nil {
		return fmt.Errorf("this machine cannot be kept awake: %w", err)
	}
	free()
	return nil
}

// Hold keeps the machine from idling into sleep while this process holds a
// lease, and not a moment longer: it is released by Release, by the lease
// ending in the store — released, lapsed, or the job turning terminal — and by
// the operating system if the process dies. The lease is the lifetime; nothing
// here has one of its own.
type Hold struct {
	store Store
	id    string
	epoch int64
	until time.Time
	stop  context.CancelFunc
	gone  chan struct{}
	mu    sync.Mutex
	free  func()
	why   error
}

// KeepAwake takes the platform's idle-sleep inhibitor for the lease r carries.
// A record whose lease is not live holds nothing.
func KeepAwake(s Store, r *Record) *Hold {
	h := &Hold{store: s, id: r.ID, epoch: r.Lease.Epoch, until: r.Lease.ExpiresAt.Time}
	if !h.alive(r) {
		h.why = ErrNoLease
		return h
	}
	free, err := keepAwake(r.Lease.Owner, r.Kind+" "+r.ID)
	if err != nil {
		h.why = fmt.Errorf("this machine cannot be kept awake: %w", err)
		return h
	}
	h.free = free
	h.gone = make(chan struct{})
	ctx, stop := context.WithCancel(context.Background())
	h.stop = stop
	go h.follow(ctx, Watch(s, r.Kind))
	return h
}

func (h *Hold) Held() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.free != nil
}

// Why says why this hold took nothing: ErrNoLease when there was no live lease
// to hold for, and the platform's own refusal when the machine cannot be kept
// awake at all. nil once the hold was taken, including after it ends — a hold
// that was released, or whose lease finished, refused nothing.
//
// Without this the two are one observation, Held() == false, and a caller that
// cannot tell them apart treats a machine it can never hold as a job that is
// already over.
func (h *Hold) Why() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.why
}

// Release lets go and returns once nothing of the hold is still reading the
// store.
func (h *Hold) Release() {
	if h.let() {
		<-h.gone
	}
}

func (h *Hold) let() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.free == nil {
		return false
	}
	h.stop()
	h.free()
	h.free = nil
	return true
}

func (h *Hold) follow(ctx context.Context, sub Subscription) {
	defer close(h.gone)
	defer sub.Close()
	for {
		wait, cancel := context.WithDeadline(ctx, h.until)
		n, err := sub.Next(wait)
		cancel()
		if ctx.Err() != nil {
			return
		}
		var r *Record
		if err == nil {
			r = find(n.Records, h.id)
		} else {
			r, _ = h.store.Load(h.id)
		}
		if !h.alive(r) {
			h.let()
			return
		}
		h.until = r.Lease.ExpiresAt.Time
	}
}

func (h *Hold) alive(r *Record) bool {
	return r != nil && r.Lease.Epoch == h.epoch && r.Lease.Held(time.Now()) && !r.State.Terminal()
}

func find(rs []*Record, id string) *Record {
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	return nil
}
