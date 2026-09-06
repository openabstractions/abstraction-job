// The job abstraction, stated once, in a language that is nobody's language.
//
// STATUS: a design sketch. NOT normative, and nothing is generated from it. The
// Go, Python and C++ implementations are hand-written, none of them reads this
// file, and where the two disagree the implementations are what ships. It is a
// place to state the shape of the abstraction in a notation that cannot express
// a file path or a JSON key — not a specification to check code against.
//
// WHERE IT DIFFERS FROM THE IMPLEMENTATIONS, as of this commit:
//
//   - `Store.update` here takes a Patch. All three implementations take a
//     mutate callback instead, and no Patch type exists in any of them.
//   - `Store.capabilities()` is implemented nowhere.
//   - CAP_PAUSE and CAP_SCRATCH are declared here and used nowhere. Go answers
//     the scratch question with a `job.Scratch` interface assertion instead.
//   - Field ids and the enum representation are notional. States and wants are
//     written to the record as lower-case strings ("pending", "run"), not
//     integers.
//   - The data structures below have been brought into line with what the three
//     implementations read and write. The service section has not.
//
// WHY THRIFT. Not because we intend to run its runtime, but because its
// architecture is the rule this project arrived at independently: an interface,
// a protocol (encoding) and a transport are three orthogonal things, swapped
// separately. Writing the contract in a notation built on that separation makes
// it hard to smuggle a binding back in. Protobuf, Smithy, Cap'n Proto and WIT
// would all serve. If handles ever need to cross as real references rather than
// as ids, Cap'n Proto and WIT are the two that model that natively, and this
// file should be revisited rather than defended.
//
// WHAT THIS FILE CANNOT SAY. No IDL on that list can express "an epoch only
// increases", "a claim is exclusive", or "a successor resumes only from a proven
// prefix" — and those are the actual contract. An IDL buys the interface and the
// encoding. The SEMANTICS need a conformance suite every binding passes, and the
// Go/Python kill-and-resume test is the seed of that suite, not a curiosity.

namespace go   abstraction.job
namespace py   abstraction.job
namespace cpp  abstraction.job

// The data models a record can carry. A record names the ones it contains in
// `content`, and the subset a reader must understand in `critical`.
//
// A name is namespaced and versioned, so an incompatible change to one model is
// a new name that old readers fail to recognise while every other model in the
// record stays readable. This replaced an integer version field, which could
// only say that something changed, never which part a reader was missing.
const string MODEL_BASE       = "abstraction.job/base@1"
const string MODEL_INTENT     = "abstraction.job/intent@1"
const string MODEL_DELEGATION = "abstraction.job/delegation@1"
const string MODEL_STEP       = "abstraction.job/step@1"

// That a finished record is closed to its own lease holder: no update, no
// release, no intent. It names a RULE rather than a field, and it is the only
// one here that does. The rule shipped under an unchanged MODEL_BASE, so a
// reader published before it walks a `complete` record back to `pending` while
// reading everything about it correctly. Declared whenever `state` is terminal,
// and critical there, because the reader that does not know the rule is exactly
// the reader that breaks it.
const string MODEL_TERMINAL   = "abstraction.job/terminal@1"

// The checkpoint carries a set of proven byte ranges as well as a prefix:
//
//     {"verified_prefix": 4194304, "verified": [[0, 4194304], [8388608, 12582912]]}
//
// Half-open, non-overlapping, sorted, and merged on write — touching ranges
// merged too, so one state has one spelling. `verified_prefix` is derived from
// the set: the end of the range starting at zero, or 0 when there is none.
//
// NEVER critical. A reader that has never heard of `verified` resumes from
// `verified_prefix` and re-fetches the rest, which is what it does today; that
// is what makes the model an addition rather than a break, and marking it
// critical would stop every existing reader for no safety gain.
//
// Named for the download kind because that is who reads a checkpoint, and
// declared here because the declaration lives in the record's `content`.
const string MODEL_RANGES     = "abstraction.download/ranges@1"

// Records written before `content` existed carried an integer `schema` instead.
// All three implementations still READ 3, 4 and 5, mapping each to the exact set
// of models that version could contain; none of them writes the integer any
// more, and a record carrying an unrecognised one is refused.
//   3 -> base
//   4 -> base, intent
//   5 -> base, intent, step

// What somebody wants to happen, as opposed to what is happening.
//
// # The problem this solves, which is not "add a pause button"
//
// Every write to a record requires the lease, and the party who wants a change
// is almost never the party holding it. A person clicks cancel in an
// application while a service on another machine moves the bytes; that person
// has no lease, cannot get one without stealing the job, and stealing it is the
// one thing the lease exists to prevent. Before this field, cancelling a job
// somebody else was working on was not expressible at all — the handle returned
// an error saying so.
//
// So DESIRED and OBSERVED are separated. Every system that has met this problem
// separates them: Kubernetes has spec against status and a deletionTimestamp
// anyone may set, Temporal records cancellation-requested apart from the run
// state, systemd distinguishes wanted from active, and BITS exposes a state its
// own service polls. This is that idea, and nothing about it is specific to
// downloading or to any one application.
//
// # The invariants, which ARE the contract
//
//   1. Anyone may write intent, holding a lease or not. It is the single field
//      exempt from the lease, and that exemption is the whole point.
//   2. Only the lease holder may write state. Unchanged.
//   3. An owner MUST observe intent at least as often as it checkpoints, and
//      move toward it. An owner that reads a record and ignores this field is
//      not an implementation of this abstraction.
//   4. CANCEL must be honoured by every implementation. It is the base
//      guarantee, because stopping is something anything can do.
//   5. PAUSE must be honoured by implementations advertising CAP_PAUSE. One that
//      cannot MUST fail the job with a stated reason rather than carry on
//      silently — a pause that does nothing is worse than a refusal.
//   6. A job whose intent is PAUSE is NOT an orphan. A sweep that adopted it
//      would resume it a moment after somebody stopped it. This is the same
//      trap as TRANSFERRED, which cost a NAS 313 MB by looking abandoned when
//      it was merely waiting.
//   7. Once state is terminal, intent is history. Nothing reopens a finished job.
//
// # Why PAUSE turns out to be nearly universal
//
// It looks like a capability only a sophisticated transfer engine could have —
// BITS suspends a queue, and Lemonade's engine parks a step. But for anything
// holding a checkpoint, pause is: stop working, release the lease, and do not
// adopt it again until the intent changes. Resume is: set intent back to RUN and
// let it become claimable. Nothing is lost, because the proven prefix was
// already durable. The mechanism that makes a crash survivable is the same one
// that makes pausing free, which is why this belongs in the job layer rather
// than in each engine that happens to have thought of it.
enum Want {
  RUN = 1,      // the default, and what an absent intent means
  PAUSE = 2,    // stop, keep everything, do not adopt until this changes
  CANCEL = 3,   // abandon on purpose; terminal once honoured
}

struct Intent {
  1: Want want,
  // Who asked. Not decoration: a job stuck against somebody's wish is one of
  // the few things a person cannot work out from the outside, and "which
  // process asked for this" is the first question.
  2: optional string by,
  3: optional Timestamp at,
}

// An instant, as ISO-8601 in UTC with exactly six fractional digits and a
// trailing Z.
//
// Six, not nine, because Python datetime holds microseconds and cannot represent
// nanoseconds: the contract is set by the least precise participant, not the
// most. Pinning the width is not pedantry. Go trims trailing zeros by default,
// so two conformant implementations wrote different bytes for the same instant,
// and a job history stopped being diffable. Nothing failed; it just rotted.
typedef string Timestamp

enum State {
  // Submitted; nobody is working on it.
  PENDING = 1,
  // A live lease, or a live delegation.
  RUNNING = 2,
  // Finished and proven, but the result has not been taken delivery of.
  // Two-phase on purpose: BITS will not hand a file over until Complete is
  // called, and collapsing this into COMPLETE makes "the service finished it
  // while the app was closed" inexpressible — the case this project exists for.
  TRANSFERRED = 3,
  COMPLETE = 4,
  FAILED = 5,
  CANCELLED = 6,
}

// A phase of a multi-phase job, for display only.
//
// It lives in the record rather than in the kind's checkpoint because progress
// is the one thing a GENERIC reader has to be able to show without knowing what
// the work is. ADVISORY: nothing may decide anything on a step — not what to do
// next, not whether work is finished, not whether to retry. `name` is opaque
// here, like kind and spec.
struct Step {
  1: string name,
  2: i32 ordinal,                 // counts from one
  3: optional i32 of,             // absent when the writer cannot say how many
  // This phase's own units, which need not be the job's: hashing counts the
  // same bytes a second time, and the job's totals must not double for it.
  4: optional i64 done,
  5: optional i64 total,
}

// Best effort, in units the kind defines, and explicitly NOT monotonic: a job
// resuming from a checkpoint may legitimately report a smaller done than it did
// before. Nothing may make a decision on it.
struct Progress {
  1: i64 done,
  2: optional i64 total,          // absent means unknown
  3: Timestamp updated_at,
  // Which phase is happening now, for work that has more than one. Absent means
  // one unnamed phase, which is what every record before the step model meant.
  4: optional Step step,
}

// The right to work on a job, held for a bounded time.
//
// epoch is the part that matters: it increases by one on every claim, and every
// write must present the epoch it holds. A process that was asleep when its
// lease lapsed therefore has its writes refused, rather than accepted on top of
// bytes a different owner has since written.
struct Lease {
  1: string owner,
  2: i64 epoch,
  3: Timestamp expires_at,
  // The issuer has asked for the lease back. Present until the next claim.
  4: optional Recall recall,
}

// The issuer's demand about the resource, as against intent, which is the
// user's wish about the job. Addressed to one holding: a claim replaces it with
// nothing. The lease lapses at `until` whether or not the holder yielded — that
// lapse is the eviction, and renew never extends past it.
struct Recall {
  1: string reason,               // opaque here; the holder's kind acts on it
  2: optional string by,
  3: Timestamp at,
  4: Timestamp until,
}

// The work has been handed to something outside this process entirely — a system
// service, a daemon on another machine — which is now doing it. When this is
// set, progress is a CACHE of what that system last reported. It is the truth;
// we are not.
struct Delegation {
  1: string system,               // who: decides who can read external_id
  2: string external_id,          // their handle, opaque here
  3: optional bool delivered,     // told to hand the result over (BITS Complete)
}

// spec and checkpoint are OPAQUE — binary, not a typed union — and that is the
// most load-bearing decision in this file. The job layer must never parse them,
// so a download can grow mirrors and chunk manifests without forcing a schema
// change on three languages. kind says who is allowed to read them; a reader
// that does not know a kind leaves that job alone rather than guessing.
struct Record {
  // The models this record carries, and the subset a reader must understand or
  // refuse the record entirely. See MODEL_BASE above. Not knowing the step model
  // is harmless; not knowing the intent model means carrying on with a job
  // somebody asked to stop.
  1: list<string> content,
  2: optional list<string> critical,
  3: string id,
  4: string kind,
  5: State state,
  6: binary spec,                      // immutable, written once at submission
  7: optional binary checkpoint,       // what a SUCCESSOR needs to continue
  8: Progress progress,
  9: Lease lease,
  10: optional Delegation delegation,
  11: optional list<string> requires,  // capabilities an impl needs to qualify
  12: optional string error,
  // Absent means RUN. Written by anyone, honoured by the owner. See Intent.
  13: optional Intent intent,
  // Data this layer does not understand, keyed by a name that says who does. A
  // reader that cannot read one MUST preserve it on write; dropping it destroys
  // another participant's data invisibly. Nothing generic may branch on a value,
  // and nothing a stranger MUST obey may live here — that is what Intent is for.
  14: optional map<string, binary> extensions,
  15: Timestamp created_at,
  16: Timestamp updated_at,
}

// The changes a lease holder may make.
//
// NOT IMPLEMENTED. All three bindings take a mutate callback, as noted at the
// top of this file; this is the shape a service binding would need instead.
//
// This struct exists because writing the IDL caught something. In Go, update
// takes a closure: Update(id, epoch, func(*Record) error). A closure cannot
// cross a process boundary, so that signature is a convenience of the IN-PROCESS
// binding and cannot be the contract. Reducing it to a patch is what makes a
// service binding possible at all — and it is the better contract regardless,
// because it enumerates exactly what holding a lease entitles you to change.
struct Patch {
  1: optional State state,
  2: optional binary checkpoint,
  3: optional Progress progress,
  4: optional Delegation delegation,
  5: optional string error,
}

exception NotFound     { 1: string id }
exception LeaseHeld    { 1: string id, 2: string owner }
exception StaleEpoch   { 1: string id, 2: i64 held, 3: i64 actual }
exception LeaseExpired { 1: string id }
exception Terminal     { 1: string id, 2: State state }
exception Invalid      { 1: string reason }

// Capabilities a binding may advertise beyond the base. An application asks; it
// does not assume. A pause button that silently does nothing is worse than no
// pause button, and that is the entire reason these are not in the base service.
const string CAP_PAUSE   = "pause"    // BITS Suspend/Resume; the Lemonade engine
const string CAP_SCRATCH = "scratch"  // this binding is a local filesystem

service Store {
  // NOT IMPLEMENTED in Go, Python or C++. See the divergence list at the top.
  set<string> capabilities(),

  // Records new work and returns its id. The id IS the handle: a plain string
  // that outlives the process which created it, that can be written to a file
  // or handed to another program.
  string submit(1: Record record) throws (1: Invalid e),

  // Any process may read at any time, including one that holds no lease and
  // never will. That is what makes work observable from outside — which a
  // callback cannot be, because a callback dies with the process that registered
  // it, and that is precisely the lifetime which fails.
  Record load(1: string id) throws (1: NotFound e),

  // Oldest first.
  list<Record> list(),

  // Work nobody is doing. The primary reclamation path, not a fallback: a
  // process that is killed never hands anything over. A TRANSFERRED job is NOT
  // an orphan — it is waiting for an acknowledgement, and no amount of redoing
  // the work produces one. Getting that wrong cost a NAS 313 MB, then again
  // thirty seconds later, forever.
  list<Record> orphans(),

  // Takes ownership for ttl_ms and returns the record carrying the new epoch.
  // Exclusive: two callers cannot hold the same epoch for one job.
  Record claim(1: string id, 2: string owner, 3: i64 ttl_ms)
    throws (1: NotFound e1, 2: LeaseHeld e2, 3: Terminal e3),

  // Extends a lease the caller still holds. MUST refuse once the lease has
  // expired, even when the epoch still matches: a process suspended for an hour
  // wakes believing it is still the owner, and forcing it to re-claim bumps the
  // epoch so anything it had in flight is refused.
  Record renew(1: string id, 2: i64 epoch, 3: i64 ttl_ms)
    throws (1: NotFound e1, 2: StaleEpoch e2, 3: LeaseExpired e3),

  // Gives up a lease early. A courtesy: everything works without it, slower.
  void release(1: string id, 2: i64 epoch)
    throws (1: NotFound e1, 2: StaleEpoch e2),

  // The single gate every change passes through, so staleness is checked in one
  // place rather than once per call site.
  Record update(1: string id, 2: i64 epoch, 3: Patch patch)
    throws (1: NotFound e1, 2: StaleEpoch e2, 3: LeaseExpired e3, 4: Invalid e4),

  // Say what should happen, without holding the lease. The ONLY write that does
  // not present an epoch, and deliberately so: the person who wants a job
  // stopped is not the process doing it, and requiring a lease here would mean
  // stealing a job in order to stop it.
  //
  // Idempotent. Refused once the job is terminal — nothing reopens finished
  // work. Setting an intent an owner cannot honour is not an error HERE; the
  // owner reports that, because only the owner knows what it can do.
  Record set_intent(1: string id, 2: Want want, 3: optional string by)
    throws (1: NotFound e1, 2: Terminal e2),

  // Ask the holder for the lease back by now+grace, for a reason it can act on.
  // epoch is the one the caller OBSERVED, not one it holds: the fencing token
  // pointed the other way, so a recall decided against one holding cannot land
  // on the next. Refused where nobody holds the lease, and on a finished job.
  Record recall(1: string id, 2: i64 epoch, 3: string reason, 4: optional string by,
                5: i64 grace_ms)
    throws (1: NotFound e1, 2: StaleEpoch e2, 3: LeaseExpired e3, 4: Terminal e4,
            5: Invalid e5),
}

// Pause and resume are NOT a separate service, and that is the design decision
// worth recording.
//
// The obvious shape was a PausableStore extending Store with pause() and
// resume() methods — which is what an engine that already has a pause button
// looks like from the outside, and it would have been a port of somebody else's
// feature rather than an abstraction. It is wrong here for two reasons.
//
// First, it puts the operation on the STORE, when the thing being paused is a
// job. Second and worse, it makes pausing a different kind of act from
// cancelling, when they are the same act: telling an owner what you want, when
// you are not the owner and cannot become one.
//
// So both go through set_intent, and what CAP_PAUSE gates is not the ability to
// ASK — anyone may ask anything — but the guarantee that an owner will honour
// PAUSE rather than fail the job saying it cannot. Cancel needs no capability
// because stopping is something anything can do.
//
// See the Intent enum for why pause is nearly universal once a checkpoint
// exists, and therefore why very few implementations should have to decline it.

// Deliberately absent from every service above:
//
//   - claimable(record). A pure predicate over a record and a clock. Making it a
//     call would put a round trip in front of an answer the caller already has.
//   - root() and work_path(). A local filesystem area is a property of ONE
//     binding. It is what leaked into nine public signatures before anyone
//     noticed, and it belongs to a storage abstraction that does not exist yet.
//   - anything naming a file, a path, a directory, an encoding or a port.
