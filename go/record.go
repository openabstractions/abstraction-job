// Package job is a handle to work that is happening somewhere else.
//
// Every other abstraction in this project sits on it. The reason it exists is
// forced rather than stylistic: implementations may live in another process (a
// system service) or on another machine (a NAS), so the thing an application
// holds is not the work. It is a reference to work, and the work outlives the
// application that asked for it. Once that is true, sleeping, crashing and
// restarting stop being special cases — they are all just the owner going away.
//
// What crosses the boundary is a reference, not a closure and not a future —
// both of those die with the process that made them, and this record has to
// survive exactly that.
//
// The cross-language contract is the SEMANTICS, not this struct's bytes: a claim
// is exclusive, an epoch only increases, a successor resumes only from what a
// predecessor proved, and a provider may not change what an operation means. A
// Go program can hand work to a Python program because both implement those
// rules, not because both parse the same JSON. How a record is represented, and
// how it reaches the other side, belong to the binding underneath — see Store.
//
// An earlier version of this comment said the record "on disk, in JSON" WAS the
// contract. That sentence was believed, and it cost: an encoding was promoted to
// a contract, the filesystem became the IPC by default, and the name FileStore
// reached nine public signatures as far up as model.Get.
//
// # What this package does NOT know
//
// It does not know what the work IS. Spec and Checkpoint are opaque here, and
// Kind says who can read them. A download's artifact, sources and destination
// live in a download's spec, not in this file.
//
// That separation is the whole reason this record can settle. An earlier version
// carried artifact, sources and sink directly, which meant every improvement to
// downloading — mirrors, chunk manifests, webseeds — forced a schema change on a
// record that Go, Python and eventually C++ all have to agree about. Google's
// long-running operations reached the same conclusion with an opaque `metadata`
// typed by the method that created it.
package job

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is bumped when a change would stop an older reader from safely
// continuing a job written by a newer writer. Readers must refuse a record whose
// schema they do not know rather than guess at it: a misread record means two
// processes disagreeing about how much work is done, which is the one failure
// this design exists to prevent.
//
// History, kept because each bump cost something and the next one should have to
// justify itself against these:
//
//	1  first cut.
//	2  added Delegation. v1 assumed whoever held the lease was also moving the
//	   bytes; Windows BITS takes the whole job, works under its own service
//	   account while every process that asked is closed, and hands back only a
//	   GUID. A v1 reader would have seen no progress and concluded it had stalled.
//	3  moved artifact, sources and sink out into an opaque Spec, and generalised
//	   Progress. Those were download concepts sitting in the generic layer, so
//	   download could not evolve without changing everyone's schema. This is the
//	   bump that is supposed to stop the bumps.
//	4  added Intent. It had to be a bump rather than an optional extra: a reader
//	   that does not know this field keeps working on a job somebody asked to
//	   stop, which is precisely the partly-understood record the schema check
//	   exists to refuse. Note what it is NOT — it is not in Spec, because Spec is
//	   opaque to this layer and every reader must understand a stop request.
const SchemaVersion = 4

// SchemaReadable are the versions this implementation can safely continue.
//
// Version 3 is readable because an absent intent means RUN, which is exactly
// what version 3 always meant — so nothing is being guessed at. Stores full of
// version 3 records exist on real disks and on a NAS; refusing them would orphan
// work in flight, which is the same reason ~/.modelget is still honoured.
//
// The reverse is not true and must not be made true: a version 3 reader has to
// refuse version 4, because it would silently ignore a cancellation.
var SchemaReadable = []int{3, 4}

func schemaReadable(v int) bool {
	for _, ok := range SchemaReadable {
		if v == ok {
			return true
		}
	}
	return false
}

// Timestamp is a time that always serialises identically, in UTC, with exactly
// six fractional digits and a trailing Z.
//
// It exists because Go's default encoding for time.Time is RFC3339Nano, which
// TRIMS trailing zeros: Go wrote "…T06:23:11.22275Z" for the same instant that
// Python wrote as "…T06:23:11.222750Z". Both are valid RFC 3339 and both parse
// correctly, so nothing failed — the record simply changed bytes every time
// the two implementations took turns, and a diff of a job's history became
// meaningless. The cross-language conformance test caught it; no unit test in
// either language could have, because each was self-consistent.
//
// Six digits rather than nine because Python's datetime holds microseconds and
// cannot represent nanoseconds. The contract is set by the least precise
// participant, not the most.
type Timestamp struct{ time.Time }

const timestampLayout = "2006-01-02T15:04:05.000000Z"

func At(t time.Time) Timestamp { return Timestamp{t.UTC().Truncate(time.Microsecond)} }

func (t Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + t.Time.UTC().Format(timestampLayout) + `"`), nil
}

func (t *Timestamp) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		t.Time = time.Time{}
		return nil
	}
	// Accept anything RFC 3339 on the way in — another implementation may be
	// less careful than this one, and refusing to read a job over a timezone
	// suffix would be absurd. Only what we WRITE is pinned.
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("%w: timestamp %q: %v", ErrInvalid, s, err)
	}
	t.Time = parsed.UTC()
	return nil
}

// State is where a job is. The states that matter are the ones that exist
// because the process doing the work and the process that wants the result are
// not the same process — see StateTransferred.
type State string

const (
	// StatePending has been submitted and nobody is working on it.
	StatePending State = "pending"
	// StateRunning has a live lease, or a live delegation.
	StateRunning State = "running"
	// StateTransferred is finished and proven, but the result has not been
	// taken delivery of.
	//
	// Not bureaucracy. BITS will not hand over a file until Complete() is
	// called, and a job left unacknowledged sits in its queue for 90 days; the
	// survey found this same two-phase shape everywhere the worker and the
	// consumer are different processes. Collapse the two states and you cannot
	// express "the service finished this while the app was closed", which is
	// the case this project exists for.
	StateTransferred State = "transferred"
	// StateComplete has been taken delivery of. Terminal.
	StateComplete State = "complete"
	// StateFailed gave up. Terminal until something re-submits it.
	StateFailed State = "failed"
	// StateCancelled was abandoned on purpose. Terminal.
	StateCancelled State = "cancelled"
)

func (s State) Terminal() bool {
	switch s {
	case StateComplete, StateFailed, StateCancelled:
		return true
	}
	return false
}

func (s State) Valid() bool {
	switch s {
	case StatePending, StateRunning, StateTransferred, StateComplete, StateFailed, StateCancelled:
		return true
	}
	return false
}

// Want is what somebody wants to happen, as opposed to what is happening.
type Want string

const (
	// WantRun is the default, and what an absent Intent means.
	WantRun Want = "run"
	// WantPause stops work and keeps everything. Not terminal: the job becomes
	// unavailable to sweeps until the intent changes back.
	WantPause Want = "pause"
	// WantCancel abandons the work on purpose. Terminal once honoured.
	WantCancel Want = "cancel"
)

func (w Want) Valid() bool {
	switch w {
	case WantRun, WantPause, WantCancel:
		return true
	}
	return false
}

// Intent is the desired state, separate from the observed one.
//
// # Why this exists, which is not "so there can be a pause button"
//
// Every write to a record needs the lease, and whoever wants a change is almost
// never holding it: a person clicks cancel in an application while a service on
// another machine moves the bytes. Before this field that was inexpressible —
// Cancel returned an error saying so — and the only alternative was to steal the
// job, which is the single thing the lease exists to prevent.
//
// So desired and observed are separated, which is what every system that has met
// this problem does: Kubernetes has spec against status with a deletionTimestamp
// anyone may set, Temporal records cancellation-requested apart from the run
// state, systemd distinguishes wanted from active, BITS exposes a state its own
// service polls. Nothing here is specific to downloading.
//
// # The rules
//
//  1. Anyone may write it, lease or no lease. It is the ONE field exempt, and
//     that exemption is the point.
//  2. Only the lease holder may write State. Unchanged.
//  3. An owner MUST check it at least as often as it checkpoints and move
//     toward it. An owner that reads a record and ignores this is not an
//     implementation of this abstraction.
//  4. WantCancel must be honoured by everything. Stopping is universal.
//  5. WantPause must be honoured by implementations that advertise it; one that
//     cannot must FAIL the job with a reason rather than carry on, because a
//     pause that quietly does nothing is worse than a refusal.
//  6. A paused job is NOT an orphan — see Store.Orphans. A sweep that adopted it
//     would resume it a moment after somebody stopped it, which is the same trap
//     TRANSFERRED set when it cost a NAS 313 MB for looking abandoned while it
//     was merely waiting.
//  7. Once State is terminal, this is history.
type Intent struct {
	Want Want `json:"want"`
	// By is who asked. Not decoration: a job sitting against somebody's wish is
	// one of the few things that cannot be worked out from the outside, and
	// "which process asked for this" is the first question anyone has.
	By string    `json:"by,omitempty"`
	At Timestamp `json:"at,omitempty"`
}

// Progress is deliberately thin: two numbers and a timestamp, in units the Kind
// defines (bytes, for a download).
//
// It is best-effort and explicitly NOT monotonic — a job that resumes from a
// checkpoint after a crash can legitimately report a smaller Done than it did
// before. Nothing may make a decision on it. Anything richer belongs in the
// Kind's own Checkpoint.
//
// The survey looked for a standard here and found five systems refusing to
// define one — LRO, Temporal, Kubernetes, systemd and HTTP 202 all decline.
// Progress got standardised only where somebody had to draw a progress bar.
type Progress struct {
	Done      int64     `json:"done"`
	Total     int64     `json:"total,omitempty"` // 0 means unknown
	UpdatedAt Timestamp `json:"updated_at"`
}

// Lease is the right to work on a job, held for a bounded time.
//
// Epoch is the part that matters. It increases by one every time the job is
// claimed, and every write must present the epoch it holds. A process that was
// asleep when its lease expired wakes up believing it still owns the job; its
// writes carry a stale epoch and are refused. Without that, two owners work on
// one job and the result is damage both of them believe they produced correctly.
type Lease struct {
	Owner     string    `json:"owner"`
	Epoch     int64     `json:"epoch"`
	ExpiresAt Timestamp `json:"expires_at"`
}

// Held reports whether the lease is still valid at now.
func (l Lease) Held(now time.Time) bool {
	return l.Owner != "" && now.Before(l.ExpiresAt.Time)
}

// Delegation records that the work has been handed to something outside this
// process entirely — a system service, a daemon on a NAS — which is now doing it.
//
// This is not an accommodation for one tool; it is the architecture. An
// application's worker hands off to a system service when one is present, and
// the system service hands off to the NAS when one is configured. The
// application never learns which did the work.
//
// When Delegation is set, Progress is a CACHE of what the external system last
// reported, not a measurement anyone here made. The external system is the truth.
type Delegation struct {
	// System names the delegate — "bits", "nas". Chooses who can interpret
	// ExternalID.
	System string `json:"system"`
	// ExternalID is that system's own handle, opaque here. For BITS it is the
	// job GUID, which outlives every process involved and survives a reboot.
	ExternalID string `json:"external_id"`
	// Delivered records that the delegate has been told to hand the result
	// over — BITS calls this Complete(), and until it happens the file is not
	// the caller's. Forgetting the step leaks jobs until BITS reaps them 90
	// days later.
	Delivered bool `json:"delivered,omitempty"`
}

// Record is the whole job, and it is the cross-language contract.
//
// Everything a different process — in a different language, after a reboot —
// needs in order to continue this work must be in here, because nothing else
// survives.
type Record struct {
	Schema int    `json:"schema"`
	ID     string `json:"id"`

	// Kind says what this job is and therefore who can read Spec and
	// Checkpoint. "download" is the first one. A reader that does not know a
	// Kind must leave that job alone rather than guess at its contents.
	Kind string `json:"kind"`

	State State `json:"state"`

	// Spec is the immutable description of the work, opaque to this package.
	// Written once at submission and never changed: a job whose definition can
	// move under a worker is not resumable by anyone else.
	Spec json.RawMessage `json:"spec"`

	// Checkpoint is what a SUCCESSOR needs in order to continue, opaque here
	// and written by whoever holds the lease.
	//
	// Temporal's activity heartbeat is the design being copied: a retried
	// activity is handed the dead worker's last heartbeat details, so the
	// successor resumes from a point the predecessor proved rather than from
	// the beginning. For a download this holds the verified prefix.
	Checkpoint json.RawMessage `json:"checkpoint,omitempty"`

	Progress   Progress    `json:"progress"`
	Lease      Lease       `json:"lease"`
	Delegation *Delegation `json:"delegation,omitempty"`

	// Requires names capabilities an implementation must have before it may
	// take this job — "survives_process_exit", for instance. Implementations
	// differ enormously and a facade that presents them as interchangeable
	// lies to the caller on the tier most people actually run.
	Requires []string `json:"requires,omitempty"`

	// Error is the last failure, kept so a human can see why a job stopped
	// without finding the log of a process that no longer exists.
	Error string `json:"error,omitempty"`

	// Intent is what somebody WANTS to happen, as against State, which is what
	// is happening. Absent means run. Anyone may write it; the owner honours it.
	Intent *Intent `json:"intent,omitempty"`

	CreatedAt Timestamp `json:"created_at"`
	UpdatedAt Timestamp `json:"updated_at"`
}

// Wants reports the desired state, which is WantRun unless somebody said
// otherwise. Callers use this rather than testing Intent for nil, so that
// "nobody has asked for anything" and "somebody asked for it to run" are the
// same answer everywhere — including for the version 3 records that have no
// intent at all.
func (r *Record) Wants() Want {
	if r.Intent == nil || r.Intent.Want == "" {
		return WantRun
	}
	return r.Intent.Want
}

// Paused reports whether somebody has asked this job to stop and it has not
// finished. Sweeps use it: a paused job must not be adopted.
func (r *Record) Paused() bool {
	return r.Wants() == WantPause && !r.State.Terminal()
}

var (
	ErrUnknownSchema = errors.New("job: unknown record schema")
	ErrInvalid       = errors.New("job: invalid record")
)

// Decode parses a record and refuses anything it cannot safely continue.
func Decode(b []byte) (*Record, error) {
	var r Record
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		// An unknown field is far more likely to be a newer writer than a typo,
		// and continuing a job whose description we only partly understand is
		// exactly the risk this refuses to take.
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !schemaReadable(r.Schema) {
		return nil, fmt.Errorf("%w: have %d, understand %v", ErrUnknownSchema, r.Schema, SchemaReadable)
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Encode writes the record in the form every implementation agrees on:
// indented, with a trailing newline, so a human can read it and a diff is
// legible.
func (r *Record) Encode() ([]byte, error) {
	// What this implementation writes, it writes as its own version. A record
	// read at version 3 and written back at 3 while carrying an intent would be
	// a lie in both directions: the field is there, and an older reader is being
	// told it is safe to ignore fields it does not know. Upgrading on write is
	// why the readable set is a range and the written version is a single number.
	r.Schema = SchemaVersion
	if err := r.Validate(); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func (r *Record) Validate() error {
	if !schemaReadable(r.Schema) {
		return fmt.Errorf("%w: schema %d", ErrInvalid, r.Schema)
	}
	if r.Intent != nil && !r.Intent.Want.Valid() {
		// An unrecognised want is refused rather than treated as run. Guessing
		// here would mean carrying on with a job somebody asked to stop, using a
		// word this implementation is too old to know.
		return fmt.Errorf("%w: intent %q", ErrInvalid, r.Intent.Want)
	}
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("%w: id is required", ErrInvalid)
	}
	if strings.TrimSpace(r.Kind) == "" {
		return fmt.Errorf("%w: kind is required — an opaque spec nobody can identify is unusable", ErrInvalid)
	}
	if !r.State.Valid() {
		return fmt.Errorf("%w: state %q", ErrInvalid, r.State)
	}
	if len(r.Spec) == 0 || !json.Valid(r.Spec) {
		return fmt.Errorf("%w: spec must be present and valid JSON", ErrInvalid)
	}
	if len(r.Checkpoint) > 0 && !json.Valid(r.Checkpoint) {
		return fmt.Errorf("%w: checkpoint is not valid JSON", ErrInvalid)
	}
	if r.Progress.Done < 0 || r.Progress.Total < 0 {
		return fmt.Errorf("%w: progress cannot be negative", ErrInvalid)
	}
	if d := r.Delegation; d != nil {
		if strings.TrimSpace(d.System) == "" || strings.TrimSpace(d.ExternalID) == "" {
			return fmt.Errorf("%w: delegation needs both a system and an external id", ErrInvalid)
		}
	}
	return nil
}

// Delegated reports whether something outside this process is doing the work.
// A caller that finds this true must not start working itself: the external
// system does not participate in the lease, so two workers is exactly the
// damage the lease exists to prevent.
func (r *Record) Delegated() bool { return r.Delegation != nil }

// DecodeSpec unmarshals the opaque spec into v. Callers check Kind first.
func (r *Record) DecodeSpec(v any) error {
	if len(r.Spec) == 0 {
		return fmt.Errorf("%w: no spec", ErrInvalid)
	}
	return json.Unmarshal(r.Spec, v)
}

// DecodeCheckpoint unmarshals the opaque checkpoint into v. A job that has never
// checkpointed leaves v untouched and returns nil, because "no checkpoint" is
// the normal state of a job that has not started, not an error.
func (r *Record) DecodeCheckpoint(v any) error {
	if len(r.Checkpoint) == 0 {
		return nil
	}
	return json.Unmarshal(r.Checkpoint, v)
}

// SetSpec stores v as the opaque spec.
func (r *Record) SetSpec(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.Spec = b
	return nil
}

// SetCheckpoint stores v as the opaque checkpoint.
func (r *Record) SetCheckpoint(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	r.Checkpoint = b
	return nil
}
