package job

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	wire "github.com/openabstractions/abstraction-job/go/rec"
)

// RemoteStore is a Store reached over a connection instead of a directory.
//
// It is the second binding, and it exists so the test that decides this project
// can be run at all: the same application, unchanged, on two of them.
//
// # What a caller can and cannot get from it
//
// Everything in Store, and nothing else. In particular it does NOT implement
// Scratch, and that is the point rather than an omission — there is no local
// area behind it, so a caller that assumed a directory now has to have a real
// answer. That assumption used to be unfalsifiable, because every store was a
// directory.
//
// # What it does not solve
//
// Nothing here makes the transport good. It is one exchange per connection over
// whatever net.Dial accepts, carrying JSON, which is a proof rather than a
// deployment. Making it binary and persistent changes this file and serve.go,
// and must change nothing above them — that is the property, and it is now
// checkable instead of asserted.
type RemoteStore struct {
	network string
	address string
	timeout time.Duration
}

// NewRemoteStore returns a store that lives somewhere else.
//
// Nothing is opened yet. A store that connected eagerly would fail at
// construction on a machine whose supervisor is merely not running, and "not
// running" is a normal state that discovery is supposed to handle by choosing a
// different tier.
func NewRemoteStore(network, address string) *RemoteStore {
	return &RemoteStore{network: network, address: address, timeout: 10 * time.Second}
}

func (r *RemoteStore) do(req wire.Request) (wire.Response, error) {
	conn, err := net.DialTimeout(r.network, r.address, r.timeout)
	if err != nil {
		return wire.Response{}, fmt.Errorf("job: no store at %s: %w", r.address, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(r.timeout))

	if _, err := conn.Write(append(wire.EncodeRequest(&req), '\n')); err != nil {
		return wire.Response{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return wire.Response{}, err
	}
	resp, err := wire.DecodeResponse(line)
	if err != nil {
		return wire.Response{}, err
	}
	if resp.Kind != "" {
		return *resp, errorOf(resp.Kind, resp.Error)
	}
	return *resp, nil
}

func (r *RemoteStore) record(resp wire.Response, err error) (*Record, error) {
	if err != nil {
		return nil, err
	}
	return Decode([]byte(resp.Record))
}

func (r *RemoteStore) Submit(rec Record) (string, error) {
	b, err := rec.EncodeProposal()
	if err != nil {
		return "", err
	}
	resp, err := r.do(wire.Request{Op: "submit", Record: wire.Raw(b)})
	if err != nil {
		return "", err
	}
	return resp.Id, nil
}

func (r *RemoteStore) Load(id string) (*Record, error) {
	return r.record(r.do(wire.Request{Op: "load", Id: id}))
}

func (r *RemoteStore) List() ([]*Record, error) { return r.records(wire.Request{Op: "list"}) }

func (r *RemoteStore) Orphans() ([]*Record, error) { return r.records(wire.Request{Op: "orphans"}) }

func (r *RemoteStore) records(req wire.Request) ([]*Record, error) {
	resp, err := r.do(req)
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(resp.Records))
	unread := ErrUnreadable{IDs: resp.Unreadable}
	if len(unread.IDs) > 0 {
		unread.Reason = errors.New(resp.Error)
	}
	// A record this client cannot read is unreadable for exactly the reason the
	// store's own were: it decoded on the far side and not here, because the two
	// know different features. Failing the whole sweep over one of them would
	// hide every job that did arrive.
	for _, raw := range resp.Records {
		rec, err := Decode([]byte(raw))
		if err != nil {
			unread.note(idIn(raw), err)
			continue
		}
		out = append(out, rec)
	}
	return out, unread.orNil()
}

// idIn names a record that will not decode, so a caller learns WHICH one it is
// missing. A record too broken to carry an id is named by the empty string,
// which is still one entry in the count.
func idIn(raw wire.Raw) string {
	var probe struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &probe)
	return probe.ID
}

// Claimable is answered here rather than remotely, and deliberately.
//
// It is a pure predicate over a record and a clock — the caller already holds
// the record, so asking a service would put a round trip in front of an answer
// it can compute. The IDL says the same thing by leaving it off the service.
func (r *RemoteStore) Claimable(rec *Record) bool {
	return !rec.State.Terminal() && !rec.Lease.Held(time.Now())
}

func (r *RemoteStore) Claim(id, owner string, ttl time.Duration) (*Record, error) {
	return r.record(r.do(wire.Request{Op: "claim", Id: id, Owner: owner, TtlMs: ttl.Milliseconds()}))
}

func (r *RemoteStore) Renew(id string, epoch int64, ttl time.Duration) (*Record, error) {
	return r.record(r.do(wire.Request{Op: "renew", Id: id, Epoch: epoch, TtlMs: ttl.Milliseconds()}))
}

func (r *RemoteStore) Release(id string, epoch int64) error {
	_, err := r.do(wire.Request{Op: "release", Id: id, Epoch: epoch})
	return err
}

func (r *RemoteStore) SetIntent(id string, want Want, by string) (*Record, error) {
	return r.record(r.do(wire.Request{Op: "set_intent", Id: id, Want: string(want), By: by}))
}

func (r *RemoteStore) Recall(id string, epoch int64, reason, by string, grace time.Duration) (*Record, error) {
	return r.record(r.do(wire.Request{Op: "recall", Id: id, Epoch: epoch, Reason: reason, By: by, TtlMs: grace.Milliseconds()}))
}

// Update reads, applies the caller's mutation to what it read, and sends the
// result conditional on the record still being that.
//
// The closure cannot cross a process boundary — the thing writing the IDL
// caught — so the read and the write are two operations here where the other
// bindings have one. The epoch does not close that gap and never did: it does
// not move when its holder writes, so two writers under one lease both present
// it and the later write puts back the fields it did not touch. What closes it
// is the base: the store refuses the write unless the record still equals what
// this caller read, and then the caller reads again and redoes the work.
//
// So the caller's mutation may run more than once, which the in-process
// bindings never do — they block on a lock instead. That is the whole declared
// divergence, and a mutation that keeps a count of its own calls is the only
// thing that can see it.
func (r *RemoteStore) Update(id string, epoch int64, mutate func(*Record) error) (*Record, error) {
	var rec *Record
	var err error
	for attempt := 0; attempt < updateAttempts; attempt++ {
		rec, err = r.reconcile(id, epoch, mutate)
		if !errors.Is(err, ErrConflict) {
			return rec, err
		}
	}
	return nil, err
}

// updateAttempts bounds the retry because an unbounded one never returns, and
// a caller that is told it lost can decide something a loop cannot. Writers
// contending here all hold one lease, so the set is small by construction.
const updateAttempts = 4

func (r *RemoteStore) reconcile(id string, epoch int64, mutate func(*Record) error) (*Record, error) {
	base, err := r.do(wire.Request{Op: "load", Id: id})
	if err != nil {
		return nil, err
	}
	current, err := Decode([]byte(base.Record))
	if err != nil {
		return nil, err
	}
	seen, err := current.Encode()
	if err != nil {
		return nil, err
	}
	if err := mutate(current); err != nil {
		return nil, err
	}
	next, err := current.Encode()
	if err != nil {
		return nil, err
	}
	return r.write(id, epoch, seen, next)
}

func (r *RemoteStore) write(id string, epoch int64, base, next []byte) (*Record, error) {
	return r.record(r.do(wire.Request{Op: "write", Id: id, Epoch: epoch, Base: wire.Raw(base), Record: wire.Raw(next)}))
}

// RemoteStore is a Store and, pointedly, not a Scratch.
var _ Store = (*RemoteStore)(nil)
