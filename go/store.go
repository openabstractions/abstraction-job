package job

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileStore is the file BINDING of Store: jobs kept as files in a directory.
//
// It is the bottom tier, not the substrate, and the difference is the whole
// argument. It is the default because it needs no daemon, no database, no server
// and no port — a directory is the one thing a Go process, a Python process, a
// Windows service and a machine that has just rebooted can all agree on without
// any of them running at the same time. That is the same reason slog still
// writes to stderr when nothing is configured. It earns this binding its place
// in the chain, not a place in the interface.
//
// Everything below this line — the layout, the epoch tokens, the JSON — is
// private to this binding. Nothing above Store may depend on any of it, and a
// service binding that speaks binary to a daemon must be substitutable here
// without a caller noticing.
//
// Layout:
//
//	<root>/jobs/<id>.json          the record
//	<root>/jobs/<id>.epoch.<n>     claim token for epoch n, created O_EXCL
//	<root>/work/<id>               scratch space for a job that needs it
//
// The claim tokens are the mutual exclusion. Creating a file with O_EXCL is
// atomic on NTFS and on POSIX, so exactly one process can create
// `<id>.epoch.7` and therefore exactly one process can become owner at epoch 7.
// Because each generation has a different filename, a token left behind by a
// process that died blocks nothing: the next claimant simply takes epoch 8.
// That is the whole reason the epoch is in the filename rather than a lockfile
// being taken and released — a lockfile whose holder was SIGKILLed has to be
// broken by a timeout, and breaking locks by timeout is how two owners end up
// writing to one file.
type FileStore struct {
	root string
	now  func() time.Time // injectable so the lease tests are not slow or flaky
}

var (
	ErrNotFound    = errors.New("job: not found")
	ErrLeaseHeld   = errors.New("job: lease is held by another owner")
	ErrStaleEpoch  = errors.New("job: stale epoch, this owner no longer holds the lease")
	ErrLeaseExpiry = errors.New("job: lease has expired")
	ErrTerminal    = errors.New("job: job is in a terminal state")
)

func NewFileStore(root string) (*FileStore, error) {
	for _, d := range []string{filepath.Join(root, "jobs"), filepath.Join(root, "work")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &FileStore{root: root, now: time.Now}, nil
}

func (s *FileStore) Root() string { return s.root }

func (s *FileStore) recordPath(id string) string {
	return filepath.Join(s.root, "jobs", id+".json")
}

func (s *FileStore) epochPath(id string, epoch int64) string {
	return filepath.Join(s.root, "jobs", fmt.Sprintf("%s.epoch.%d", id, epoch))
}

// WorkPath is a scratch location a job may use while it runs. Which Kinds need
// one, and what they put there, is their business; the store only guarantees the
// directory exists and that the path is derived from the id, so a successor can
// find what a predecessor left.
func (s *FileStore) WorkPath(id string) string {
	return filepath.Join(s.root, "work", id)
}

// NewID returns an identifier that sorts by creation time, so listing a
// directory gives jobs in roughly the order they were submitted without anything
// having to record that separately.
func NewID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("job: no randomness available: " + err.Error())
	}
	return fmt.Sprintf("%d-%s", time.Now().UTC().UnixMilli(), hex.EncodeToString(b[:]))
}

// Submit records a new job and returns its id. The id is the handle: it is a
// plain string, it can be written to a config file or handed to another program,
// and the process that submitted the job does not have to be alive for it to
// remain meaningful.
func (s *FileStore) Submit(r Record) (string, error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	now := s.now().UTC()
	r.describe()
	if r.State == "" {
		r.State = StatePending
	}
	r.CreatedAt, r.UpdatedAt = At(now), At(now)
	r.Progress.UpdatedAt = At(now)

	b, err := r.Encode()
	if err != nil {
		return "", err
	}
	// O_EXCL: submitting the same id twice is a bug in the caller, not something
	// to paper over by overwriting a job that may already be running.
	f, err := os.OpenFile(s.recordPath(r.ID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return r.ID, nil
}

// Load reads a record. Any process may do this at any time, including one that
// has no lease and never will — that is what makes progress observable from
// outside, which a callback-based API cannot offer because a callback is bound
// to the lifetime of the process that registered it, and that lifetime is
// exactly the one that fails.
func (s *FileStore) Load(id string) (*Record, error) {
	b, err := readWithRetry(s.recordPath(id))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return Decode(b)
}

// readWithRetry absorbs a transient the file binding creates for itself.
//
// Records are replaced by writing a temporary file and renaming over the
// original, so a reader sees the old record or the new one and never half of
// one. That is true on both platforms. What is ALSO true, only on Windows, is
// that a reader opening the file during the replace gets a sharing violation
// rather than either version — MoveFileEx and CreateFile briefly cannot both
// have it.
//
// So the guarantee this layer makes — any process may read at any time, which
// is what makes work observable from outside — was not quite true on Windows.
// It failed rarely enough to look like a flake and often enough to be real: a
// UI polling a running download, `dl list` while a supervisor writes, or a test
// reading a record every 100ms.
//
// This is the binding absorbing its own artefact, which is where it belongs.
// Nothing above job.Store should ever learn that a rename has a window.
func readWithRetry(path string) ([]byte, error) {
	var err error
	for attempt := 0; attempt < 20; attempt++ {
		var b []byte
		b, err = os.ReadFile(path)
		if err == nil {
			return b, nil
		}
		if os.IsNotExist(err) {
			return nil, err
		}
		// Anything else is treated as possibly transient. On platforms where it
		// is not, this costs one extra syscall on a path that was going to fail
		// anyway; the alternative is naming a Windows error number in the
		// generic file store, which would be worse.
		time.Sleep(5 * time.Millisecond)
	}
	return nil, err
}

// List returns every job, oldest first.
func (s *FileStore) List() ([]*Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "jobs"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	out := make([]*Record, 0, len(names))
	for _, id := range names {
		r, err := s.Load(id)
		if err != nil {
			// One unreadable record must not hide every other job.
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Claimable reports whether this job is available to be taken over right now.
// A job is claimable when it is not finished and nobody holds a live lease on
// it — which covers both the polite case (the previous owner released it on the
// way out) and the one that actually matters: the previous owner was killed,
// the machine lost power, and nothing was released at all.
func (s *FileStore) Claimable(r *Record) bool {
	return !r.State.Terminal() && !r.Lease.Held(s.now())
}

// Orphans returns the jobs no one is working on. This is the reclamation path,
// and it is the primary mechanism rather than a fallback: a process that is
// SIGKILLed, or a machine that loses power, never gets to hand anything over, so
// a design that relies on graceful handoff has no answer for the case that
// loses a 40 GB download. Handing off on exit is only an optimisation — it
// releases the lease early so the next owner starts in seconds instead of
// waiting out the expiry.
//
// A TRANSFERRED job is not an orphan, and the difference cost a NAS 313 MB.
// Transferred is not terminal — deliberately, because the requester still has
// to take delivery — so `Claimable` says yes to it, and a supervisor sweeping
// for stranded work saw a finished, digest-proven job with a lapsed lease and
// downloaded the whole thing again. Then again 30 seconds later, forever. What
// a transferred job is waiting for is an acknowledgement, and no amount of
// re-downloading produces one.
func (s *FileStore) Orphans() ([]*Record, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(all))
	for _, r := range all {
		// A PAUSED job is not an orphan either, and for the same reason a
		// transferred one is not: it looks abandoned and is not. Somebody asked
		// it to stop, so the lease was released deliberately — and a sweep that
		// adopted it would start it again seconds after a person pressed pause,
		// which is a worse failure than never having offered pause at all.
		if s.Claimable(r) && r.State != StateTransferred && !r.Paused() {
			out = append(out, r)
		}
	}
	return out, nil
}

// SetIntent records what should happen, without a lease.
//
// The only write in this interface that presents no epoch, and deliberately: the
// party who wants a job stopped is not the process doing it, and requiring a
// lease would mean stealing the job in order to stop it — the one thing the
// lease exists to prevent.
//
// Idempotent, and refused once the job is terminal, because nothing reopens
// finished work. Asking for something the current owner cannot do is NOT an
// error here: only the owner knows what it can do, so only the owner can say so.
func (s *FileStore) SetIntent(id string, want Want, by string) (*Record, error) {
	if !want.Valid() {
		return nil, fmt.Errorf("%w: intent %q", ErrInvalid, want)
	}
	r, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if r.State.Terminal() {
		return nil, fmt.Errorf("%w: %s is %s", ErrTerminal, id, r.State)
	}
	r.Intent = &Intent{Want: want, By: by, At: At(s.now())}
	r.UpdatedAt = At(s.now())
	if err := s.write(r); err != nil {
		return nil, err
	}
	return r, nil
}

// Claim takes ownership of a job for ttl, and returns the record carrying the
// caller's new epoch. Every subsequent write must present that epoch.
func (s *FileStore) Claim(id, owner string, ttl time.Duration) (*Record, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("job: claim requires an owner")
	}
	r, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if r.State.Terminal() {
		return nil, fmt.Errorf("%w: %s is %s", ErrTerminal, id, r.State)
	}
	now := s.now()
	if r.Lease.Held(now) && r.Lease.Owner != owner {
		return nil, fmt.Errorf("%w: %s holds it until %s", ErrLeaseHeld, r.Lease.Owner, r.Lease.ExpiresAt.Format(time.RFC3339))
	}

	// The atomic step. Whoever creates this file is the owner at this epoch, and
	// nobody else can be, because the filesystem will not create it twice.
	tok, next, err := s.takeEpoch(id, r.Lease.Epoch+1)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(tok, "%s\n", owner)
	tok.Close()

	r.Lease = Lease{Owner: owner, Epoch: next, ExpiresAt: At(now.Add(ttl))}
	if r.State == StatePending || r.State == StateRunning {
		r.State = StateRunning
	}
	if err := s.write(r); err != nil {
		return nil, err
	}

	// The previous epoch's token can go now. Nobody will ever ask for it again:
	// a claimant computes its epoch from the RECORD, which now says `next`.
	//
	// Left alone these accumulate one file per claim, forever. A real store had
	// 1069 files for 17 jobs, and a single job that was reconciled every five
	// seconds contributed 217 of them.
	if next > 1 {
		_ = os.Remove(s.epochPath(id, next-1))
	}
	return r, nil
}

// claimHandover is how long a claim token may exist without its record having
// caught up before the claimant that made it is presumed gone.
//
// The two writes are microseconds apart in the same function, so anything on
// this scale is a crash rather than a slow disk — and generous even for a store
// on a share.
const claimHandover = 10 * time.Second

// takeEpoch creates the claim token for the first epoch at or after `from` that
// nobody holds, and returns which one it got.
//
// # Why this is not simply "create the next one"
//
// It was, and a job could be bricked by it. The token is created BEFORE the
// record is written, so a process that dies in between leaves a token for an
// epoch the record never reached. Every later claim then computes the same next
// epoch, finds that token, and fails — permanently. The job cannot be claimed,
// so it cannot be updated, cancelled, adopted or finished by anyone, ever.
//
// Seen on a live store: record at epoch 216, a token for 217, and a supervisor
// reporting a healthy sweep every five seconds while that job silently failed
// its claim. Setting an intent on it did nothing either, because honouring an
// intent requires claiming first.
//
// All three implementations documented this as impossible — "a token left
// behind by a process that was killed blocks nothing, the next claimant takes
// the epoch after" — which is true only when the record DID advance. The
// unhandled case is a token that is AHEAD of its record.
//
// # Why skipping is safe, and when it is not
//
// Skipping past a held epoch would break the exclusivity the token exists to
// provide, so freshness decides. A claimant that is genuinely mid-flight wrote
// its token moments ago and is about to write the record; this claim must lose
// to it. A token that has sat there while the record stayed behind belongs to
// nobody, and the epoch is unusable rather than held.
//
// The file's own timestamp is the evidence, and it is compared against real
// time rather than the store's clock: an injected test clock says nothing about
// when the filesystem wrote a file.
// ageOn reports how old a file is according to the clock that stamped it.
//
// The obvious time.Since(modtime) compares two clocks: the one on whatever
// holds the store, and this machine's. On a share those are different
// machines. A store host running behind by more than claimHandover makes every
// freshly written token look abandoned, so two claimants skip past each other
// and both start moving bytes -- the exact thing the epoch exists to prevent.
// The Update epoch check refuses one of them eventually, but only after the
// work is duplicated, and nothing reports it.
//
// So the age is measured against a mark the store itself just made. When that
// cannot be done the answer is "too recent to touch", because refusing a claim
// costs a retry and taking one wrongly costs correctness.
func (s *FileStore) ageOn(mod time.Time) time.Duration {
	probe, err := os.CreateTemp(filepath.Join(s.root, "jobs"), ".now-*")
	if err != nil {
		return 0
	}
	name := probe.Name()
	probe.Close()
	defer os.Remove(name)

	st, err := os.Stat(name)
	if err != nil {
		return 0
	}
	return st.ModTime().Sub(mod)
}

func (s *FileStore) takeEpoch(id string, from int64) (*os.File, int64, error) {
	const maxSkip = 64
	for next := from; next < from+maxSkip; next++ {
		tok, err := os.OpenFile(s.epochPath(id, next), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return tok, next, nil
		}
		if !os.IsExist(err) {
			return nil, 0, err
		}
		st, serr := os.Stat(s.epochPath(id, next))
		if serr != nil || s.ageOn(st.ModTime()) < claimHandover {
			return nil, 0, fmt.Errorf("%w: epoch %d was taken by someone else", ErrLeaseHeld, next)
		}
		// Abandoned. Take the epoch after it, and leave the orphan for the
		// cleanup that runs on a successful claim.
	}
	return nil, 0, fmt.Errorf("%w: %d epochs from %d are all spoken for", ErrLeaseHeld, maxSkip, from)
}

// Renew extends a lease the caller still holds.
//
// It refuses once the lease has expired, even when the epoch still matches and
// nobody else has claimed. That is deliberate and it is the sleep case: a
// process suspended for an hour wakes up believing it is still the owner. Making
// it re-Claim — which bumps the epoch — means any write it had in flight is
// refused, instead of being accepted on top of bytes a different owner may since
// have written.
func (s *FileStore) Renew(id string, epoch int64, ttl time.Duration) (*Record, error) {
	r, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if r.Lease.Epoch != epoch {
		return nil, fmt.Errorf("%w: record is at epoch %d, caller holds %d", ErrStaleEpoch, r.Lease.Epoch, epoch)
	}
	if !r.Lease.Held(s.now()) {
		return nil, fmt.Errorf("%w: expired at %s, re-claim instead", ErrLeaseExpiry, r.Lease.ExpiresAt.Format(time.RFC3339))
	}
	r.Lease.ExpiresAt = At(s.now().Add(ttl))
	if err := s.write(r); err != nil {
		return nil, err
	}
	return r, nil
}

// Release gives up a lease early so the job can be picked up immediately. This
// is the graceful-handoff path, and it is a courtesy: everything still works
// without it, just more slowly.
func (s *FileStore) Release(id string, epoch int64) error {
	_, err := s.Update(id, epoch, func(r *Record) error {
		r.Lease.ExpiresAt = At(s.now())
		r.Lease.Owner = ""
		// A delegated job stays RUNNING. Letting go of the lease means "I am not
		// the one watching this any more", not "this stopped" — the work is
		// going on inside BITS, or on a NAS, and demoting it to pending tells
		// every supervisor that sweeps for stranded work to start it over
		// somewhere else. Delegating is supposed to release the lease
		// immediately, so this is not an edge case; it is what delegation does.
		if r.State == StateRunning && !r.Delegated() {
			r.State = StatePending
		}
		return nil
	})
	return err
}

// Update applies mutate to the record, but only if the caller still holds the
// lease at the epoch it presents.
//
// This is the single gate through which every change passes, so there is exactly
// one place where staleness is checked rather than one per call site.
func (s *FileStore) Update(id string, epoch int64, mutate func(*Record) error) (*Record, error) {
	r, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if r.Lease.Epoch != epoch {
		return nil, fmt.Errorf("%w: record is at epoch %d, caller holds %d", ErrStaleEpoch, r.Lease.Epoch, epoch)
	}
	if !r.Lease.Held(s.now()) {
		return nil, fmt.Errorf("%w: expired at %s", ErrLeaseExpiry, r.Lease.ExpiresAt.Format(time.RFC3339))
	}
	if err := mutate(r); err != nil {
		return nil, err
	}
	r.UpdatedAt = At(s.now())
	if err := s.write(r); err != nil {
		return nil, err
	}
	return r, nil
}

// write replaces the record atomically. A reader that opens the file at any
// moment sees either the old record or the new one, never half of one — which
// matters because the readers are other processes, and one of them may be
// deciding right now whether this job is an orphan.
func (s *FileStore) write(r *Record) error {
	b, err := r.Encode()
	if err != nil {
		return err
	}
	dir := filepath.Join(s.root, "jobs")
	tmp, err := os.CreateTemp(dir, r.ID+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.recordPath(r.ID))
}
