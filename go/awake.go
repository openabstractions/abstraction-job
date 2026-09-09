package job

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrNoLease is why a hold took nothing when the record it was given carried no
	// live lease of its own to borrow a lifetime from.
	ErrNoLease = errors.New("no live lease to hold the machine awake for")

	// ErrAbsent, wrapped in a Holder's error, means nobody answered: the hold
	// then follows the contract's default rather than the answer it did not get.
	ErrAbsent = errors.New("nobody answered")

	// ErrTakenAway is why a hold that was granted ended before its lease did: a
	// person revoked the right, or the service keeping it stopped. The client
	// cannot tell those apart, so neither falls back to the platform.
	ErrTakenAway = errors.New("the hold was taken away")
)

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

// Held is a hold somebody keeps on this process's behalf. Done closes when
// they let go of it on their own.
type Held interface {
	Done() <-chan struct{}
	Release() error
}

// Holder decides whether this process may hold the machine awake, and holds it
// when it may. Platform asks nobody. An error wrapping ErrAbsent means nobody
// answered; any other error is a refusal and is honoured.
type Holder interface {
	Hold(who, why string) (Held, error)
}

// Platform takes the operating system's idle-sleep inhibitor in this process.
var Platform Holder = platform{}

type platform struct{}

type held struct {
	free func()
	done chan struct{}
}

func (h held) Done() <-chan struct{} { return h.done }

func (h held) Release() error {
	h.free()
	return nil
}

func (platform) Hold(who, why string) (Held, error) {
	free, err := keepAwake(who, why)
	if err != nil {
		return nil, fmt.Errorf("this machine cannot be kept awake: %w", err)
	}
	return held{free: free, done: make(chan struct{})}, nil
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
func KeepAwake(s Store, r *Record) *Hold { return KeepAwakeVia(Platform, s, r) }

// KeepAwakeVia asks by before it holds. A refusal holds nothing and Why says
// what by said; when nobody answers the hold takes the platform's inhibitor
// itself, decided once, now.
func KeepAwakeVia(by Holder, s Store, r *Record) *Hold {
	h := &Hold{store: s, id: r.ID, epoch: r.Lease.Epoch, until: r.Lease.ExpiresAt.Time, gone: make(chan struct{})}
	if !h.alive(r) {
		h.why = ErrNoLease
		close(h.gone)
		return h
	}
	kept, err := by.Hold(r.Lease.Owner, r.Kind+" "+r.ID)
	if errors.Is(err, ErrAbsent) {
		kept, err = Platform.Hold(r.Lease.Owner, r.Kind+" "+r.ID)
	}
	if err != nil {
		h.why = err
		close(h.gone)
		return h
	}
	h.free = func() { kept.Release() }
	ctx, stop := context.WithCancel(context.Background())
	h.stop = stop
	go h.follow(ctx, Watch(s, r.Kind))
	go func() {
		select {
		case <-kept.Done():
			h.takenAway()
		case <-ctx.Done():
		}
	}()
	return h
}

func (h *Hold) Held() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.free != nil
}

// Why says why this hold took nothing: ErrNoLease when there was no live lease
// to hold for, the holder's refusal, or the platform's own when the machine
// cannot be kept awake at all; ErrTakenAway once a granted hold was ended by
// its holder rather than by its lease. nil while held and after it was released
// or its lease finished — those refused nothing.
//
// Without this the reasons are one observation, Held() == false, and a caller
// that cannot tell them apart treats a machine it can never hold as a job that
// is already over.
func (h *Hold) Why() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.why
}

// Done closes once the hold is over — released, its lease ended, or taken
// away — and is closed already for a hold that took nothing. A runner that
// wants to notice a revoke while its work runs waits here.
func (h *Hold) Done() <-chan struct{} { return h.gone }

// Release lets go and returns once nothing of the hold is still reading the
// store.
func (h *Hold) Release() {
	h.let()
	<-h.gone
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

func (h *Hold) takenAway() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.free == nil {
		return
	}
	h.stop()
	h.free = nil
	h.why = ErrTakenAway
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
