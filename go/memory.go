package job

import (
	"fmt"
	"sync"
	"time"
)

// Memory is a store that keeps jobs in memory, and it exists to answer an
// objection to the service binding.
//
// # Why this and not just the socket
//
// The service binding proves a caller can work without a directory. It does not
// prove the SEMANTICS are separable, because the thing behind the socket is a
// FileStore — swap the transport, keep the implementation. A fair reading of
// that is "one implementation, two ways in".
//
// This shares no code with FileStore. The lease, the epoch, the exclusivity, the
// refusal of a stale write, the rule that a paused job is not an orphan — all of
// it is written again here, against a map. It passes the same contract test, and
// that is the difference between claiming the semantics are the contract and
// showing it.
//
// # What it is honestly for
//
// Tests, and an application that wants job semantics inside one process without
// a directory. It is NOT durable, which is the one promise it cannot make, and
// the promise the whole project is about — so anything that needs work to
// outlive its process must not be handed one of these. That is a real limit and
// it is not hidden: there is nowhere for a successor to look, because there is
// no successor.
type Memory struct {
	mu      sync.Mutex
	records map[string]*Record
	now     func() time.Time
}

func NewMemory() *Memory {
	return &Memory{records: map[string]*Record{}, now: time.Now}
}

// copyOf hands out a copy rather than the stored pointer.
//
// Not defensive habit: callers mutate what Load returns — that is what Update's
// closure does — and a store that handed out its own pointer would have those
// mutations land without an epoch ever being checked. The file binding gets this
// for free by decoding fresh bytes every time, which is the kind of accident
// that makes a second implementation worth writing.
func copyOf(r *Record) *Record {
	if r == nil {
		return nil
	}
	c := *r
	if r.Spec != nil {
		c.Spec = append([]byte(nil), r.Spec...)
	}
	if r.Checkpoint != nil {
		c.Checkpoint = append([]byte(nil), r.Checkpoint...)
	}
	if r.Delegation != nil {
		d := *r.Delegation
		c.Delegation = &d
	}
	if r.Intent != nil {
		i := *r.Intent
		c.Intent = &i
	}
	if r.Requires != nil {
		c.Requires = append([]string(nil), r.Requires...)
	}
	return &c
}

func (m *Memory) Submit(r Record) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ID == "" {
		r.ID = NewID()
	}
	if _, taken := m.records[r.ID]; taken {
		return "", fmt.Errorf("%w: %s already exists", ErrInvalid, r.ID)
	}
	now := m.now().UTC()
	r.Schema = SchemaVersion
	if r.State == "" {
		r.State = StatePending
	}
	r.CreatedAt, r.UpdatedAt = At(now), At(now)
	r.Progress.UpdatedAt = At(now)
	if err := r.Validate(); err != nil {
		return "", err
	}
	m.records[r.ID] = copyOf(&r)
	return r.ID, nil
}

func (m *Memory) Load(id string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return copyOf(r), nil
}

func (m *Memory) List() ([]*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.records))
	for id := range m.records {
		ids = append(ids, id)
	}
	// Oldest first. Ids sort by creation time, which is why they are shaped the
	// way they are — the file binding gets this from a directory listing.
	sortStrings(ids)
	out := make([]*Record, 0, len(ids))
	for _, id := range ids {
		out = append(out, copyOf(m.records[id]))
	}
	return out, nil
}

func (m *Memory) Claimable(r *Record) bool {
	return !r.State.Terminal() && !r.Lease.Held(m.now()) && !r.Paused()
}

func (m *Memory) Orphans() ([]*Record, error) {
	all, err := m.List()
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(all))
	for _, r := range all {
		if m.Claimable(r) && r.State != StateTransferred {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *Memory) Claim(id, owner string, ttl time.Duration) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if r.State.Terminal() {
		return nil, fmt.Errorf("%w: %s is %s", ErrTerminal, id, r.State)
	}
	now := m.now()
	if r.Lease.Held(now) && r.Lease.Owner != owner {
		return nil, fmt.Errorf("%w: %s holds it", ErrLeaseHeld, r.Lease.Owner)
	}
	r.Lease = Lease{Owner: owner, Epoch: r.Lease.Epoch + 1, ExpiresAt: At(now.Add(ttl))}
	if r.State == StatePending || r.State == StateRunning {
		r.State = StateRunning
	}
	r.UpdatedAt = At(now)
	return copyOf(r), nil
}

func (m *Memory) Renew(id string, epoch int64, ttl time.Duration) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if r.Lease.Epoch != epoch {
		return nil, fmt.Errorf("%w: record is at %d, caller holds %d", ErrStaleEpoch, r.Lease.Epoch, epoch)
	}
	if !r.Lease.Held(m.now()) {
		return nil, fmt.Errorf("%w: re-claim instead", ErrLeaseExpiry)
	}
	r.Lease.ExpiresAt = At(m.now().Add(ttl))
	return copyOf(r), nil
}

func (m *Memory) Release(id string, epoch int64) error {
	_, err := m.Update(id, epoch, func(r *Record) error {
		r.Lease.ExpiresAt = At(m.now())
		r.Lease.Owner = ""
		if r.State == StateRunning && !r.Delegated() {
			r.State = StatePending
		}
		return nil
	})
	return err
}

func (m *Memory) Update(id string, epoch int64, mutate func(*Record) error) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.records[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if stored.Lease.Epoch != epoch {
		return nil, fmt.Errorf("%w: record is at %d, caller holds %d", ErrStaleEpoch, stored.Lease.Epoch, epoch)
	}
	if !stored.Lease.Held(m.now()) {
		return nil, fmt.Errorf("%w: expired", ErrLeaseExpiry)
	}
	working := copyOf(stored)
	if err := mutate(working); err != nil {
		return nil, err
	}
	working.UpdatedAt = At(m.now())
	if err := working.Validate(); err != nil {
		return nil, err
	}
	m.records[id] = working
	return copyOf(working), nil
}

func (m *Memory) SetIntent(id string, want Want, by string) (*Record, error) {
	if !want.Valid() {
		return nil, fmt.Errorf("%w: intent %q", ErrInvalid, want)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if r.State.Terminal() {
		return nil, fmt.Errorf("%w: %s is %s", ErrTerminal, id, r.State)
	}
	now := m.now()
	r.Intent = &Intent{Want: want, By: by, At: At(now)}
	r.UpdatedAt = At(now)
	return copyOf(r), nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Memory is a Store and, like the service binding, not a Scratch: there is no
// local area, and a caller that assumed one has to find out.
var _ Store = (*Memory)(nil)
