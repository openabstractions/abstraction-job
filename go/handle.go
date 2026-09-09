package job

import (
	"errors"
	"fmt"
	"time"
)

// Job is a handle to work happening somewhere else, and the base capability is
// deliberately small: look at it, and give up on it.
//
// # Why the base is this small
//
// Providers genuinely differ. BITS can suspend a transfer; a plain HTTP fetch in
// a goroutine cannot without inventing a mechanism; a NAS may or may not,
// depending on what is running there. A facade that presents them as
// interchangeable lies to the caller on the tier most people actually run — the
// same reason Record.Requires exists.
//
// So anything beyond "what is it doing" and "stop" is an EXTENDED capability
// that a provider advertises and an application asks for:
//
//	if p, ok := j.(job.Pausable); ok { p.Pause() }
//
// An application that does not ask gets a UI without a pause button, which is
// correct, rather than a pause button that silently does nothing, which is what
// pushing pause into the base interface would have produced.
//
// # A handle is a view, not the work
//
// Record returns what the store says right now. It can be stale the instant it
// returns, because the process that owns this job is not this one and may have
// died between the read and the caller looking at it. Nothing may be cached
// across a decision. This is the same discipline the record itself imposes and
// it is the reason there is no field on this type holding a State.
type Job interface {
	// ID is the durable handle. It survives this process.
	ID() string
	// Record is the current view. Re-read it; never cache it across a decision.
	Record() (*Record, error)
	// Cancel abandons the work on purpose. Idempotent: cancelling an already
	// cancelled job succeeds.
	Cancel() error
}

// Pausable is work that can be stopped and taken up again where it left off, as
// distinct from cancelled.
//
// # Why this is not a separate kind of store
//
// The obvious shape was a store with Pause and Resume methods on it, which is
// what an engine that already has a pause button looks like from outside — and
// it would have been a port of somebody else's feature rather than an
// abstraction. It is wrong twice over: it puts the operation on the STORE when
// the thing being paused is a job, and it makes pausing a different kind of act
// from cancelling when they are the same act — telling an owner what you want,
// when you are not the owner and cannot become one. Both go through SetIntent.
//
// # Why almost everything can do this
//
// Pause looks like a capability only a real transfer engine could have: BITS
// suspends a queue, Lemonade's engine parks a step. But for anything holding a
// checkpoint, pause is stop working, release the lease, and stay out of sweeps
// until the intent changes; resume is set the intent back and become claimable.
// Nothing is lost, because the proven prefix was already durable. The mechanism
// that makes a crash survivable is the same one that makes pausing free — which
// is why this belongs here rather than in each engine that happened to think of
// it, and why the plain file binding implements it.
type Pausable interface {
	Job
	Pause() error
	Resume() error
}

// ErrCancelNeedsOwner is no longer returned by anything here.
//
// It existed because schema 3 could not express "somebody asked this to stop"
// while another process held the lease, so cancelling from outside was refused.
// Schema 4 added Intent and the refusal went with it. Kept as a name so that
// callers written against the old behaviour still compile, and so the reason it
// disappeared is written down next to it rather than only in a commit message.
//
// Deprecated: cancelling from outside now works. Nothing returns this.
var ErrCancelNeedsOwner = errors.New("job: cannot cancel from outside while a lease is held")

// cancelLease is short on purpose. Taking a job over to finish it off touches
// the record once and is done; a long lease would block the real owner from
// coming back if it raced with a claim.
const cancelLease = 15 * time.Second

// Open returns a handle to a job in a store.
//
// owner is who to record as the actor when this handle has to take the lease to
// do something. It is supplied rather than invented: a name this package made up
// would be a constant chosen by the wrong layer, and it would be wrong on every
// machine where two programs used this library.
func Open(s Store, id, owner string) Job { return &handle{store: s, id: id, owner: owner} }

type handle struct {
	store Store
	id    string
	owner string
}

func (h *handle) ID() string { return h.id }

func (h *handle) Record() (*Record, error) { return h.store.Load(h.id) }

// Cancel marks the work abandoned.
//
// A DELEGATED job is marked here and stopped elsewhere: the external system —
// BITS, a NAS — does not participate in the lease and cannot see this record, so
// whoever owns that delegation is responsible for abandoning it when it next
// polls. Marking the record is the request, not the act.
func (h *handle) Cancel() error { return h.ask(WantCancel) }

func (h *handle) Pause() error { return h.ask(WantPause) }

func (h *handle) Resume() error { return h.ask(WantRun) }

// ask records what the caller wants and, when nobody is working on the job,
// carries it out immediately.
//
// The two halves matter equally. Recording the intent is what makes this work
// against a live owner on another machine — that owner converges at its next
// checkpoint. Acting on it directly is what makes cancel feel immediate for the
// common case where the job is merely sitting there: waiting for a supervisor to
// notice a job nobody is running would leave a cancelled download visible for a
// sweep interval, and people press the button again when that happens.
func (h *handle) ask(want Want) error {
	r, err := h.store.Load(h.id)
	if err != nil {
		return err
	}
	if r.State.Terminal() {
		// Idempotent where it can be: asking to cancel something already
		// cancelled is a double click, not an error.
		if want == WantCancel && r.State == StateCancelled {
			return nil
		}
		return fmt.Errorf("%w: %s is already %s", ErrTerminal, h.id, r.State)
	}
	if _, err := h.store.SetIntent(h.id, want, h.owner); err != nil {
		return err
	}
	if want != WantCancel {
		// Pause and resume need no further action here. Pause: the owner stops
		// at its next checkpoint, and until then the bytes it writes are its
		// own business. Resume: the job simply becomes eligible for sweeps again.
		return nil
	}
	// Nobody is working on it, so finish the job off rather than leaving a
	// cancelled-but-running record for a supervisor to tidy up later.
	claimed, err := h.store.Claim(h.id, h.owner, cancelLease)
	if err != nil {
		// Somebody holds it. The intent is recorded and they will honour it —
		// which is the entire reason the intent exists, so this is success.
		return nil
	}
	// The lease is deliberately not released: the job is terminal, so nobody
	// can claim it, and the lease lapses on its own.
	_, err = h.store.Update(h.id, claimed.Lease.Epoch, func(r *Record) error {
		r.State = StateCancelled
		return nil
	})
	return err
}
