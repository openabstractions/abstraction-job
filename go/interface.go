package job

import "time"

// Store is where jobs live. Every other abstraction in this project sits on it.
//
// # What this interface is, and what it deliberately is not
//
// It is the SEMANTICS of the lease protocol, and nothing else: a claim is
// exclusive, an epoch only ever increases, every write must present the epoch it
// holds, and a successor may continue only from what a predecessor proved. That
// is what two implementations have to agree about, and not one clause of it
// mentions a byte.
//
// It is NOT a file format and NOT a transport. Records have to be represented
// somehow, and a call has to reach whoever executes it somehow, but both of
// those belong to the BINDING underneath — files in a directory today, a local
// service over a pipe next, a machine across the network after that. Changing
// the representation must be invisible from here.
//
// That distinction was not free. This package's own documentation used to say
// the record "on disk, in JSON" was the contract, which promoted an encoding to
// a contract and was then believed: FileStore's name reached nine public
// signatures, as far up as model.Submit, three layers above anything that should
// know what a file is. Swapping the binding would have been a rewrite of every
// caller rather than a link-time change.
//
// The test an abstraction has to pass is THE SAME APPLICATION, UNCHANGED,
// RUNNING ON TWO BINDINGS. It is the test SLF4J passes and a log file format
// cannot, and this interface exists so that this project can pass it too.
type Store interface {
	// Submit records new work and returns its id. The id is the handle: a plain
	// string that outlives the process which created it.
	Submit(r Record) (string, error)

	// Load reads a record. Any process may do this at any time, including one
	// that holds no lease and never will — that is what makes work observable
	// from outside, which a callback cannot be, because a callback is bound to
	// the lifetime of the process that registered it and that is exactly the
	// lifetime which fails.
	Load(id string) (*Record, error)

	// List returns every job, oldest first.
	List() ([]*Record, error)

	// Claimable reports whether this job can be taken over right now.
	Claimable(r *Record) bool

	// Orphans returns work nobody is doing. This is the reclamation path and it
	// is primary, not a fallback: a process that is killed never hands anything
	// over, so a design that relies on graceful handoff has no answer for the
	// case that loses a 40 GB download.
	Orphans() ([]*Record, error)

	// Claim takes ownership for ttl and returns the record carrying the caller's
	// new epoch. Exclusive: two callers cannot hold the same epoch.
	Claim(id, owner string, ttl time.Duration) (*Record, error)

	// Renew extends a lease the caller still holds. It must refuse once the
	// lease has expired even when the epoch still matches, because a process
	// suspended for an hour wakes up believing it is still the owner.
	Renew(id string, epoch int64, ttl time.Duration) (*Record, error)

	// Release gives up a lease early. A courtesy: everything works without it,
	// only more slowly. It is an Update, so it is refused on a finished job —
	// there is nothing left to hand over, and the lease lapses on its own.
	Release(id string, epoch int64) error

	// Update applies mutate, but only if the caller still holds the lease at the
	// epoch it presents AND the record is not already complete, failed or
	// cancelled. The single gate every change passes through.
	//
	// Terminal is judged on the record as read, so the write that ends a job
	// lands and the one after it does not. An owner therefore puts everything it
	// wants recorded into the same update as the final state.
	Update(id string, epoch int64, mutate func(*Record) error) (*Record, error)

	// SetIntent says what should happen, WITHOUT a lease.
	//
	// The one write here that presents no epoch. Whoever wants a job stopped is
	// not the process doing it — a person clicks cancel while a service on
	// another machine moves the bytes — and requiring a lease would mean
	// stealing the job in order to stop it, which is what the lease prevents.
	//
	// An owner is required to observe this and converge. See Record.Intent for
	// the rules that make that a contract rather than a suggestion.
	SetIntent(id string, want Want, by string) (*Record, error)

	// Recall asks the holder at epoch — the epoch the caller observed, not one
	// it holds — to give the lease back by now+grace, for a reason it can act
	// on. The lease lapses at that deadline either way: that is the fallback,
	// and it is what makes this a demand rather than a suggestion. Not an
	// intent — the user's wish about the job stays where it is.
	Recall(id string, epoch int64, reason, by string, grace time.Duration) (*Record, error)
}

// Scratch is an OPTIONAL capability: a store whose binding happens to BE a local
// filesystem can offer an area on it.
//
// It exists to contain a leak, not to bless one. Callers used to reach through a
// concrete *FileStore for Root() in eight places — to resolve a relative sink,
// to drop a nudge file, to write a heartbeat — and every one of those was the
// binding reaching up through the abstraction. Now they must ask, in one line,
// whether this store has a local area at all:
//
//	if sc, ok := store.(job.Scratch); ok { ... }
//
// A store bound to a service answers no, and the caller has to have a real
// answer for that case rather than assuming a directory exists. When storage
// becomes a layer of its own, deleting this interface is a one-symbol search.
type Scratch interface {
	// Root is the local area this binding keeps its own state in. Relative
	// paths in a record are resolved against it, so that a record written by a
	// PC and read by a NAS names one directory rather than one machine's view
	// of it.
	Root() string
	// WorkPath is the name a single job may spend on scratch. A name, not a
	// shape: the only guarantees are that it is derived from the id, so a
	// successor can find what a predecessor left, and that nothing else in the
	// binding will use it. File or directory is the Kind's choice.
	WorkPath(id string) string
}

// Both assertions are the point of the file: FileStore is now one implementation
// of Store, and its filesystem nature is an optional extra rather than the
// definition.
var (
	_ Store   = (*FileStore)(nil)
	_ Scratch = (*FileStore)(nil)
)
