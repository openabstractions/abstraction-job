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

	cas "github.com/openabstractions/abstraction-cas/go"
)

// FileStore is the file binding of Store: one record per job under
// <root>/jobs/<id>.json, every write a cas.Change on that file, so writers in
// any process on this host apply their edit to the truth. <root>/work/<id> is
// the name job <id> may spend on scratch. The layout is normative for anything
// sharing the directory and is written in abstraction-job/README.md; nothing above Store
// may depend on it.
type FileStore struct {
	root string
	now  func() time.Time
	list listCache
}

var (
	ErrNotFound    = errors.New("job: not found")
	ErrLeaseHeld   = errors.New("job: lease is held by another owner")
	ErrStaleEpoch  = errors.New("job: stale epoch, this owner no longer holds the lease")
	ErrLeaseExpiry = errors.New("job: lease has expired")
	ErrTerminal    = errors.New("job: job is in a terminal state")

	// ErrConflict is a write computed against a record that has since moved.
	//
	// A different question from ErrStaleEpoch, and the two are not
	// interchangeable: the epoch answers "are you still the holder", this
	// answers "is this the record you read". A holder in good standing gets
	// this one, and the answer to it is to read again and redo the work.
	ErrConflict = errors.New("job: the record moved since it was read")
)

// ErrUnreadable names the records a sweep could not decode, and is returned
// ALONGSIDE the ones it could.
//
// A sweep has two independent answers — what it found, and what it could not
// look at — and one (value, error) pair carries only one of them. Returning the
// error alone loses the work a supervisor can still do; returning nil loses the
// fact that it is half blind, and a supervisor that reports zero orphans over a
// directory it cannot read has told everybody it is healthy. So both travel: the
// slice holds every record that decoded, and errors.As recovers the rest.
type ErrUnreadable struct {
	IDs    []string
	Reason error
}

func (e *ErrUnreadable) Error() string {
	return fmt.Sprintf("job: %d record(s) could not be read (%s): %v", len(e.IDs), strings.Join(e.IDs, ", "), e.Reason)
}

func (e *ErrUnreadable) Unwrap() error { return e.Reason }

// note keeps the first reason rather than the last, so Unwrap answers the same
// way whatever order a directory happened to be listed in.
func (e *ErrUnreadable) note(id string, err error) {
	e.IDs = append(e.IDs, id)
	if e.Reason == nil {
		e.Reason = err
	}
}

func (e *ErrUnreadable) orNil() error {
	if len(e.IDs) == 0 {
		return nil
	}
	return e
}

// NewFileStore opens the file store at root and creates jobs/ and work/ when
// they are missing. Every record write takes the cas lock, which holds within
// one host, so a root off this host's local volumes (a UNC path, a network
// drive, a network file system) is refused here with cas's reason before
// anything is created (cas CAS-L2). A record written from two hosts needs a
// protocol this store does not have.
func NewFileStore(root string) (*FileStore, error) {
	if err := cas.LocalPath(root); err != nil {
		return nil, fmt.Errorf("job: %w", err)
	}
	for _, d := range []string{filepath.Join(root, "jobs"), filepath.Join(root, "work")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &FileStore{root: root, now: time.Now}, nil
}

func (s *FileStore) Root() string { return s.root }

// recordPath is the file holding job id's record. An id names one file
// directly under jobs/, so an empty id, one carrying a path separator, and one
// that is not a local name (a "..", a volume, a reserved device name) are
// refused before any path is built from it.
func (s *FileStore) recordPath(id string) (string, error) {
	if err := checkID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.root, "jobs", id+".json"), nil
}

// checkID refuses a record id that would name anything other than one file in
// the store's jobs/ directory.
func checkID(id string) error {
	if id == "" || strings.ContainsAny(id, `/\`) || !filepath.IsLocal(id) || !filepath.IsLocal(id+".json") {
		return fmt.Errorf("%w: record id %q must name one file in the store", ErrInvalid, id)
	}
	return nil
}

// WorkPath is a name, not a shape: derived from the id so a successor finds
// what a predecessor left, and used by nothing else. The store creates work/
// and never this.
func (s *FileStore) WorkPath(id string) string {
	return filepath.Join(s.root, "work", id)
}

// NewID sorts by creation time, so a directory listing is submission order.
func NewID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("job: no randomness available: " + err.Error())
	}
	return fmt.Sprintf("%d-%s", time.Now().UTC().UnixMilli(), hex.EncodeToString(b[:]))
}

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

	path, err := s.recordPath(r.ID)
	if err != nil {
		return "", err
	}
	b, err := r.Encode()
	if err != nil {
		return "", err
	}
	err = cas.Write(path, nil, b)
	if errors.Is(err, cas.ErrMoved) {
		return "", fmt.Errorf("%w: %s already exists", ErrInvalid, r.ID)
	}
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// readRecord reads one record file for Load; tests count reads through it.
var readRecord = readFile

// Load never takes the lock: any process may read at any time, which is what
// makes work observable from outside.
func (s *FileStore) Load(id string) (*Record, error) {
	path, err := s.recordPath(id)
	if err != nil {
		return nil, err
	}
	b, err := readRecord(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return Decode(b)
}

func (s *FileStore) change(id string, edit func(*Record) error) (*Record, error) {
	path, err := s.recordPath(id)
	if err != nil {
		return nil, err
	}
	var out *Record
	err = cas.Change(path, func(cur []byte) ([]byte, error) {
		if cur == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		r, err := Decode(cur)
		if err != nil {
			return nil, err
		}
		if err := edit(r); err != nil {
			return nil, err
		}
		out = r
		return r.Encode()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// List returns every job, oldest first, and *ErrUnreadable naming the ones it
// could not decode. A record whose directory entry shows the size and settled
// write time of the copy List last read is answered from that copy.
func (s *FileStore) List() ([]*Record, error) {
	// Taken before the listing: every write after it carries a later write time.
	now := listNow()
	entries, err := os.ReadDir(filepath.Join(s.root, "jobs"))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	infos := make(map[string]os.FileInfo, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		names = append(names, id)
		// A stat that fails leaves this record read from its file.
		if info, err := e.Info(); err == nil {
			infos[id] = info
		}
	}
	sort.Strings(names)
	out := make([]*Record, 0, len(names))
	var unread ErrUnreadable
	listed := make(map[string]bool, len(names))
	for _, id := range names {
		listed[id] = true
		if r := s.list.cached(id, infos[id], now); r != nil {
			out = append(out, r)
			continue
		}
		r, err := s.Load(id)
		if err != nil {
			unread.note(id, err)
			continue
		}
		s.list.remember(id, infos[id], r, now)
		out = append(out, r)
	}
	s.list.keep(listed)
	return out, unread.orNil()
}

func (s *FileStore) Claimable(r *Record) bool {
	return !r.State.Terminal() && !r.Lease.Held(s.now())
}

// Orphans is the primary reclamation path, not a fallback: a process that is
// killed never hands anything over. Which records qualify is Record.Stranded.
func (s *FileStore) Orphans() ([]*Record, error) {
	all, err := s.List()
	var unread *ErrUnreadable
	if err != nil && !errors.As(err, &unread) {
		return nil, err
	}
	out := make([]*Record, 0, len(all))
	for _, r := range all {
		if s.Claimable(r) && r.Stranded() {
			out = append(out, r)
		}
	}
	return out, err
}

// SetIntent is the one write that presents no epoch: the party who wants a job
// stopped is not the process doing it, and requiring a lease would mean
// stealing the job in order to stop it. Refused once terminal; asking for
// something the owner cannot do is not an error here, only the owner knows.
func (s *FileStore) SetIntent(id string, want Want, by string) (*Record, error) {
	if !want.Valid() {
		return nil, fmt.Errorf("%w: intent %q", ErrInvalid, want)
	}
	return s.change(id, func(r *Record) error {
		if r.State.Terminal() {
			return fmt.Errorf("%w: %s is %s", ErrTerminal, id, r.State)
		}
		r.Intent = &Intent{Want: want, By: by, At: At(s.now())}
		r.UpdatedAt = At(s.now())
		return nil
	})
}

func (s *FileStore) Claim(id, owner string, ttl time.Duration) (*Record, error) {
	seen, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	return s.ClaimFrom(seen, owner, ttl)
}

// ClaimFrom takes the job as the caller last read it: refused with
// ErrLeaseHeld if the record has changed hands since, so a sweeper holding a
// record from Orphans is told rather than quietly claiming a job that is no
// longer the one it decided about. The record written is the one on disk under
// the lock, never the caller's copy, so an intent set since the caller read is
// kept.
func (s *FileStore) ClaimFrom(seen *Record, owner string, ttl time.Duration) (*Record, error) {
	if strings.TrimSpace(owner) == "" {
		return nil, errors.New("job: claim requires an owner")
	}
	if seen == nil {
		return nil, fmt.Errorf("%w: claim needs the record it is claiming", ErrInvalid)
	}
	return s.change(seen.ID, func(r *Record) error {
		if r.Lease.Epoch != seen.Lease.Epoch {
			return fmt.Errorf("%w: the record moved to epoch %d since it was read at %d", ErrLeaseHeld, r.Lease.Epoch, seen.Lease.Epoch)
		}
		if r.State.Terminal() {
			return fmt.Errorf("%w: %s is %s", ErrTerminal, r.ID, r.State)
		}
		now := s.now()
		if r.Lease.Held(now) && (r.Lease.Owner != owner || r.Lease.Recalled()) {
			return fmt.Errorf("%w: %s holds it until %s", ErrLeaseHeld, r.Lease.Owner, r.Lease.ExpiresAt.Format(time.RFC3339))
		}
		r.Lease = Lease{Owner: owner, Epoch: r.Lease.Epoch + 1, ExpiresAt: At(now.Add(ttl))}
		if r.State == StatePending || r.State == StateRunning {
			r.State = StateRunning
		}
		return nil
	})
}

// Renew refuses once the lease has expired even when the epoch still matches:
// a process suspended for an hour wakes up believing it is the owner, and
// making it re-Claim bumps the epoch so any write it had in flight is refused.
func (s *FileStore) Renew(id string, epoch int64, ttl time.Duration) (*Record, error) {
	return s.change(id, func(r *Record) error {
		if r.Lease.Epoch != epoch {
			return fmt.Errorf("%w: record is at epoch %d, caller holds %d", ErrStaleEpoch, r.Lease.Epoch, epoch)
		}
		if !r.Lease.Held(s.now()) {
			return fmt.Errorf("%w: expired at %s, re-claim instead", ErrLeaseExpiry, r.Lease.ExpiresAt.Format(time.RFC3339))
		}
		r.Lease.ExpiresAt = renewedUntil(r.Lease, s.now().Add(ttl))
		return nil
	})
}

// renewedUntil caps a renewal at the recall's deadline; without it a holder
// keeps its lease alive through any recall simply by renewing.
func renewedUntil(l Lease, want time.Time) Timestamp {
	if l.Recall != nil && l.Recall.Until.Before(want) {
		return l.Recall.Until
	}
	return At(want)
}

// Recall's epoch is the one the issuer observed, not one it holds: a recall
// decided against epoch 3 must not land on the holder at epoch 4.
func (s *FileStore) Recall(id string, epoch int64, reason, by string, grace time.Duration) (*Record, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: a recall needs a reason the holder can act on", ErrInvalid)
	}
	return s.change(id, func(r *Record) error {
		return recall(r, s.now(), epoch, reason, by, grace)
	})
}

func recall(r *Record, now time.Time, epoch int64, reason, by string, grace time.Duration) error {
	if r.State.Terminal() {
		return fmt.Errorf("%w: %s is %s", ErrTerminal, r.ID, r.State)
	}
	if r.Lease.Epoch != epoch {
		return fmt.Errorf("%w: record is at epoch %d, recall was decided against %d", ErrStaleEpoch, r.Lease.Epoch, epoch)
	}
	if !r.Lease.Held(now) {
		return fmt.Errorf("%w: nobody holds %s", ErrLeaseExpiry, r.ID)
	}
	until := At(now.Add(grace))
	r.Lease.Recall = &Recall{Reason: reason, By: by, At: At(now), Until: until}
	if until.Before(r.Lease.ExpiresAt.Time) {
		r.Lease.ExpiresAt = until
	}
	r.UpdatedAt = At(now)
	return nil
}

// Release keeps a delegated job running: letting go of the lease means "I am
// not the one watching this any more", and demoting it to pending would tell
// every sweeper to start it over somewhere else.
func (s *FileStore) Release(id string, epoch int64) error {
	_, err := s.Update(id, epoch, func(r *Record) error {
		r.Lease.ExpiresAt = At(s.now())
		r.Lease.Owner = ""
		if r.State == StateRunning && !r.Delegated() {
			r.State = StatePending
		}
		return nil
	})
	return err
}

// Update judges terminal on the record as loaded, never on what mutate leaves
// behind: the write that makes a record complete lands, and the write after it
// does not, so an owner puts everything it wants recorded into the same update
// as the final state.
func (s *FileStore) Update(id string, epoch int64, mutate func(*Record) error) (*Record, error) {
	return s.change(id, func(r *Record) error {
		if r.State.Terminal() {
			return fmt.Errorf("%w: %s is %s", ErrTerminal, id, r.State)
		}
		if r.Lease.Epoch != epoch {
			return fmt.Errorf("%w: record is at epoch %d, caller holds %d", ErrStaleEpoch, r.Lease.Epoch, epoch)
		}
		if !r.Lease.Held(s.now()) {
			return fmt.Errorf("%w: expired at %s", ErrLeaseExpiry, r.Lease.ExpiresAt.Format(time.RFC3339))
		}
		was := immutablesOf(r)
		if err := mutate(r); err != nil {
			return err
		}
		if err := was.unmoved(r); err != nil {
			return err
		}
		r.UpdatedAt = At(s.now())
		return nil
	})
}
