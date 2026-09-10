package job

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// immutable is what a record was before a write, for the fields a lease holder
// does not own [JOB-M1].
//
// A lease is the right to record what HAPPENED to the work. It was never the
// right to change what the work IS, and until this existed all three bindings
// disagreed about that: the file and memory bindings ran the caller's closure
// and checked only the envelope afterwards, so a changed spec was written and
// reported as success; the service binding enumerated the fields a write may
// carry, so the same call succeeded and SILENTLY DISCARDED the change. Two
// bindings that answer one call differently are two abstractions.
//
// Which is why this is a value taken before the write rather than three
// enumerations. A guarantee held by a field's absence from a list is a guarantee
// nothing can fail — the same argument [JOB-V4] makes for the envelope, which is
// checked here now so that one write has one refusal.
type immutable struct {
	id        string
	kind      string
	spec      []byte
	createdAt Timestamp
	envelope  *Envelope
}

func immutablesOf(r *Record) immutable {
	return immutable{
		id:        r.ID,
		kind:      r.Kind,
		spec:      append([]byte(nil), r.Spec...),
		createdAt: r.CreatedAt,
		envelope:  r.Envelope.clone(),
	}
}

func (i immutable) unmoved(r *Record) error {
	if i.id != r.ID {
		return immutableMoved("id")
	}
	if i.kind != r.Kind {
		return immutableMoved("kind")
	}
	if !sameOpaque(i.spec, r.Spec) {
		return immutableMoved("spec")
	}
	if !i.createdAt.Time.Equal(r.CreatedAt.Time) {
		return immutableMoved("created_at")
	}
	return envelopeUnmoved(i.envelope, r.Envelope)
}

// The message carries no values: a spec is opaque and arbitrarily large, and a
// refusal that quotes it puts a payload into every log that reads one.
func immutableMoved(field string) error {
	return fmt.Errorf("%w: %s is written once at submit and a lease does not move it", ErrInvalid, field)
}

// sameOpaque compares two opaque payloads as the format defines them.
//
// Not bytes.Equal, and the difference is the whole reason this rule has to hold
// on all three bindings at once. Whitespace INSIDE an opaque value belongs to
// the record format, not to the payload: [JOB-E1] fixes the indent, so the same
// spec is two-space-indented on disk and compact on the wire, and the service
// binding would refuse every ordinary write if it compared what it received
// against what it holds. json.Compact removes only insignificant whitespace, so
// every escape and every number survives it exactly as [JOB-E7] promises.
func sameOpaque(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return false
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}
