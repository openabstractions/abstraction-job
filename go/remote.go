package job

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Remote is a Store reached over a connection instead of a directory.
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
type Remote struct {
	network string
	address string
	timeout time.Duration
}

// Dial returns a store that lives somewhere else.
//
// Nothing is opened yet. A store that connected eagerly would fail at
// construction on a machine whose supervisor is merely not running, and "not
// running" is a normal state that discovery is supposed to handle by choosing a
// different tier.
func Dial(network, address string) *Remote {
	return &Remote{network: network, address: address, timeout: 10 * time.Second}
}

func (r *Remote) do(req request) (response, error) {
	conn, err := net.DialTimeout(r.network, r.address, r.timeout)
	if err != nil {
		return response{}, fmt.Errorf("job: no store at %s: %w", r.address, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(r.timeout))

	b, err := json.Marshal(req)
	if err != nil {
		return response{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return response{}, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return response{}, err
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return response{}, err
	}
	if resp.Kind != kindNone {
		return resp, errorOf(resp.Kind, resp.Error)
	}
	return resp, nil
}

func (r *Remote) record(resp response, err error) (*Record, error) {
	if err != nil {
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(resp.Record, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *Remote) Submit(rec Record) (string, error) {
	b, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	resp, err := r.do(request{Op: "submit", Record: b})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

func (r *Remote) Load(id string) (*Record, error) {
	return r.record(r.do(request{Op: "load", ID: id}))
}

func (r *Remote) List() ([]*Record, error) { return r.records(request{Op: "list"}) }

func (r *Remote) Orphans() ([]*Record, error) { return r.records(request{Op: "orphans"}) }

func (r *Remote) records(req request) ([]*Record, error) {
	resp, err := r.do(req)
	if err != nil {
		return nil, err
	}
	out := make([]*Record, 0, len(resp.Records))
	for _, raw := range resp.Records {
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, err
		}
		out = append(out, &rec)
	}
	return out, nil
}

// Claimable is answered here rather than remotely, and deliberately.
//
// It is a pure predicate over a record and a clock — the caller already holds
// the record, so asking a service would put a round trip in front of an answer
// it can compute. The IDL says the same thing by leaving it off the service.
func (r *Remote) Claimable(rec *Record) bool {
	return !rec.State.Terminal() && !rec.Lease.Held(time.Now())
}

func (r *Remote) Claim(id, owner string, ttl time.Duration) (*Record, error) {
	return r.record(r.do(request{Op: "claim", ID: id, Owner: owner, TTLMS: ttl.Milliseconds()}))
}

func (r *Remote) Renew(id string, epoch int64, ttl time.Duration) (*Record, error) {
	return r.record(r.do(request{Op: "renew", ID: id, Epoch: epoch, TTLMS: ttl.Milliseconds()}))
}

func (r *Remote) Release(id string, epoch int64) error {
	_, err := r.do(request{Op: "release", ID: id, Epoch: epoch})
	return err
}

func (r *Remote) SetIntent(id string, want Want, by string) (*Record, error) {
	return r.record(r.do(request{Op: "set_intent", ID: id, Want: string(want), By: by}))
}

// Update reads, applies the caller's mutation locally, and sends the result
// with the epoch it holds.
//
// The closure cannot cross a process boundary — the thing writing the IDL
// caught — so this is where that shows up in practice. It is safe because of the
// epoch, not in spite of it: if the record moved between the read and the write,
// the server refuses the stale epoch rather than applying the mutation to a
// record the caller never saw.
//
// It is not identical to running the closure on the far side. A mutation whose
// decision depends on state that changed in between is rejected instead of
// silently applied, which is the failure mode worth having, and it is the same
// one the in-process binding has for the same reason.
func (r *Remote) Update(id string, epoch int64, mutate func(*Record) error) (*Record, error) {
	current, err := r.Load(id)
	if err != nil {
		return nil, err
	}
	if err := mutate(current); err != nil {
		return nil, err
	}
	b, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	return r.record(r.do(request{Op: "write", ID: id, Epoch: epoch, Record: b}))
}

// Remote is a Store and, pointedly, not a Scratch.
var _ Store = (*Remote)(nil)
