#pragma once

// job — a handle to work that is happening somewhere else.
//
// This is the C++ side of an abstraction that already exists in Go and in
// Python. The three are not ports of one another and are not generated from a
// shared IDL. They agree about one thing: the record on disk, and the rules for
// taking ownership of it. The API here looks like C++; the bytes are the
// contract.
//
// WHAT THIS DOES NOT KNOW: what the work IS. `spec` and `checkpoint` are opaque
// and `kind` says who may read them. A download's artifact, sources and sink
// live in a download's spec, not here — which is why downloading can grow
// mirrors and chunk manifests without the record every language must agree
// about changing underneath them.

#include <abstraction/json/value.h>
#include <chrono>
#include <cstdint>
#include <optional>
#include <stdexcept>
#include <string>
#include <vector>

namespace abstraction {
namespace job {

// Key order is part of the on-disk contract — Go decodes with
// DisallowUnknownFields and the file is read by humans in diffs — so this keeps
// insertion order rather than sorting, which would silently alphabetise every
// object it touched, spec included.
using Json = json::Value;

using Clock = std::chrono::system_clock;
using TimePoint = Clock::time_point;

// The data features this implementation understands, and writes.
//
// A name is namespaced and versioned: "abstraction.job/base@1". The version is
// part of the name rather than a separate field, so an incompatible change to
// one feature is a NEW name that old readers correctly fail to recognise, while
// every other feature in the record stays readable.
//
// This replaced an integer version. A version conflates WHAT CHANGED with WHAT
// YOU MUST UNDERSTAND: "schema 5" cannot say which part a reader is missing, so
// refusing the whole record was the only safe answer even when the addition was
// decoration. See kCritical below for the half that makes this more than a
// rename.
namespace feature {
// The record every reader must understand. Always present, always critical.
constexpr const char* kBase = "abstraction.job/base@1";
// What somebody WANTS to happen. Critical whenever present: a reader that
// ignores it works on a job somebody asked to stop.
constexpr const char* kIntent = "abstraction.job/intent@1";
// The work is being done by an external system. Critical: a reader that misses
// it would adopt a job BITS is already running and fetch the bytes twice.
constexpr const char* kDelegation = "abstraction.job/delegation@1";
// Which phase of multi-phase work is happening now. NEVER critical: advisory,
// for telling a person what is going on.
constexpr const char* kStep = "abstraction.job/step@1";
// That a finished record is closed to its own lease holder: no update, no
// release, no intent. It names a RULE rather than a field, which is why it went
// out under an unchanged kBase and why a published reader walks a complete job
// back to pending on a record this tree wrote. Declared whenever the state is
// terminal, and critical there, because a reader that does not know the rule is
// exactly the reader that breaks it.
constexpr const char* kTerminal = "abstraction.job/terminal@1";
// That the issuer has asked the holder for the lease back: lease.recall and
// its rules — a renew never extends past `until`, the holder cannot re-claim to
// shed it, and the lease lapsing at `until` is the eviction. Critical whenever
// present.
constexpr const char* kRecall = "abstraction.job/recall@1";
// A checkpoint carrying proven byte ranges rather than only a prefix. NEVER
// critical: a reader that ignores it resumes from the prefix and re-fetches the
// rest, which costs a second fetch, while marking it critical would stop every
// existing reader dead for no safety gain. Defined in ranges.h.
constexpr const char* kRanges = "abstraction.download/ranges@1";
// That the record says which schema its opaque halves follow and what may be
// asked of a job of this kind. Critical whenever present: a reader that ignored
// it would carry on and then write the record back without it, and the thing
// destroyed would be the description of what was destroyed.
constexpr const char* kEnvelope = "abstraction.job/envelope@1";
}  // namespace feature

// Features this layer declares but cannot derive, because what they describe
// lives inside a field that is opaque here. See derive_features.
bool is_carried_feature(const std::string& name);

// Features that must not appear in `critical` whoever asked for it: both are
// advisory by their own definition, so marking one critical tells a stranger to
// refuse work over a decoration.
bool is_never_critical_feature(const std::string& name);

// Whether this implementation can read a feature named in a record's `critical`.
bool is_known_feature(const std::string& name);

// Every content-set name this implementation can read, sorted. Exposed so a
// harness can diff one implementation's roster against another's: three readers
// that do not know the same names are not three implementations of one
// contract, and nothing could see that until this was askable.
std::vector<std::string> known_features();

// The features a record carries, derived from what is actually in it.
void derive_features(const struct Record& r, std::vector<std::string>& out_content,
                   std::vector<std::string>& out_critical);

// The first critical feature this implementation cannot read, or "" if it can
// read them all. Only `critical` is consulted: a name in `content` that nobody
// here knows is data to carry, not a reason to stop.
std::string first_unknown_critical(const std::vector<std::string>& critical);

// The integer this format used to carry, mapped onto the features each version
// implied. No longer written; still read, because stores full of version 3 and
// 4 records exist on real disks and on a NAS. The mapping is exact rather than
// a guess: those versions are frozen and it is known what each could contain.
std::vector<std::string> features_for_legacy_schema(int schema, bool* ok);

// What somebody WANTS to happen, as against what is happening.
namespace want {
constexpr const char* kRun = "run";      // the default, and what absence means
constexpr const char* kPause = "pause";  // stop, keep everything, stay out of sweeps
constexpr const char* kCancel = "cancel";  // abandon on purpose
}  // namespace want

bool is_valid_want(const std::string& w);

namespace state {
constexpr const char* kPending = "pending";
constexpr const char* kRunning = "running";
// Finished and proven, but the result has not been taken delivery of. Not
// bureaucracy: it is the only way to express "the service finished this while
// the application was closed".
constexpr const char* kTransferred = "transferred";
constexpr const char* kComplete = "complete";
constexpr const char* kFailed = "failed";
constexpr const char* kCancelled = "cancelled";
}  // namespace state

bool is_valid_state(const std::string& s);
bool is_terminal_state(const std::string& s);

class JobError : public std::runtime_error {
public:
    explicit JobError(const std::string& what) : std::runtime_error(what) {}
    virtual const char* name() const noexcept { return "JobError"; }
};

class NotFound : public JobError {
public:
    explicit NotFound(const std::string& what) : JobError(what) {}
    const char* name() const noexcept override { return "NotFound"; }
};

class Invalid : public JobError {
public:
    explicit Invalid(const std::string& what) : JobError(what) {}
    const char* name() const noexcept override { return "Invalid"; }
};

class UnknownSchema : public Invalid {
public:
    explicit UnknownSchema(const std::string& what) : Invalid(what) {}
    const char* name() const noexcept override { return "UnknownSchema"; }
};

// An action a kind does not declare, or one this supervisor has not built. A
// different question from UnknownSchema and the two are not interchangeable:
// UnknownSchema means leave this job alone, and this means the job is mine and
// this is not one of the things it does.
class NotSupported : public JobError {
public:
    explicit NotSupported(const std::string& what) : JobError(what) {}
    const char* name() const noexcept override { return "NotSupported"; }
};

class LeaseHeld : public JobError {
public:
    explicit LeaseHeld(const std::string& what) : JobError(what) {}
    const char* name() const noexcept override { return "LeaseHeld"; }
};

class StaleEpoch : public JobError {
public:
    explicit StaleEpoch(const std::string& what) : JobError(what) {}
    const char* name() const noexcept override { return "StaleEpoch"; }
};

class LeaseExpired : public JobError {
public:
    explicit LeaseExpired(const std::string& what) : JobError(what) {}
    const char* name() const noexcept override { return "LeaseExpired"; }
};

class TerminalState : public JobError {
public:
    explicit TerminalState(const std::string& what) : JobError(what) {}
    const char* name() const noexcept override { return "TerminalState"; }
};

// Always UTC, always exactly six fractional digits, always a trailing Z.
//
// Six and not nine because Python's datetime cannot hold nanoseconds, and the
// contract is set by the least precise participant. Trailing zeros are NOT
// trimmed: Go trimmed them for a while and wrote ".22275Z" where Python wrote
// ".222750Z" for the same instant, so a job's history diffed as changed every
// time the two took turns on it.
//
// Throws Invalid for an instant no record can spell, rather than writing a year
// outside four digits that no peer could read back.
std::string format_rfc3339(TimePoint t);

// Permissive on the way in, because another implementation may be less careful
// than this one and refusing to read a job over a timezone suffix would be
// absurd. Accepts any fractional precision and either Z or a numeric offset.
//
// Throws Invalid for an instant this build's clock cannot hold. Where the tick
// is a nanosecond that is everything before 1677 or after 2262, which includes
// Go's zero time of year one: the span the clock reaches is narrower than the
// span the wire permits, and it differs between toolchains, so the reader says
// so rather than letting an int64 wrap and calling year one 1754.
TimePoint parse_rfc3339(const std::string& s);

// Deliberately thin: two numbers and a timestamp, in units the kind defines.
// Best-effort and explicitly NOT monotonic — a job resuming from a checkpoint
// after a crash can legitimately report a smaller `done` than before. Nothing
// may decide anything on it. Anything richer belongs in the kind's checkpoint.
// Which phase of a multi-phase job is happening now.
//
// In the record and not the kind's checkpoint, because a checkpoint is opaque:
// only a reader that knows the kind could render it. Progress is the one thing a
// GENERIC reader must be able to display -- a supervisor's status output, a
// download manager listing work of every kind -- without knowing what the work
// is.
//
// What made it necessary: a download delegated to a NAS is two transfers, and
// only the first was ever visible. The far side fetched 40 GB and reported done;
// then this machine copied those 40 GB back across a share and re-hashed them,
// with the record still showing the first transfer's numbers throughout.
//
// ADVISORY ONLY, the same rule progress already has. Nothing may decide anything
// on a step. The moment something branches on ordinal == 2, a workflow engine
// has been smuggled into a record whose value is that it describes work without
// prescribing it. `name` is opaque here, exactly like kind and spec.
struct Step {
    std::string name;
    int ordinal = 0;  // counts from one
    int of = 0;       // 0 when the writer cannot say how many
    // This phase's own units, which need not be the job's: hashing counts the
    // same bytes a second time, and the overall numbers must not double for it.
    std::int64_t done = 0;
    std::int64_t total = 0;
};

struct Progress {
    std::int64_t done = 0;
    std::int64_t total = 0;  // 0 means unknown
    TimePoint updated_at{};
    // Absent means what every record before this meant: one unnamed phase.
    std::optional<Step> step;
};

// The right to work on a job, for a bounded time.
//
// `epoch` is the part that matters: it rises by one on every claim, and every
// write must present the epoch it holds. A process that slept through its own
// expiry wakes believing it is still the owner; its writes carry a stale epoch
// and are refused. Without that, two owners work one job and each believes the
// result is correct.
// The issuer asking for the lease back — the half of a lease this design
// lacked: ours expired, and none could be revoked early with a reason the
// holder could act on.
//
// Not an intent. Intent is what the user wants of the job; a recall is what the
// issuer demands of the resource, addressed to one holding — the epoch it was
// decided against — so a claim replaces it with nothing.
//
// The fallback is the lease lapsing at `until`: the recall moves `expires_at`
// earlier and renew never extends past it. WDDM's shape, not Android's —
// residency ends whether or not the holder trimmed, and the holder finds out on
// its next write. A lease cannot kill a process on another machine; it can stop
// believing in it.
//
// `reason` is opaque here, chosen by the issuer for the holder's kind, as `kind`
// scopes `spec`. Required: a recall without one is a cancel in the wrong field.
struct Recall {
    std::string reason;
    std::string by;
    TimePoint at{};
    TimePoint until{};
};

struct Lease {
    std::string owner;
    std::int64_t epoch = 0;
    TimePoint expires_at{};
    std::optional<Recall> recall;

    bool held(TimePoint now) const { return !owner.empty() && now < expires_at; }
    bool recalled() const { return recall.has_value(); }
};

// The work has been handed to something outside this process — a system
// service, a daemon on a NAS — which is now doing it. When this is set,
// `progress` is a CACHE of what the external system last reported; the external
// system is the truth.
struct Delegation {
    std::string system;
    std::string external_id;
    bool delivered = false;
};

// The desired state, separate from the observed one.
//
// Why this exists, which is not "so there can be a pause button": every write
// to a record needs the lease, and whoever wants a change is almost never
// holding it — a person clicks cancel in this server's UI while a supervisor on
// another machine moves the bytes. Before this field that was inexpressible,
// and the only alternative was to steal the job, which is the single thing the
// lease exists to prevent.
//
// Every system that has met this problem separates desired from observed:
// Kubernetes has spec against status with a deletionTimestamp anyone may set,
// Temporal records cancellation-requested apart from the run state, BITS
// exposes a state its own service polls.
//
// The rules, which are the contract rather than this implementation's habits:
//
//   1. Anyone may write it, lease or no lease. It is the ONE field exempt.
//   2. Only the lease holder may write `state`. Unchanged.
//   3. An owner MUST check it at least as often as it checkpoints and move
//      toward it. An owner that reads a record and ignores this is not an
//      implementation of this abstraction.
//   4. Cancel must be honoured by everything; stopping is universal.
//   5. Pause must be honoured by implementations that advertise it. One that
//      cannot must FAIL the job with a reason rather than carry on, because a
//      pause that quietly does nothing is worse than a refusal.
//   6. A paused job is NOT an orphan unless it is still RUNNING — see
//      Record::stranded.
//   7. Once `state` is terminal this is history.
struct Intent {
    std::string want = want::kRun;
    // Who asked. Not decoration: a job sitting against somebody's wish is one
    // of the few things that cannot be worked out from outside, and "which
    // process asked for this" is the first question anyone has.
    std::string by;
    std::optional<TimePoint> at;
};

// --- the base envelope ------------------------------------------------------
//
// NOTHING IN THIS LIBRARY RESOLVES A SCHEMA IDENTIFIER. There is no function
// here from a schema name to a schema; the only questions askable of one are
// whether it is well formed and whether it is a string the caller already
// knows. That is the whole safety argument, and valid_schema is the half that
// makes it structural rather than a promise: a legal identifier cannot be a
// URL, a UNC name or a path, so a supervisor that wanted to fetch one would
// first have to be handed something valid_schema refuses. A supervisor that
// fetches an address out of somebody else's record is running attacker-chosen
// content as a service on the owner's machine.

// The schema a bare action name belongs to. The bare namespace is this layer's;
// anyone else's name is qualified, or two vendors both define "cancel" and a
// supervisor cannot tell which one it just performed.
constexpr const char* kBaseSchema = feature::kBase;

// What may be asked of ANY job through this layer's own operations, and which a
// kind declares it actually honours. Capability says what an IMPLEMENTATION
// promises; this says what a KIND honours, and they are different questions — a
// store that survives process exit still cannot pause a kind whose worker has
// no pause. All four are asks, never instructions: each names an existing store
// operation, and none of them can describe how to carry anything out.
namespace action {
constexpr const char* kPause = "pause";
constexpr const char* kResume = "resume";
constexpr const char* kCancel = "cancel";
constexpr const char* kRecall = "recall";
}  // namespace action

bool is_base_action(const std::string& bare);

// Is `s` a schema identifier [JOB-V1]?
//
//     namespace "/" name "@" version
//
// spelled exactly as this record's content names already are, so there is one
// grammar for names here and not two. There is no scheme, no authority, no
// percent-escape, no backslash, no query and exactly one "/" and one "@", which
// is what makes "https://example.invalid/x@1", "\\host\share", "../../etc" and
// "file:///x" illegal values rather than discouraged ones.
bool valid_schema(const std::string& s);

// An action name split into the schema that declares it and the bare name
// [JOB-V2]. A bare name leaves `out_schema` empty, which means the base schema.
//
// The separator is "#" because it is the one component of a URI reference
// defined never to be sent anywhere (RFC 3986 §3.5): a fragment names something
// inside a document rather than a document to go and get. It is also excluded
// from both grammars either side of it, so the split is exact.
bool split_action(const std::string& name, std::string* out_schema, std::string* out_bare);

// What a job of this kind is, and what may be asked of it.
//
// Two fields and no third. An envelope is a property of the KIND, so nothing in
// it may report what is true of this instance now: a supervisor that read
// "pause" here and concluded this job can be paused at this moment would be
// reading live state off a static declaration, and the job's worker may have
// died an hour ago. The stores refuse an update that moves it [JOB-V4].
struct Envelope {
    // A NAME, never an address — see the note above.
    std::string schema;
    // Names a supervisor matches against what it already implements. Matching is
    // the only operation defined on them.
    std::vector<std::string> actions;

    void validate() const;
    bool operator==(const Envelope& other) const {
        return schema == other.schema && actions == other.actions;
    }
    bool operator!=(const Envelope& other) const { return !(*this == other); }
};

// What a caller has actually built, in its own process: the schemas it was
// written against and the actions it implements.
//
// A value the caller constructs and never something read out of a record. That
// is the shape the whole design turns on — an unknown schema is answered from a
// list the caller already holds, so the answer to "I have never heard of this"
// is a refusal and there is nowhere for it to be a fetch.
struct Supervisor {
    std::vector<std::string> schemas;
    std::vector<std::string> actions;
};

// [JOB-V4]: the envelope is a property of the KIND, so a lease holder cannot
// move it. Throws Invalid when it moved.
void envelope_unmoved(const std::optional<Envelope>& before,
                      const std::optional<Envelope>& after);

// The whole job, and the cross-language contract. Everything a different
// process — in a different language, after a reboot — needs in order to
// continue this work has to be in here, because nothing else survives.
struct Record {
    // What this record carries, and the subset a reader MUST understand or
    // refuse it entirely. Not knowing the step feature is harmless; not knowing
    // the intent feature means working on a job somebody asked to stop. X.509
    // settled the fallback (RFC 5280 §4.2); JOSE's "crit" (RFC 7515 §4.1.11)
    // settled the shape, a parallel list naming things carried elsewhere in the
    // same document, so critical is always a subset of content.
    std::vector<std::string> content;
    std::vector<std::string> critical;
    // Data this layer does not understand, keyed by a name that says who does.
    // A reader that cannot read one MUST preserve it on write: dropping it
    // destroys another participant's data invisibly, because nobody here can
    // see what was lost.
    Json extensions = Json::object();
    std::string id;
    std::string kind;
    // Which schema `spec` and `checkpoint` follow and what may be asked of a job
    // of this kind. The difference between leaving a stranger's job alone and
    // being able to act on it without ever opening it. See Envelope.
    std::optional<Envelope> envelope;
    std::string state = state::kPending;
    Json spec = Json::object();
    std::optional<Json> checkpoint;
    Progress progress;
    Lease lease;
    std::optional<Delegation> delegation;
    // The JSON key is "requires"; the member cannot be, because `requires` is a
    // keyword from C++20 on and this header has to compile under both standards.
    std::vector<std::string> requires_capabilities;
    std::string error;
    // What somebody WANTS to happen, as against `state`, which is what IS
    // happening. Absent means run. Anyone may write it, lease or no lease; the
    // owner honours it. See Intent.
    std::optional<Intent> intent;
    TimePoint created_at{};
    TimePoint updated_at{};

    void validate() const;
    // Validate against a content declaration that has not been stored yet, so
    // encode() can derive and check in one pass while staying const.
    void validate_with(const std::vector<std::string>& check_content,
                       const std::vector<std::string>& check_critical) const;

    // Fill in content and critical from what this record actually carries.
    // Derived rather than remembered, so the declaration cannot drift from the
    // data. Caller-declared criticals are preserved.
    void describe();
    bool terminal() const { return is_terminal_state(state); }
    bool delegated() const { return delegation.has_value(); }

    // The schema this job's opaque halves follow, or "" when the record does not
    // say. A name to compare, not a place to look.
    std::string schema() const { return envelope ? envelope->schema : std::string(); }

    // The action names the kind declares, in the record's own order.
    std::vector<std::string> actions() const {
        return envelope ? envelope->actions : std::vector<std::string>();
    }

    // Does the KIND declare this action? Not whether it can be performed now:
    // this is a static declaration, and the worker may have died an hour ago. A
    // bare name and a qualified one are different actions and never match each
    // other, which is the point of qualifying at all.
    bool supports(const std::string& action) const;

    // May this supervisor perform `action` on this record? It performs nothing,
    // and every refusal names what was refused, because a supervisor that
    // silently declines is indistinguishable from one that quietly did the
    // wrong thing.
    //
    //   Invalid        the name is not an action name
    //   UnknownSchema  the record follows a schema this supervisor does not know
    //   NotSupported   the kind does not declare it, or this supervisor has not
    //                  implemented it
    void ask(const std::string& action, const Supervisor& supervisor) const;

    // The desired state, which is "run" unless somebody said otherwise.
    //
    // Callers use this rather than testing `intent` for a value, so that
    // "nobody asked for anything" and "somebody asked for it to run" are the
    // same answer everywhere — including for version 3 records, which have no
    // intent at all.
    const std::string& wants() const {
        static const std::string kDefault = want::kRun;
        if (!intent.has_value() || intent->want.empty()) return kDefault;
        return intent->want;
    }

    // Somebody asked this to stop and it has not finished.
    bool paused() const { return wants() == want::kPause && !terminal(); }

    // Does this record's own state and intent leave work for a sweep?
    //
    // A TRANSFERRED job does not, and the difference cost a NAS 313 MB:
    // transferred is deliberately not terminal, so a sweep saw a finished,
    // digest-proven job with a lapsed lease and fetched the whole thing again,
    // forever. What it waits for is an acknowledgement, and no amount of
    // re-downloading produces one.
    //
    // A PAUSED job does not either — it looks abandoned and is not. Unless
    // nobody ever carried the pause out: an owner that honours one releases the
    // lease and the record stops being RUNNING, so a record still RUNNING under
    // nobody is an owner that died between the two. Skipping that one makes the
    // state permanent, because nothing may claim the job and therefore nothing
    // may pause, resume or cancel it either.
    bool stranded() const {
        if (state == state::kTransferred) {
            return false;
        }
        return !paused() || state == state::kRunning;
    }

    // The form every implementation agrees on: 2-space indent, trailing
    // newline, keys in this order.
    std::string encode() const;

    // Refuses anything it cannot safely continue, including a record carrying a
    // key this version does not know — far more likely to be a newer writer
    // than a typo, and continuing a job whose description we only partly
    // understand is exactly the risk not worth taking.
    static Record decode(const std::string& text);
};

}  // namespace job
}  // namespace abstraction
