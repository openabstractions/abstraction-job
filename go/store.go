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
// sharing the directory and is written in job/README.md; nothing above Store
// may depend on it.
type FileStore struct {
	root string
	now  func() time.Time
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

	b, err := r.Encode()
	if err != nil {
		return "", err
	}
	err = cas.Write(s.recordPath(r.ID), nil, b)
	if errors.Is(err, cas.ErrMoved) {
		return "", fmt.Errorf("%w: %s already exists", ErrInvalid, r.ID)
	}
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// Load never takes the lock: any process may read at any time, which is what
// makes work observable from outside.
func (s *FileStore) Load(id string) (*Record, error) {
	b, err := readFile(s.recordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	return Decode(b)
}

func (s *FileStore) change(id string, edit func(*Record) error) (*Record, error) {
	var out *Record
	err := cas.Change(s.recordPath(id), func(cur []byte) ([]byte, error) {
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
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *FileStore) Claimable(r *Record) bool {
	return !r.State.Terminal() && !r.Lease.Held(s.now())
}

// Orphans is the primary reclamation path, not a fallback: a process that is
// killed never hands anything over. Which records qualify is Record.Stranded.
func (s *FileStore) Orphans() ([]*Record, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(all))
	for _, r := range all {
		if s.Claimable(r) && r.Stranded() {
			out = append(out, r)
		}
	}
	return out, nil
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
		if err := mutate(r); err != nil {
			return err
		}
		r.UpdatedAt = At(s.now())
		return nil
	})
}
