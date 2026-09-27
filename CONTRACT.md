# abstraction.job contract

Binds: `acceptance.thrift`

`acceptance.thrift` defines `abstraction.job/acceptance@1`, which accepts a
job under the caller's idempotency key and reconciles or cancels it;
`abstraction.job/operations@1`, which observes a job and reads its result;
`abstraction.job/inventory@1`, which lists the calling subject's own jobs;
and `abstraction.job/operator@1`, which lists and cancels every subject's
jobs under a rights decision made per call. `job.thrift` defines the job
record the deprecated job store writes to disk, with no service of its own.
[README.md](README.md) is the door: what this module is, how to obtain it,
one example that runs. [SPEC.md](SPEC.md) is the job store's behavioural
specification, drawn from this page, the implementations and the scenarios.

## Reading this page

The key words "MUST", "MUST NOT", "REQUIRED", "SHALL", "SHALL NOT", "SHOULD",
"SHOULD NOT", "RECOMMENDED", "MAY" and "OPTIONAL" in this page are to be
interpreted as described in RFC 2119 and RFC 8174, when, and only when, they
appear in all capitals, as shown here.

A rule id such as `JOB-A1` is declared once, in bold brackets before its
title, at the head of the rule it names. A conformance scenario cites it on
its `# expect` line, tests and refusals cite it the same way, and a citation
that resolves to no rule on this page or on [SPEC.md](SPEC.md) is a defect in
one of the two. A retired id is never reused; [HISTORY.md](HISTORY.md) keeps
it with the release it left. `HISTORY.md` also carries this contract's prior
art, its rationale appendix, and what was measured and proven, linked from
here and linking back.

The letters are this module's, on this page and on `SPEC.md` alike:

| letter | topic |
| --- | --- |
| A | admission: the acceptance service, idempotency keys, receipts, reconciliation, cancellation, labels and rights |
| K | kind: the opaque specification and who may read it |
| M | immutability: what a lease never moves, and who owns a record handed across |
| C | checkpoint: what a successor inherits, and proven ranges |
| P | progress |
| L | lease: acquiring, the epoch and the lock |
| O | ownership: a lease held against another owner (`SPEC.md`) |
| W | work directory: the store root's names and what is reserved |
| N | notification: what a watch reports as a change |
| H | hold: keeping the machine awake for exactly one lease (`SPEC.md`) |
| R | yield requests: the issuer asking a holder to give the lease back |
| F | fields: unknown fields refused, extensions preserved |
| J | JSON encoding: how a record is written, byte for byte |
| D | declarations: `content`, `critical`, the schema integer and version skew |
| V | envelope: schema identifiers and action names |
| I | intent: what somebody wants, written without a lease |
| S | state: who writes each state |
| T | terminal: what a finished record refuses |
| G | delegation: a job an external system owns |
| U | a lost answer: a store call whose outcome is unknown (`SPEC.md`) |
| B | backoff and retry (`SPEC.md`) |
| E | error and outcome |

`A` (admission), `E` (error and outcome) and `X` (extension) are reserved
with one meaning in every contract (S12). `X` is unused here. The encoding
rules that read `JOB-E1`..`JOB-E9` before this change are
`JOB-J1`..`JOB-J9`; `SPEC.md`'s outcome rules continue the reserved letter
from `JOB-E10`. `SPEC.md`'s own ids that repeated one of this page's with a
different rule, and its two `X` ids, continue under `U`, `S`, `D`, `I` and
`G`; `HISTORY.md` lists each.

| Prose | Wire (until its own release) |
| --- | --- |
| job | `operation` and `work` in `operation_id`, `OperationSnapshot`, `OperationControl`, `WorkState`, `WorkProgress`, `WorkFailure`, `ListAccountWork` and `CancelOperation` |
| Cancel, Observe, List | `CancelWork`, `ObserveWork`, `ListWork` (`acceptance@2`, `research/vocabulary/RENAME-PLAN.md` §4 "first") |
| idempotency key | `RequestIdentity`, its `key`, `history_epoch` and `attempt` fields together (`IdempotencyKey` at `acceptance@2`) |
| subject | "caller scope" in `acceptance.thrift`'s doc strings and Go's `Bind(authenticatedCallerScope)` |
| acquire, acquisition | `Claim` on the deprecated `Store` and the scenario operation `claim`, kept until the job store is removed (D24) |
| yield request | the record field `lease.recall`, `Store.Recall`, the content name `abstraction.job/recall@1`, the transcript field `recall` and the scenario operation `recall`, kept (D49) |

The executable [acceptance corpus](testdata/acceptance.json) and Go
`abstraction/job/acceptance` decision tests check the admission rules'
semantic distinctions. The decision helper consumes trusted owner evidence;
its booleans are no wire claim, persistence engine or substitute for atomic
provider transactions. Generated codecs validate structure, and cross-field
receipt validation remains required at the receiving boundary.

## Admission: the acceptance service

`abstraction.job/acceptance@1` defines submission and reconciliation
independently of the deprecated job store and its tagged record. A job store
implementation acquires none of these guarantees by compiling generated
code. These rules state what a provider implements; a provider's own test
results are the evidence that it ships them.

**[JOB-A1] The idempotency key precedes transmission.** The client MUST
obtain an owner-issued history epoch, create a stable idempotency key, and
retain the key, epoch and logical owner before sending. The service MUST
deduplicate on the receiving boundary's authenticated subject together with
this service, the epoch and the key. The subject MUST be the account and the
calling executable's absolute path, as the operating system reports them, in
every language's client. The service MUST NOT accept a subject the caller
supplies. The subject is stable across connections and caller restarts, and
it changes when the executable is rebuilt or moved: receipts filed under the
earlier path stay under it. Reconciliation and cancellation MUST require
current authorization; possession of a key or receipt confers none. A caller
that loses its key before receiving a receipt reports unknown: this version
has no recovery by guessing a new key, and authorized job discovery is under
"Not built".

**[JOB-A2] Arguments and promises are immutable.** Submission equality MUST
compare kind, exact opaque specification bytes and the set of required
guarantee names. Empty or duplicate guarantee names are invalid. Ordering is
immaterial. Guarantee names identify versioned contracts the provider
understands, and the provider MUST NOT silently ignore an unsupported
required guarantee. Reusing a retained accepted idempotency key with
different arguments MUST return `key_conflict` and start no job. Equal
arguments MUST return the original receipt and job. The receipt binds the
idempotency key, stable logical owner, job id, accepted guarantees and
minimum history retention. Accepted guarantees MUST include every requested
guarantee. A logical owner is never a PID, endpoint, directory or implicit
transfer instruction. When retained arguments are available, reconciliation
MUST validate their key, shape and required guarantees against the receipt
even if the caller supplies only an idempotency key. Contradictory evidence
MUST return `unknown`. A valid authorized receipt alone suffices for lookup
when argument evidence is absent; it cannot independently prove the original
required guarantees were met. A duplicate Submit needs argument evidence to
validate equality and MUST return `unknown` when that evidence is absent.

**[JOB-A3] Acceptance has an atomic recovery boundary.** Before
acknowledging acceptance or initiating effects, the provider MUST atomically
associate the idempotency key, immutable arguments, job and promises with
recoverable owner evidence. The persistence and downstream recovery needed
depend on the promises actually accepted. A local record alone proves
nothing about an external engine submitting once. A provider unable to
reconcile downstream acceptance MUST refuse that guarantee. Reconnection or
installing another provider transfers no job.

**[JOB-A4] A definite negative fences delayed requests.** Reconciliation
MUST return `accepted` with a receipt, `definitely_not_accepted`, or
`unknown`; key conflict, authorization refusal and invalid input are
distinct outcomes. `definitely_not_accepted` means the authoritative owner
proves this key was never accepted **and atomically seals it against
subsequent acceptance**. The provider MUST refuse delayed Submit messages.
This applies also to a refused Submit whose negative outcome permits fresh
resolution. An empty lookup is insufficient even with current history: the
first request may still be in flight. Only this sealed negative allows fresh
provider selection with a new key. A reused sealed key MUST stay
nonaccepted, and it cannot be reopened. Failed transport, unavailable
history, access denial and a lost reply are never definite negatives.

**[JOB-A5] Expiry removes evidence and leaves effects in place.**
GetHistoryWindow MUST advertise a minimum retention duration in milliseconds
from original acceptance; receipts repeat the accepted duration. Duplicate
replies MUST NOT reset its start. The owner decides which history is
available, and client wall clocks decide nothing about expiry. At or after
expiry a missing receipt MUST return `unknown`, including after caller or
owner restart. Closing a history epoch MUST permanently fence new and delayed
submissions in that epoch before individual key tombstones may be discarded.
Epoch closure proves no new acceptance can occur. It proves nothing about
whether an old unknown job was accepted. Neither expiry nor closure cancels
a job or permits automatic resubmission. An implementation MUST retain
evidence for the promised window or report uncertainty honestly when
recovery fails.

**[JOB-A6] Waiting and the job have different lifetimes.** Stopping a wait,
a deadline expiring or caller exit MUST leave the accepted job under its
owner and promises. Cancel is separately authorized and explicit.
`requested` records intent, and terminal completion may win the race against
it. Repeating Cancel for the same key MUST repeat the intent without creating
a new job or reversing a terminal result. `already_terminal`, `unknown`,
`forbidden` and `unsupported` retain distinct meanings. A caller observes the
existing job's terminal result through its capability API, and cancellation
never authorizes creating a replacement job automatically.

**[JOB-A7] A retry is a numbered attempt of the same key.**
`RequestIdentity.attempt` numbers explicit caller retries of one `key`
within one history epoch. The original request is attempt zero; negative
attempts are invalid. Attempt N+1 MUST be eligible only when the owner holds
evidence for attempt N under the same subject, `key` and epoch, and attempt
N is either accepted with its job in state `failed`, or sealed as definitely
not accepted. An absent, live, `complete` or `cancelled` attempt N makes
attempt N+1 ineligible. An ineligible attempt MUST return `invalid` from
Submit and Reconcile; the provider neither accepts nor seals it, and the
caller may present it again after attempt N ends. Each attempt is its own
idempotency key under JOB-A1 to JOB-A6: a duplicate Submit of one attempt
returns its receipt, and reconciliation, observation, results and
cancellation each address one attempt. Arguments of a new attempt MUST equal
those of the latest accepted earlier attempt; different arguments return
`key_conflict` and start no job. Eligibility requires every earlier attempt
to be terminally failed or sealed, and both states are irreversible. One
`key` has at most one live job in an epoch. The provider decides eligibility
from owner evidence at admission. The provider MUST NOT create an attempt
itself, and cancellation, expiry and failure authorize no automatic retry.

**[JOB-A8] A permanent failure is terminal and typed.** An observed failure
classified `permanent` MUST belong to a job in state `failed`, and the
provider MUST NOT try that job again. A `retryable` failure accompanies a job
the provider will try again. `WorkFailure.cause` names the provider's typed
reason when known: `digest_mismatch`, `oversize`, `short_transfer`,
`unauthorized`, `not_found`, `refused`, `server_error`, `transport`,
`credential` or `other`. `credential` means a named credential could not be
applied; the message carries the applier's outcome, and the class is
`retryable` exactly when that outcome is `unavailable`. An absent cause is
unreported. A reader MUST treat an unrecognized cause as `other`. Clients
MUST NOT derive classification or cause from message text.

**[JOB-A9] An unobtainable policy decision is unavailable.** When a provider
requires a service-owned policy decision for Submit, Reconcile or Cancel and
cannot obtain it, the call MUST return `unavailable`. Submit and Reconcile
then carry no receipt, make no admission effect and record no seal. The same
idempotency key, including its attempt, may be presented again. An
`unavailable` result is no JOB-A7 evidence: an attempt refused this way
leaves no journal for a later attempt to rely on. Cancel returning
`unavailable` records no cancellation intent. `forbidden` is an evaluated
refusal. `unknown` asks for reconciliation of a call that may have been
accepted; `unavailable` reports a call that reached no acceptance evidence.
Older generated clients refuse this outcome word, and a provider MUST return
it only for a configured policy decision. The same decision outage MUST make
Observe return `unavailable`, ReadResult return `unavailable` and List
return an `unavailable` page. None of them reads job state, records a seal
or records a lost result. A ReadResult `unavailable` caused by an outage
makes no claim about the result bytes; Observe reports a recorded loss
(JOB-A10).

**[JOB-A10] A lost result ends its job as a typed failure.** When a job is
`complete` and the provider determines that its result bytes no longer
exist, the provider MUST durably record the loss before replying. From then
on ReadResult MUST return `unavailable`, and Observe MUST report state
`failed` with a `permanent` failure whose cause is `result_lost`. That
observation is JOB-A7 evidence: the next attempt of the key is eligible, and
the lost attempt keeps its receipt. A read error that does not establish
absence MUST return `unavailable` without recording a loss, and the job
stays `complete`. A recorded loss is irreversible. The provider MUST NOT
serve bytes that reappear under the lost key. Recording a loss creates no
job and authorizes no automatic retry.

**[JOB-A11] A provider declares its result retention.** `HistoryWindow` MUST
carry `result_retention_ms`: the minimum time after completion that the
provider keeps a complete job's result bytes readable. What those bytes are
is kind-defined: `ReadResult` streams whatever content the executing kind
put at the job's result document (the download kind's own rule is
`abstraction-download/CONTRACT.md` DL-R36). Zero declares no retention. A
provider without result access MUST declare zero. Within a declared
retention, `result_lost` reports storage damage outside the provider's own
policy. After it, the provider MAY remove result bytes, and a read then
reports `result_lost` under JOB-A10. The value is a minimum, and bytes MAY
stay readable longer. It applies per provider, and a caller restoring a
binding reads it again from the history window. A negative value is invalid.

**[JOB-A12] A job carries a display label outside its idempotency key.**
`Submission.label` is optional display text the caller gives the job. The
provider MUST trim leading and trailing white space, and a label that is
empty after trimming is absent. A present label MUST be 1 to 256 bytes of
valid UTF-8 on one line: no code point below U+0020, no U+007F, and no
U+2028 or U+2029. A label outside these limits MUST make Submit return
`invalid`, and the provider journals, accepts and seals nothing for that
call. When the caller supplies no label, the provider of the submission's
kind MAY derive one at acceptance from what it reads safely from the
specification. A derived label MUST obey the same limits and MUST NOT carry
a credential: the `download` kind uses the first source's host and its last
non-empty path segment, joined by ` · ` (U+00B7 between spaces), and drops
the scheme, userinfo, port, query and fragment. A source with no path
segment yields the host alone. An unknown kind, or a specification the
provider cannot read, leaves the label absent.

The provider MUST store the label once, at acceptance, and MUST NOT change
it afterwards. `OperationSnapshot.label` reports it through Observe and
List, stable across observation and restarts, and `label_derived` is true
exactly when the provider derived it. The label is outside JOB-A2 equality
and the JOB-M1 immutable fields. A duplicate Submit whose label differs MUST
return the original receipt, and observation keeps the stored label. Each
attempt (JOB-A7) stores the label its own acceptance received. The label has
the snapshot's own visibility. It MUST NOT be a rights resource, a policy
decision MUST NOT receive it, and the service's own logs and audit name the
job id instead. A caller-supplied label is the caller's own text. The label
is display text alone: never a path, a result file name or a unit grouping
several jobs.

**[JOB-A13] Account-wide inventory and cross-subject cancellation are
decided per call.** A caller's own jobs are scoped by ownership: its subject
observes, lists, reads and cancels what that subject submitted, and no rule
is consulted for it. `abstraction.job/operator@1` serves the two actions
that cross subjects. `ListAccountWork` lists every subject's accepted jobs
under the rule `abstraction.job/inventory.read`. `CancelOperation` records
cancellation intent on any job by its receipt job id under the rule
`abstraction.job/acceptance.cancel`. Both rules name the resource
`abstraction.job/acceptance@1`. The provider MUST ask its configured
decision on every call and MUST NOT cache a permit. With no configured
decision every operator call MUST be `forbidden`. A decision that cannot be
obtained is `unavailable` and reads or changes nothing (JOB-A9). A cursor
from `ListAccountWork` MUST NOT continue a List enumeration, and the
reverse. An operator snapshot MUST carry no subject, program or provider
path. Submit is decided under the rule `abstraction.job/acceptance.submit`
where the runtime configures one.

**[JOB-A14] The job rights actions are closed.** `resource_actions` MUST name
exactly `acceptance.submit`, `acceptance.cancel` and `inventory.read`. They
are the job capability's own, and a runtime registers them into its rights
decision policy. `abstraction.job/result.read`, for reading another
subject's result bytes, is under "Not built".

**[JOB-A15] An unfinished job reports what holds it.** A kind's
specification may carry conditions on when its job may move, and a provider
that honours one MUST record the condition that holds the job under an
extension key of that kind, such as `abstraction.download/waiting@1`.
`OperationSnapshot.waiting` MUST report the word the kind's provider reads
from that key: 1 to 64 bytes of lowercase ASCII letters, digits, `_`, `-`,
`.` and `:`, such as `network:metered`. It MUST be empty when nothing holds
the job, and always empty for a terminal job. A waiting job is accepted and
live, normally `pending`; it carries no failure, and the word never
classifies one. The word is advisory display, and a caller MAY branch on it;
it authorizes nothing. Caller exit leaves a waiting job waiting, and Cancel
cancels it as any other live job. A provider relaying a remote job MUST
report the remote word verbatim. A reader MUST treat an unrecognized word as
an unnamed wait.

A provider restarting with accepted jobs whose guarantee depends on
something that cannot answer yet, such as a network cost source that has
not started, MUST open its store and keep those jobs accepted. Each such job
waits, and its word names the missing dependency, such as
`network:unavailable`. The provider MUST offer the guarantee to new
submissions only while the dependency answers, and the waiting job moves
once it does. Other jobs proceed meanwhile. A provider whose execution
profile no longer knows a guarantee still refuses the store under the
storage checks below.

**[JOB-A16] An executor may refuse admission for the caller.** Before a
provider journals, accepts or seals a new idempotency key, the executor of
its kind MAY decide that it can never serve this submission for this caller
as submitted, for example because the submission names a credential the
caller may not apply. Submit then MUST return `invalid` with the executor's
reason, and the provider journals, accepts and seals nothing. The reason is
a stable word the kind defines: the `download` kind uses
`credential:<outcome>:<name>`, where outcome is the credentials applier's
word. The same key is admissible once the cause is repaired. An executor
that cannot obtain its decision MUST make Submit return `unavailable` under
JOB-A9. The check MUST run only for a key with no journal: a duplicate
Submit of an accepted job returns its receipt without asking, and Reconcile
never asks. A job the executor admitted may still end with the same refusal
at execution, when the cause changed in between.

Deprecated job store records and calls keep their existing semantics.

## The deprecated job store and managed ownership

The job store (`Store`, `NewFileStore`, `FileStore`) is deprecated and kept:
no release removing it is named (D30, D33). Its existing records remain
available through an explicitly selected deprecated provider. Managed
service setup does not infer their submitting subject, idempotency key or
epoch, acceptance receipt or execution profile from a job id, lease,
terminal state or external delegation handle.

The Go service configuration checks CheckManaged and OpenManaged, and
OpenWithExecutor with an execution profile, reject an unowned nonempty jobs
directory with ErrLegacyOwnership (also matching ErrIncompatibleStorage).
They leave record, checkpoint and `work/` bytes and existing ownership
unchanged before creating managed metadata or preparing execution. This
refusal identifies the missing ownership transition and certifies nothing
about the contents of that directory. Existing explicit admission-only Open
behavior remains compatible and creates no acceptance mapping for
unjournaled records.

**Storage features.** The private owner header lists the journal encodings a
store has used in `Features`. `abstraction.job/journal-attempts@1` marks
journals for retry attempts (JOB-A7). `abstraction.job/journal-result-lost@1`
marks journals that record a lost result (JOB-A10).
`abstraction.job/journal-labels@1` marks journals that store a display label
(JOB-A12). The provider writes a feature into the header atomically and
durably before the first journal that needs it, and never removes one.
Journals for original requests and results that were never lost, without a
label, use no feature; their bytes match providers that predate features.
CheckManaged, OpenManaged, Open and OpenWithExecutor refuse a header with a
feature they do not support, naming it and matching ErrIncompatibleStorage,
before they create directories, locks or journals. A provider released
before features refuses the `Features` field itself in the same check. A
runtime downgrade fails its storage preflight by name, after the first
retry, recorded loss or stored label, and leaves the store unchanged.

An installation may keep the explicitly selected deprecated provider
available while new submissions use a separate managed service root.
Converting existing jobs requires an authoritative subject mapping and a
fenced transfer preserving immutable jobs and delegate ownership. Current
managed setup offers no such conversion, including for terminal records.
Applications receive no private path from the service API. The focused Go
`legacy_migration_test.go` covers valid records in every state, active
lease and delegation retention, and nonmutating refusal.

## The opaque specification

```json
"kind": "download",
"spec": { "artifact": {}, "sources": [], "sink": {} }
```

**[JOB-K1] The spec is opaque, and `kind` says who can read it.** The store
MUST store and return `spec` without understanding it, byte for byte, down
to how each number and escape was spelled (JOB-J7). A reader that meets a
`kind` it does not know MUST leave that job alone. This is what lets one
module evolve without disturbing the others.

**[JOB-M1] A lease never moves what the job is, and every implementation
refuses a write that tries, with `invalid`.** The set is `id`, `kind`,
`spec`, `created_at` and `envelope`: each is written once, at submit, and
every implementation MUST refuse with `invalid` a write that changes one. A
lease is the right to record what HAPPENED to the job: `state`, `progress`,
`checkpoint`, `delegation`, `error`, `extensions`, and `requires`, which a
holder may widen when it hands the job to a system that demands more of a
successor. A moved spec turns every checkpoint already written into a proof
about a different job, which a successor then resumes from and is wrong
about while every command reports success. The comparison of an opaque half
is on its compact form, because the whitespace inside one belongs to the
record format (JOB-J1); every escape and every number is still compared
exactly (JOB-J7). This is JOB-V4 widened past the envelope.
`TestNoBindingLetsALeaseMoveWhatTheWorkIs` and
`TestALeaseStillWritesWhatALeaseIsFor` in `abstraction-job/go` are the pair:
one refusal, compared across every implementation, and the write a holder is
still owed.

**[JOB-M2] A record handed across an ownership boundary is owned by whoever
receives it, to every depth.** Submit MUST keep nothing of the caller's
record, and Load MUST return nothing of the store's, including the byte
slices inside `extensions`. An implementation that hands out its own state
has an unleased write: a caller assigning into what Load returned changes
the store with no epoch presented, nothing validated and no new
`updated_at`. The same aliasing defeats rollback, because a mutation that
touches shared data and then fails has already landed. The in-memory
implementation is where this is easy to get wrong; the file implementation
gets it by decoding fresh bytes. A store that is not durable is exempt from
nothing else it promises. `TestNoBindingHandsOutItsOwnState` and
`TestAFailedUpdateLeavesTheStoreUnchanged` in `abstraction-job/go` are the
pair.

## Checkpoint and progress

```json
"progress":   { "done": 460 },
"checkpoint": { "verified_prefix": 400 }
```

**[JOB-P1] Progress decides nothing.** `progress` is `BG_JOB_PROGRESS`'s
`BytesTransferred`/`BytesTotal`: best-effort and explicitly non-monotonic.
Nothing MAY decide anything on it: a job resuming after a crash can
legitimately report a smaller number than before.

**[JOB-C1] A successor inherits what its predecessor proved.** The
checkpoint is whatever a successor needs in order to continue, and a
successor MUST resume from the checkpoint. Above, the predecessor wrote 460
units and proved 400: the next owner resumes from 400 and discards the
unproven remainder. This is Temporal's activity heartbeat: a retried worker
is handed the dead worker's last checkpoint and continues from it.
`verified_prefix` is tus' `Upload-Offset` pointing the other way down the
wire, a committed offset a successor resumes at in Kafka's sense of
committed, and the range set below is BitTorrent's piece bitfield (BEP 3),
which aria2 keeps in its `.aria2` control file for the same reason.

A checkpoint may also say **which** ranges are proven, beyond how many
leading bytes: one prefix describes a single stream appending to a file, and
every parallel fetcher lands bytes at scattered offsets.

```json
"checkpoint": { "verified_prefix": 8, "verified": [[0, 8], [16, 20]] }
```

**[JOB-C2] Proven ranges are advisory.** `verified` is declared as
`abstraction.download/ranges@1`. A reader that knows nothing about
`verified` MUST resume from `verified_prefix` and re-fetch the rest, which
is what it did before ranges existed. A reader MUST strip a critical marking
on it, before it checks the list for anything else, and without refusing
(JOB-D10).

**[JOB-C3] Ranges are half-open, non-overlapping, sorted and merged where
they touch.** A writer MUST merge ranges that touch as well as ranges that
overlap: `[[0,4],[4,8]]` and `[[0,8]]` are the same proven bytes, and
admitting both would leave no canonical form to compare across
implementations. Half-open is what gives the merge rule a canonical form at
all: the inclusive spelling has to notice that `[0,3]` and `[4,7]` touch
without overlapping. HTTP's byte ranges are inclusive at both ends, and
every implementation converts at the wire (Divergences).

**[JOB-C4] Empty ranges are dropped, and a fractional offset is refused.** A
reader MUST drop an empty range and MUST refuse a fractional offset: JSON
has one number type, and a decoder that widens `4194304` to a float
re-emits it as one.

**[JOB-C5] A stored prefix and stored ranges that disagree are unioned.** A
reader MUST take the union of `verified_prefix` and `verified`: that is what
a writer predating ranges leaves behind.

## The lease

```json
"lease": { "owner": "go-worker", "epoch": 2, "expires_at": "…" }
```

**[JOB-L1] Every write presents the epoch it holds.** A job lease is
time-bounded, exclusive ownership of one record: the holder keeps it by
renewing and loses it by lapsing. A yield request (JOB-R3) moves its expiry
earlier, and the lapse is what ends it. This is the job sense of lease;
resource's lease (RES-L2) is asked back with a deadline, and the glossary
states both (D89). The epoch MUST rise by one on every acquisition, and
every write except intent MUST present the epoch it holds. A process
suspended past its own expiry wakes up still believing it owns the job; its
writes carry a stale epoch, and the store MUST refuse them. Without the
epoch, two owners work the same job and both believe the result is correct.
The ancestors of each part are in `HISTORY.md`, keyed JOB-L1.

**[JOB-L5] A write after the lease lapsed is refused.** The store MUST
refuse any write made once the lease has lapsed.

**[JOB-L2] Acquiring is a compare-and-set on the record under its lock.**
The store MUST read the record, refuse unless its epoch is the one the
acquirer read and nobody holds a live lease, and write it with the epoch one
higher, all while the acquirer holds byte 0 of `<id>.json.lock`, the lock
`cas` names. The kernel releases that lock when the holder dies, and no lock
is ever broken by a timeout. Two acquirers that read the same epoch cannot
both write it: the second finds the first's lease under the lock and is
refused.

**[JOB-L3] The lock file is never deleted.** The store MUST NOT delete a
record's lock file. A deleted lock file is a new inode, and holders of two
inodes exclude nobody.

The **lock is as much of the agreement as the record**, and this is the one
place where leaving it unwritten fails silently. An implementation that
locks a different byte, a different file, or through a different API
(`fcntl` against `flock` on Linux) passes every single-language test and
loses updates against the others. `abstraction-cas/README.md` § *What a
fourth implementation must do* is the contract;
`abstraction-cas/mixed.py --job` is the instrument, and it fails a foreign
lock on the first run.

**[JOB-L4] Acquiring writes the record as found under the lock.** The store
MUST write the record as it is under the lock and MUST NOT write the
acquirer's copy. An acquirer that read the record before two other
acquisitions went through MUST be refused. Writing its version over theirs
would move the epoch backwards, let an owner whose lease lapsed at that
number pass the staleness check again, and leave two processes working one
job while every command reports success. An acquisition from a record read a
moment ago MUST keep an intent set since.

**[JOB-L6] Release turns `running` back into `pending`.** Releasing a lease
MUST turn a `running` record into `pending`. An owner that honours a pause
releases (JOB-I12), and a record left `running` with nobody holding it is an
owner that died.

`release()` exists so a polite exit frees the job in seconds instead of
after the expiry, and nothing depends on it. A design that *requires*
graceful handoff has no answer for the case that actually loses your 40 GB.

## Where the files are

Normative for anything sharing a directory with this store:

```
<root>/jobs/<id>.json          the record
<root>/jobs/<id>.json.lock     the lock every write to the record holds; never deleted
<root>/jobs/<id>.json.*.tmp    a record being written, renamed over the record
<root>/work/<id>               the name job <id> may spend on scratch
```

**The lock is one machine's.** A byte-range lock taken over SMB and a
`flock` on the server's own volume never meet: writers on two machines on
one record lost 147–149 of 2150 updates in three runs of three
(`abstraction-cas/README.md`). Several processes on one machine, in any
language, lose nothing. A record written from two machines needs a protocol
this store does not have.

**[JOB-W1] `work/<id>` is a name the job shapes.** The store MUST derive the
name from the id, so a successor finds what a predecessor left, and MUST
give it to nothing else. Whether the job makes it a file or a directory is
the job's business: the download module writes its partial there as a file,
and a kind that needs several scratch files makes it a directory.

**[JOB-W3] The store creates `work/` and leaves `work/<id>` to the job.** The
store MUST create `work/` and MUST NOT create `work/<id>`.

This is the layout of the **file implementation**. Nothing written against
the interface may depend on any of it, and an implementation that speaks to
a daemon has no directory at all. Two participants that mean to share one
directory have to agree on it exactly, and that agreement is a contract like
any other.

**[JOB-W2] The root is a shared namespace, and `Reserved` guards it.**
Modules above put files beside `jobs/` and `work/`: the download runner's
`supervisor.json` and `supervisor.sock` are there today. Everything not
listed above belongs to someone other than the store, and none of it is
free. A module that lets a caller name a destination inside this directory
MUST ask `job.Reserved(owner, path)` (`reserved` in Python,
`abstraction::job::reserved` in C++) and refuse what it reserves. Without
it a destination of `jobs/<id>.json` overwrites a record and `work/<other>`
overwrites another job's scratch; both are contained, and `Reserved`
refuses both. See
[`abstraction-download/CONTRACT.md`](https://github.com/openabstractions/abstraction-download/blob/main/CONTRACT.md).

`work/<id>` is a workspace in the sense `GITHUB_WORKSPACE` and Nomad's alloc
dir are: a scratch area the runner is given. `Reserved` is a **reserved
namespace** guarding a **path traversal** (CWE-22, and the Zip Slip family):
Git refuses to check out a path under `.git/` for exactly this reason, and
containment alone is what did not stop it.

## Watching

**[JOB-N1] A change is a visible one.** `Watch` (`watch` in Python and C++)
is a live view of one kind's jobs, over the
[watch](https://github.com/openabstractions/abstraction-watch) module: the
present first, then every change, and quiet once nothing has changed within
its deadline. A watch MUST report a change in identity, state, `progress.done`,
`progress.total`, the lease owner or the error, and MUST NOT report a lease
renewal, which moves `updated_at` and nothing a person can see. A view that
redraws on every heartbeat flickers. Filtering on `kind` keeps the opaque
spec opaque: the module selects on the field it owns and never reads the one
it does not.

## Awake for exactly as long as a lease

`KeepAwake(store, acquired)` keeps the machine from idling into sleep while
this process holds the lease `acquired` carries, and no longer. A runner
takes it at acquisition and releases it when its run ends; underneath, the
hold watches the record and lets go when the lease does (released, lapsed,
or the job turned terminal), and the operating system lets go if the process
dies. The lease is the lifetime, and nothing here has one of its own. That
the hold is taken, follows the lease and no longer, and reports which of the
two reasons it took nothing, is held by `go/awake_test.go`:
`TestHoldFollowsLease` reads the platform's own execution state. The
measurement that asked for it is in `HISTORY.md`.

The assertion is the weakest that answers that: `PowerRequestSystemRequired`
on Windows, `caffeinate -i` on macOS, `systemd-inhibit --what=idle:sleep` on
Linux. The screen may go off; the job carries on. On Windows a power request
holds the machine indefinitely on mains and for at most five minutes past
the sleep timeout on battery, the platform's own promise about a battery.
macOS and Linux are written and not run. The Linux hold also blocks an
explicit suspend, one notch stronger than the other two, because a desktop's
idle action does not reliably honour `idle` alone; the machine unlocks it by
lid or power button as it always did.

A queued job holds nothing. A delegated job holds nothing here: the NAS's
fetch is the NAS's power.

### Who may hold is a service's decision, and this is where it is enforced

`KeepAwakeVia(holder, store, acquired)` asks before it holds. The ancestor
is [polkit](https://www.freedesktop.org/software/polkit/docs/latest/polkit.8.html):
a policy decision point answering *may this subject do this action*, and a
policy enforcement point in every program that acts on the answer. The
decision point is [`rights`](https://github.com/openabstractions/abstraction-rights);
`rights.Registration` (the service and the secret a person's approval gave
the application) is the `Holder` that asks it for `awake` and lets the
service keep the platform request on the application's behalf. What the
service records is the hold itself: `rights holds` shows the application,
the right, the lease owner and the job's kind and id as the reason, when,
and the program the kernel says asked; and the service's log carries the
same line on `hold`, `released` and `revoked`. `rights revoke` ends a live
hold the same second.

Three answers, and each has one meaning:

- **A refusal holds nothing, and the caller is told why.** Not registered,
  not granted, an unidentifiable caller: `Held()` is false and `Why()` is
  what the service said, verbatim. The platform is not asked instead.
- **Nobody answering means the platform.** With no service at the endpoint
  the hold takes the platform's inhibitor itself, exactly as `KeepAwake`
  does, and `Why()` is nil. Absent means permitted here, because the
  measurement this section rests on was of work that slept; an absent
  service must not put it back to sleep. Absent is *nothing listened* and
  nothing else: every answer the service gives, including *refused*, is a
  decision.
- **Decided once, at the moment of asking.** A hold the service grants and
  later takes away (a person revoked the right, or the service stopped, and
  the client cannot tell which) ends, `Why()` is `ErrTakenAway`, and nothing
  falls back to the platform behind the person's back.

`KeepAwake(store, acquired)` is `KeepAwakeVia(Platform, …)`: it asks nobody,
and it is what every caller in this tree still does. The rules above are held
by `TestARefusalHoldsNothingAndSaysWhy`, `TestNobodyAnsweringMeansThePlatform`
and `TestAHoldTakenAwayEndsAndDoesNotFallBack` in `go/`, and against the
running service by `TestAJobAsksBeforeItHolds`,
`TestNoServiceMeansThePlatformHolds` and
`TestAStoppedServiceTakesItsHoldsWithIt` in `abstraction-rights/go`; the
conformance driver has no policy service in its vocabulary, so no scenario
file can reach them, and the `awake` scenario's every `hold` is the absent
case.

## Yield requests: the issuer asks for the lease back

```json
"lease": { "owner": "resident@lmstudio", "epoch": 2, "expires_at": "…",
           "recall": { "reason": "doubled: lemonade holds qwen3.6-35b-a3b",
                       "by": "resident-broker", "at": "…", "until": "…" } }
```

A yield request is carried in `lease.recall`. Without one a lease is only a
promise to wait, and the residency broker could name a model loaded twice
and evict neither copy. The shape's ancestors are in `HISTORY.md`, keyed
JOB-R8.

**[JOB-R8] A yield request is the issuer's demand about the resource, and
intent is untouched.** A yield request MUST leave `intent` unchanged:
`intent` is the user's wish about the job, and a yield request is the
issuer's demand about the resource.

**[JOB-R1] A yield request presents the epoch the issuer read.** A yield
request is addressed to one holding, and the store MUST refuse it
`stale_epoch` if the record has moved since the issuer read it: the fencing
token pointed the other way.

**[JOB-R2] A yield request needs a reason, a live lease and an unfinished
job.** The store MUST refuse a yield request `invalid` without a reason,
`lease_expired` where nobody holds the lease, and `terminal` on a finished
record. `reason` is opaque here, chosen by the issuer for the holder's kind
exactly as `kind` scopes `spec`; `by` says who asked.

**[JOB-R3] The lapse is the eviction.** A yield request MUST move
`expires_at` to `until` where `until` is earlier, and the store MUST refuse
the holder's next write after that deadline as it refuses any lapsed write.
Nothing else evicts.

**[JOB-R4] A renew stops at `until`.** A renew on a lease carrying a yield
request MUST NOT extend `expires_at` past `until`.

**[JOB-R5] The holder cannot shed a yield request by acquiring again.** The
store MUST refuse the holder's acquisition while its lease is held with a
yield request on it. Until the deadline the holder MAY renew and checkpoint:
yielding takes time, and a holder that cannot record what it proved on the
way out loses it. A lease cannot kill a process on another machine. It stops
believing in it, and everything that already refuses a lapsed epoch enforces
the yield request for free. The alternative this shape was chosen over is in
`HISTORY.md`, keyed JOB-R5.

**[JOB-R6] A yield request survives release and expiry.** The store MUST
keep `lease.recall` when the lease is released or lapses. A job whose lease
lapsed or was released after a yield request is an orphan, and a later
reader can tell *nobody renewed* (no yield request) from *asked and yielded*
(yield request, no owner) from *asked and evicted* (yield request, owner
still named, lease lapsed).

**[JOB-R7] A new acquisition carries no yield request.** An acquisition MUST
start a new holding with no `lease.recall`. The issuer asks the new holder
again if it still wants the resource, which is the policy loop WDDM runs
too.

```go
store.Recall(id, epoch, "doubled: lemonade holds it", by, grace)
```

`epoch` is the one the caller saw: a third party asking a holder it has only
read to yield. The residency broker in
[`model/`](https://github.com/openabstractions/abstraction-model) is the
first issuer and the first holder.

## Adoption is the mechanism; handing off is an optimisation

A process killed with `SIGKILL`, or a machine that loses power, never gets
to hand anything over. The primary path is **adoption**: on start, look for
jobs whose lease has expired and acquire them.

```python
for orphan in store.orphans():
    store.claim(orphan.id, "me", ttl_seconds=30)
```

The deprecated store's `claim` is acquire (D24): Azure Blob's lease verb and
the Kubernetes Lease's, beanstalkd's `reserve`, SQS' receive under a
visibility timeout. `orphans()` is Sidekiq Pro's `super_fetch` orphan check,
work whose owner died (Divergences). `Adopt` above it is the controller's,
taken from the same place `ownerReferences` adoption is.

## The record

Each language's implementation is written by hand, from no shared schema.
They agree about one thing: **the JSON file on disk, and the rules for
taking it over.** Each language's API looks like that language.

The table is in the order the keys are written, because that order is part
of the agreement (JOB-J1).

| field | meaning |
|---|---|
| `content` | the data models this record carries, by name. Always present |
| `critical` | the subset of `content` a reader must understand or refuse the record |
| `id` | opaque, sortable by creation time, safe to pass around |
| `kind` | what this job is, and who can read `spec` and `checkpoint` |
| `envelope` | `schema`, `actions`: which schema the opaque halves follow and what may be asked of a job of this kind. Optional, written once |
| `state` | `pending` · `running` · `transferred` · `complete` · `failed` · `cancelled` |
| `spec` | the immutable description of the job (JOB-M1). **Opaque here** |
| `checkpoint` | what a successor needs to resume. **Opaque here**. Omitted until something is proven |
| `progress` | `done`, `total`, `updated_at`, optional `step`: best-effort, decide nothing on it |
| `lease` | `owner`, `epoch`, `expires_at`, and `recall` (`reason`, `by`, `at`, `until`) while the issuer asks for the lease back |
| `delegation` | `system`, `external_id`, `delivered`: set when an external system owns the job |
| `requires` | capabilities an implementation must have to take this job |
| `error` | the last failure, so a person can see why a job stopped |
| `intent` | `want`, `by`, `at`: what somebody WANTS, written without a lease |
| `extensions` | data this module does not understand, keyed by a name that says who does |
| `created_at`, `updated_at` | UTC, exactly six fractional digits, trailing `Z` (JOB-J5) |

**[JOB-F1] A field outside this table is refused.** A reader MUST refuse a
field that is not in this table, at the top level and inside `progress`,
`progress.step`, `lease`, `lease.recall`, `delegation`, `envelope` and
`intent` alike. Continuing a job whose description a reader only partly
understands is the risk this mechanism exists to avoid, and a newer writer's
addition is far more likely than a typo. Refusing is the unusual half
(Divergences). Strictness about fields cannot see a rule, which is what
`critical` is for (JOB-D6).

**[JOB-F2] What a reader could not read goes back untouched.** Anything a
participant needs to carry outside this table goes in `extensions`, under a
namespaced key, and every reader MUST write back untouched what it could not
read: protobuf's unknown-field preservation, and the role `annotations`
plays in Kubernetes and OCI.

`error` is a bare string (Divergences). The classification of a failure is
specified in
[`abstraction-download/CONTRACT.md`](https://github.com/openabstractions/abstraction-download/blob/main/CONTRACT.md#two-endings),
and no record field carries it.

## How a record is written

**[JOB-J1] The encoding is fixed.** The bytes are compared between
implementations. A writer MUST indent with two spaces, write keys in the
order of the table above, end with one trailing newline, and write UTF-8 with
LF line ends.

**[JOB-J2] An empty field is omitted.** `content`, `id`, `kind`, `state`,
`spec`, `progress.done`, `progress.updated_at`, the whole `lease`,
`created_at` and `updated_at` MUST always be present. A writer MUST omit
every other field and sub-field when it is empty: a record with no intent
has no `intent` key, and never a null one.

**[JOB-J3] Absent and empty are the same.** A reader MUST treat absent and
empty as the same thing.

**[JOB-J4] One state has one spelling.** A writer MUST NOT spell the same
state two ways.

**[JOB-J5] A timestamp carries exactly six fractional digits.** Every
timestamp a record carries MUST be UTC with exactly six fractional digits and
a trailing `Z`. Six, because Python's datetime holds microseconds and the
least precise participant sets the format. The cross-language run that found
the drift is in `HISTORY.md`.

**[JOB-J6] A string is escaped exactly here and nowhere else.** A writer MUST
escape `"` and `\\`; U+0008, U+0009, U+000A, U+000C and U+000D as `\b`,
`\t`, `\n`, `\f`, `\r`, and every other character below U+0020 as `\u00xx`
in lower-case hex; U+2028 and U+2029 as ` ` and ` `. **Everything
else MUST be raw UTF-8**: `&`, `<`, `>`, `/`, U+007F, every non-ASCII
character, and every character above U+FFFF as its four UTF-8 bytes, never a
surrogate pair. A byte that can neither begin nor continue a well-formed
UTF-8 sequence MUST be replaced, one byte for one, by the escape `�`.
This is RFC 8785's string serialisation (JSON Canonicalization Scheme, which
takes it from ECMAScript `JSON.stringify`), with two divergences listed
under Divergences.

Go escapes `&`, `<` and `>` by default so a document is safe inside an HTML
`<script>`; Python escapes every non-ASCII character by default so a
document survives a transport that cannot carry high bytes. A record is a
UTF-8 file a runner reads, and both defaults stay off in this encoding.

JOB-J6 governs what a writer **spells**. The bytes of an opaque `spec` or
`checkpoint` are JOB-J7's.

**[JOB-J7] An implementation reproduces the exact bytes of an opaque value
it was given.** An implementation MUST reproduce every escape as it was
spelled, every number as it was written, and every member in the order it
arrived, inside `spec`, `checkpoint` and every `extensions` value, all the
way down. A writer that reads `1.50` and writes `1.5`, or reads `"a\/b"` and
writes `"a/b"`, has imposed its own policy on somebody else's document, and
the next reader downstream sees the changed one.

**[JOB-J8] An opaque field contains one syntactically valid JSON value.** A
reader MUST validate its syntax and retain its original encoded bytes. It
MUST NOT normalise the value or validate its kind-specific schema.
Validation is of the whole value including everything nested in it: correct
string escapes, delimiters, separators, literals and number grammar
(RFC 8259 §§4–7); no unescaped control characters, trailing commas, comments
or truncation; valid UTF-8; and the record's own depth limit (Bounds).
`"a\qb"` is refused, since `\q` is none of the eight escapes JSON has, and
`"a\\qb"`, a backslash followed by `q`, is accepted.

Validate, retain, reproduce: a reader never decodes an opaque value into
ordinary objects and reconstructs it. A validating scanner can record the
value's byte span; a lossless parser also works; bracket matching alone
cannot, because it cannot see an illegal escape. An implementation MUST NOT
convert an opaque number into a host numeric type to validate it: syntactic
validity does not require fitting a `double` or a job counter, and a reader
that converted `123456789012345678901234567890` to check it would either
lose it or refuse it.

**[JOB-J9] Object member names are unique within each object.** Object
member names MUST be unique within each object, including objects nested in
opaque values. Names MUST be compared after JSON escape decoding,
case-sensitively, without Unicode normalization. Accepted opaque values
retain their original encoded bytes. `{"x":1,"x":2}` is refused, since
both names decode to `x`; the same name in two *separate* objects is fine;
`{"a":1,"A":2}` is accepted because the comparison is case-sensitive; and
`{"é":1,"é":2}` is accepted because it does not normalize.
RFC 8259 permits a repeated name and calls a receiver's behaviour
unpredictable, and unpredictable receiver behaviour is the whole damage:
two readers handed identical bytes read different documents out of them.
`abstraction-download` accepted a record whose `spec` names `sources` twice
and downloaded from the second source, and a first-wins reader in another
language downloads a different artifact from the same record. JOB-J7 keeps
the bytes intact in transit and can do nothing about disagreement at the
moment they are interpreted.

Decoding a name in order to compare it licenses no rewriting of the payload
that was retained. The comparison is over the decoded name; what is written
back out is what arrived, escape for escape (JOB-J7). An implementation that
decodes to compare and then re-encodes from its own parse satisfies JOB-J9
and breaks JOB-J7, and `TestOpaqueBytesSurviveNameComparison` in
`abstraction-job/go` is the test that separates the two.

JOB-J9 is stated here in full. `duplicate_keys = "refuse"` in `job.thrift` is
an encoding instruction: it says what the generated readers do, and it
stands. This rule binds every implementation, generated or not.

A reader MUST refuse an unpaired surrogate escape inside an opaque value,
exactly as elsewhere in the record: outside a payload JOB-J6 replaces one
with `�` on write, and inside one a reader may not rewrite what it carries,
and refusing is the one answer that invents no second policy. Every
record-wide restriction MUST apply inside an opaque value on the same terms,
and no implementation relaxes one there.

Arbitrary non-JSON bytes go in a JSON string or in an artifact the record
points at. A raw opaque value is never a route for embedding malformed JSON
inside an otherwise valid record: the record is a UTF-8 file a runner reads,
and one bad escape at any depth makes the whole file unreadable to every
parser that is not ours.

JOB-J7 leaves two things out, both somebody's rule. The payload's own
**whitespace** is not carried: the record is canonical by construction under
JOB-J1, which fixes the indent for the whole file, and a payload written on
one line and a payload written on six are the same document written the
same way. **U+2028 and U+2029** are escaped inside a payload as everywhere
else, because the reason for escaping them concerns a document being read by
a JavaScript parser and does not stop at a field boundary.

Carrying an opaque payload byte for byte needs a reader that can still
address a value's byte range in the input, and a tree can do that as well as
a token stream. Go's standard library addresses a value's byte range
directly, Python's `raw_decode` returns an end index to slice at, and .NET's
`JsonElement.GetRawText()` indexes offsets over the original buffer.
`go/opaque_test.go` holds it: `TestOpaqueBytesSurvive` submits a spec
spelling every scalar a way this package would not choose (a redundant
solidus escape in a value and in a key, `1.50`, `1e2`, `-0.0`, an integer
past float64, members in an order nothing sorts them into) and fails if any
of it comes back re-spelled. Go compacts an opaque payload's whitespace
(`marshalCompact`), and the record encoder re-indents whatever it is handed.
No implementation preserves a payload's whitespace, and JOB-J1 governs it.

## Declarations: what a record carries and must be understood

```json
"content":  ["abstraction.job/base@1", "abstraction.job/intent@1"],
"critical": ["abstraction.job/base@1", "abstraction.job/intent@1"]
```

A record declares the data models it carries in two lists and carries no
`schema` field. `content` names every data model in this record, a
**profile declaration** in the sense of JSON-LD's `@context` and RFC 6906's
`profile`: the document says which vocabularies it is written in, and a
reader looks them up rather than guessing from the keys. The lineage of the
mechanism (X.509 critical extensions, JSON Schema `$vocabulary`, JOSE `crit`
and COSE) is in `HISTORY.md`.

**[JOB-D1] An unknown critical name refuses the record.** `critical` is the
subset of `content` a reader MUST understand. A reader MUST refuse the
record over a name in `critical` it does not know, and MUST carry the record
and act on the rest over an unknown name only in `content`. This is COSE's
reading of `crit` (RFC 9052 §3.1), which binds the reader.

**[JOB-D7] `unknown_schema` is its own outcome.** A reader MUST answer a
record refused under JOB-D1 with `unknown_schema` and MUST keep it distinct
from `invalid`. The two ask for opposite things: `invalid` says the record
is wrong and a caller may discard it; `unknown_schema` says the record is
fine and **this reader is too old**, and the one correct response is to
leave it for something newer. Collapsing them is how a critical name gets
acted on as corruption.

**[JOB-D8] `critical` is a subset of `content`.** A reader MUST refuse a
record naming in `critical` a model absent from `content`, and a writer MUST
drop such a name on write. A name marked critical and not carried is a
declaration about nothing. A reader MAY take `content` as the whole roster
and `critical` as a marking on it.

**[JOB-D10] The two rules that read `critical` apply in one order.** A
reader MUST first remove from `critical` every name the table below marks
*never*, then check what is left for a name it does not know (JOB-D1,
refuse) and for a name absent from `content` (JOB-D8, refuse). Stripping
first is what makes JOB-C2 true against JOB-D8: a writer that wrongly marked
an advisory model critical cannot stop a reader that would otherwise have
done the work, and the marking is gone from what that reader writes back.
Stripping refuses nothing and permits ignoring no criticality: a name
outside the table still refuses the whole record. A prohibited marking is a
diagnostic for whoever wrote it, which is where every ancestor puts it:
RFC 5280 §4.2 says *"Conforming CAs MUST mark this extension as
non-critical"* and tells no reader to reject a certificate over a CA that
did not.

The names are normative, and this is the whole list:

| name | covers | in `critical` |
|---|---|---|
| `abstraction.job/base@1` | `id`, `kind`, `state`, `spec`, `checkpoint`, `progress`, `lease` | always |
| `abstraction.job/intent@1` | `intent` | whenever present |
| `abstraction.job/delegation@1` | `delegation` | whenever present |
| `abstraction.job/step@1` | `progress.step` | never: advisory, and a reader that ignores it is correct about everything that matters |
| `abstraction.download/ranges@1` | `checkpoint.verified`: which bytes are proven, beyond how many leading ones (JOB-C2) | never: a marking on it is stripped, never refused (JOB-D10) |
| `abstraction.job/terminal@1` | the rule that a terminal record refuses its own holder's update, release and intent (JOB-D6) | whenever `state` is terminal |
| `abstraction.job/recall@1` | `lease.recall`, and the rules that a renew stops at `until`, that the holder cannot acquire again, and that the lapse is the eviction (JOB-D9) | whenever `lease.recall` is present |
| `abstraction.job/envelope@1` | `envelope`, and the rule that it is written once and never moved by a lease holder (JOB-V5) | whenever `envelope` is present |
| an extension key | that extension's value | only if its writer says so |

**[JOB-D6] A content name may name a rule.** `abstraction.job/terminal@1`
names a rule, and a writer MUST mark it critical whenever `state` is
terminal. The unit of `critical` here is a **feature**, a named bundle of
obligations, some fields and some rules: `abstraction.job/recall@1` names a
field and three rules about writes at once, and `abstraction.job/base@1`
names seven fields. Naming a rule is the mechanism's missing half: terminal
enforcement was added to this format under an unchanged
`abstraction.job/base@1`, and a reader published before it walks a
`complete` record back to `pending`. A `go/v0.1.0` lease holder did exactly
that, and the `terminal` conformance scenario is what refuses it now. A
shape can be ignored; a rule about writes cannot, because the reader that
does not know it is precisely the reader that breaks it. A change to what a
record *means* is declarable here on the same terms as a change to what it
carries.

**[JOB-D9] `abstraction.job/recall@1` travels with `lease.recall`.** A writer
MUST declare `abstraction.job/recall@1` in `content` and mark it critical
whenever the record carries `lease.recall`. The name covers the field and
the three write rules JOB-R3, JOB-R4 and JOB-R5.

**[JOB-D2] Both lists are derived on every write.** A writer MUST derive
`content` and `critical` from what the record actually carries on every
write, never from memory, and a declaration cannot drift from the data. A
record that gained an intent since it was last written says so on the next
write without anybody updating a list.

**[JOB-D3] Extension keys are appended sorted.** A writer MUST append
extension keys to `content` in sorted order, because this output is compared
byte for byte between implementations.

**[JOB-D4] The schema integer is read and never written.** A reader MUST
read the integer `schema` of a version 3, 4 or 5 record by the mapping
below, and a writer MUST NOT write it. Stores full of version 3 and 4
records exist on real disks and on a NAS. The mapping is exact: those
versions are frozen, and what each one could contain is known.

| `schema` | means `content` of | plus `delegation@1` if the record has one |
|---|---|---|
| 3 | `base@1` | yes |
| 4 | `base@1`, `intent@1` | yes |
| 5 | `base@1`, `intent@1`, `step@1` | yes |

**[JOB-D5] Every other schema integer is refused.** A reader MUST refuse
every other value: 1, 2, 6, 99 and the rest. `step@1` is the one model the
mapping does not mark critical.

## The envelope: which schema, and what may be asked

```json
"envelope": {
  "schema": "nas.example/transfer@2",
  "actions": ["cancel", "nas.example/transfer@2#re-mirror"]
}
```

`kind` says WHO may read `spec` and `checkpoint`. It does not say WHAT they
are, and it carries no version. A runner that did not create a job could
once do exactly two things with it: find it orphaned and reclaim it. The
envelope is the missing half: it lets a runner written by somebody else act
on a kind it was never built for without ever opening the opaque halves. Its
ancestors, Protocol Buffers' `Any` and AIP-151's opaque `metadata`, and the
one divergence from them are under Divergences.

**[JOB-V1] A schema identifier is a name, and nothing dereferences it.** A
schema identifier MUST match `namespace "/" name "@" version`, where a
namespace is dot-separated labels, a label is `[a-z0-9]` with interior `-`,
a version is `[1-9][0-9]*`, and the whole is at most 128 bytes: the same
spelling as a content name, one grammar for names in this record. There is
no scheme, no authority, no percent-escape, no backslash, no query, and
exactly one `/` and one `@`. `https://example.invalid/x@1`, `\\host\share`,
`../../etc` and `file:///x` are illegal values, and a reader MUST refuse
them with `invalid` before any runner sees them. No implementation MAY offer
a function from a schema identifier to a schema. A runner that fetched an
address out of somebody else's record would be running attacker-chosen
content as a service on the owner's machine, and this rule is a grammar
because a grammar is checkable.

**[JOB-V2] An action name is a name a runner matches, and never anything
executable.** An action name MUST be either `bare` (`[a-z0-9]` with interior
`-` and `_`, at most 64 bytes) or `<schema>#<bare>`. Nothing in the record
can say *how* to do anything, and no implementation performs an action;
matching is the only operation defined on the field. The separator is `#`
because it is the one component of a URI reference defined never to be sent
anywhere (RFC 3986 §3.5): a fragment names something inside a document.

**[JOB-V3] Every action name resolves to a schema the record declares.** A
bare name belongs to `abstraction.job/base@1` and MUST be one of its four
words, `pause`, `resume`, `cancel` and `recall`, each naming a store call the
kind declares it honours. A qualified name's schema part MUST equal this
record's own `envelope.schema`. A reader MUST refuse a base action spelled
the long way (`abstraction.job/base@1#pause`): one action has one spelling,
and `supports` is a comparison. Anything else is `invalid`. Two vendors will
otherwise both define `cancel`, and a runner matching on the bare name alone
would perform one of them believing it had performed the other. An extension
that wants actions of its own needs its own envelope (Not built).

**[JOB-V4] A supported action is a property of the kind.** The envelope is
written at submit and never moves, and every implementation MUST refuse a
write that moves it with `invalid`. The in-process implementations compare
the envelope before and after the caller's own closure; the service
implementation compares the record it holds against the one the message
carries. The struct has two fields and no third: anything reporting what is
true *now* would make a runner try to pause a job whose worker died an hour
ago.

**[JOB-V5] A record carrying an envelope declares
`abstraction.job/envelope@1` and marks it critical.** A writer MUST declare
it and mark it critical whenever `envelope` is present. A reader that
ignored the envelope would carry on and then write the record back without
it, deleting the kind's own declaration, and the loss would be invisible
because the thing destroyed is the description.

**[JOB-V6] A runner asks and never resolves.** The answer to *may I do this*
MUST be computed from the record's declaration and from the schemas and
actions the asking process already implements, which it passes in. Three
refusals answer three different questions: `unknown_schema` (the record
follows a schema this runner was not written against: leave the job alone);
`not_supported` (the kind does not declare this action, or this runner has
not built it); `invalid` (the name is no action name, and somebody should be
told). Every refusal MUST name what was refused: a runner that declines in
silence is indistinguishable from one that quietly did the wrong thing.

**Backward compatibility, precisely.** A record written before this field
existed loads unchanged and re-encodes byte for byte: `envelope` is optional
and omitted when absent. A record that DOES carry an envelope is **refused
by a reader too old to know the field**, because every struct in this
format refuses unknown fields (JOB-F1). That refusal is the feature: an old
runner that ignored the field would rewrite the record without it, and
JOB-V5 is the same argument from the other side. Adopting the envelope for a
kind is a flag day for that kind's readers, paid per kind and never per
store.

## Intent: what somebody wants, written without a lease

```json
"intent": { "want": "pause", "by": "comfyui@desktop:9184", "at": "2026-09-05T09:12:44.180000Z" }
```

`intent` is the `spec` half of Kubernetes' `spec`/`status` split, and
`deletionTimestamp` in particular: a field anyone with write access may set,
that the party doing the work converges on in its own time. Argo spells the
same thing `spec.suspend` and `spec.shutdown`. The three values are BITS'
Resume, Suspend and Cancel. It is a field of its own because this record's
`spec` is opaque and immutable (JOB-M1).

**[JOB-I1] `want` is one of three words.** `want` MUST be exactly one of
`run`, `pause` and `cancel`, and a reader MUST refuse anything else and
never treat it as `run`.

**[JOB-I2] Absent means `run`.** A reader MUST read an absent intent as
`run` and MUST NOT distinguish "nobody asked" from "somebody asked for it to
run"; that keeps version 3 records, which have no intent at all, readable.

**[JOB-I3] Intent is the one write that presents no epoch.** The store MUST
accept an intent write without an epoch. The party who wants a job stopped
is usually some process other than the one doing it, and requiring a lease
would mean stealing the job in order to stop it, the one thing the lease
exists to prevent.

**[JOB-I4] Intent is idempotent.** The store MUST accept a repeated intent
as it accepted the first.

**[JOB-I5] A terminal record refuses intent.** The store MUST refuse an
intent once `state` is terminal: nothing reopens finished work.

**[JOB-I6] An intent the owner cannot carry out is accepted.** The store
MUST accept an intent whatever the current owner can do; only the owner
knows what it can do.

**[JOB-I7] `run` records a value.** Writing `run` MUST record
`{"want":"run", "by":…, "at":…}` and MUST NOT delete the field. The record
keeps declaring `abstraction.job/intent@1` in `content` and `critical` from
then on, and a reader too old to know that model refuses the record instead
of working on a job somebody may have asked to stop.

**[JOB-I8] An owner checks the intent before it starts and as often as it
checkpoints.** An owner MUST check the intent at least as often as it
checkpoints **and before it starts**, and MUST move toward it.

**[JOB-I9] Every owner honours `cancel`.** Every implementation MUST honour
`cancel`: stopping is universal.

**[JOB-I10] An owner that cannot pause fails the job.** An implementation
that cannot honour `pause` MUST fail the job with a reason and stop. A pause
that quietly does nothing is worse than no pause button.

**[JOB-I11] A paused job is no orphan.** `Orphans` MUST NOT offer a paused
job: a sweep that adopted it would restart the work seconds after a person
stopped it.

**[JOB-I12] A paused job still `running` is an orphan.** `Orphans` MUST offer
a paused job whose `state` is still `running` and whose lease nobody holds.
An owner that honours a pause releases the lease, and releasing turns
`running` back into `pending` (JOB-L6). A record left `running` with nobody
holding it is an owner that died between the pause being asked for and the
pause being carried out. That one IS abandoned, and excluding it makes the
state permanent: nothing may acquire the job, and nothing may pause, resume
or cancel it ever again.

The check before starting and the sweep exception are **one rule in two
places**, and an implementation needs both. Sweeping alone restarts a
download seconds after a person stopped it; checking alone leaves the record
unreachable forever.

In the store interface:

```go
store.SetIntent(id, want, by) // want is run, pause or cancel
```

## Who moves the state

**[JOB-S1] The store writes two states.** The store MUST write `pending` on
submit and `running` on a successful acquisition, and MUST write no other
state. Nothing else in this module ever sets one.

**[JOB-S2] The lease holder writes `transferred`, `complete` and `failed`.**
These three MUST be written by the lease holder, through the ordinary
epoch-checked update, and by nothing else. This module cannot decide them:
deciding them means knowing whether the work is done, and `spec` and
`checkpoint` are opaque here. The kind above knows.

**[JOB-S3] The owner honouring `intent: cancel` writes `cancelled`.** An
owner MUST write `cancelled` when it honours `intent: cancel`.

**[JOB-S4] An unheld job is cancelled by the caller itself.** Where nobody
holds the lease, the caller MUST take it for a moment and write `cancelled`
itself. A cancelled job never sits visibly running until some runner
notices. Go's `job.Open(store, id, me).Cancel()` does exactly that, and the
intent alone is what reaches an owner on another machine.

**[JOB-T1] A terminal record refuses acquisition, update and intent.**
`complete`, `failed` and `cancelled` are terminal, and the store MUST refuse
acquisition, update and intent on a terminal record with `terminal`.

**[JOB-T2] `transferred` stays acquirable.** `transferred` is
deliberately outside the terminal set, and the store MUST accept an
acquisition of a `transferred` record, so the requester can take delivery.

**[JOB-T3] Terminal is judged on the record as read.** The store MUST judge
terminality on the record as read, never on what the write leaves behind.
That is the only reason a job can ever finish: the update that *makes* a
record terminal is an ordinary epoch-checked write onto a record that is not
yet terminal, and it lands. The write after it is refused.

**[JOB-T4] The final update carries everything.** An owner MUST put
everything it wants recorded into the same update as the final state: the
error text, the last checkpoint, the final `done`. Nothing of that epoch
writes again, and an implementation that checkpoints *after* finishing has
the ordering wrong.

**[JOB-T5] A terminal record refuses release.** The store MUST refuse
`release` on a terminal record: release is an update, a finished job has
nothing to hand over, and the lease lapses on its own.

**[JOB-T6] Renew is the one write a terminal record accepts.** The store
MUST accept `renew` on a terminal record. It is the one write that changes
nothing a reader may branch on: only the current holder's own expiry, on a
record nobody may acquire, update or intend regardless. Refusing it would
give a lease keeper racing its owner's final write a failure to interpret,
for no observable difference.

**[JOB-G1] A delegated job's external system is the truth.** When an
application's worker hands off to a system service, or that service hands
off to a NAS, the handle that finds the job again is `{system,
external_id}`: for Windows BITS, a job GUID that survives a reboot. It is
Camunda's external task and CSI's external provisioner, and the field is the
external id that Stripe and Salesforce mean by the phrase. When `delegation`
is set, a reader MUST treat `progress` as a cache of what the external
system last reported. Delegation is the architecture every hop uses.

`transferred` means the work is finished and proven and the result has not
been delivered. BITS has the same two-phase shape for the same reason, and
will not hand over a file until you call `Complete()`. Collapsing the two
states leaves no way to express *"the service finished this while ComfyUI
was closed"*, which is the case this is built for.

## Outcomes

The acceptance services answer with an outcome word on every call:

| Call | Outcome | Meaning |
| --- | --- | --- |
| Submit, Reconcile | `accepted` | A receipt for the key's job (JOB-A2, JOB-A4). |
| Submit, Reconcile | `definitely_not_accepted` | Never accepted, and sealed against delayed acceptance (JOB-A4). |
| Submit, Reconcile | `unknown` | No evidence either way; reconcile again (JOB-A4, JOB-A5). |
| Submit, Reconcile | `key_conflict` | The key was accepted with different arguments (JOB-A2, JOB-A7). |
| Submit, Reconcile | `forbidden` | An evaluated authorization refusal. |
| Submit, Reconcile | `invalid` | Invalid input, an ineligible attempt (JOB-A7), a bad label (JOB-A12) or an executor's refusal (JOB-A16). |
| Submit, Reconcile | `unavailable` | A policy decision could not be obtained; nothing recorded (JOB-A9). |
| Cancel | `requested` | Cancellation intent recorded; effects may still finish (JOB-A6). |
| Cancel | `already_terminal`, `unknown`, `forbidden`, `unsupported` | Distinct refusals (JOB-A6). |
| Cancel | `unavailable` | No intent recorded (JOB-A9). |
| Observe | `observed`, `unknown`, `forbidden`, `invalid`, `definitely_not_accepted`, `unavailable` | Only `observed` carries a snapshot (JOB-A9, JOB-A10). |
| ReadResult | `data`, `not_ready`, `unavailable`, `unsupported`, `unknown`, `forbidden`, `invalid` | Only `data` carries a chunk (JOB-A10, JOB-A11). |
| List, `ListAccountWork` | `page`, `gap`, `forbidden`, `invalid`, `unavailable` | Only `page` carries snapshots (JOB-A13). |

The job store's calls answer `ok` or one of `not_found`, `lease_held`,
`stale_epoch`, `lease_expired`, `terminal`, `unknown_schema`, `invalid` and
`refused`. **The class of refusal is the contract, and its wording is
free**: pinning wording would make every improved message a cross-language
breakage.

| verdict | outside name |
| --- | --- |
| `stale_epoch` | gRPC `ABORTED`, whose own gloss is a sequencer check failure; HTTP `412 Precondition Failed` |
| `lease_held` | `409 Conflict`; Azure's `LeaseAlreadyPresent` |
| `lease_expired` | Azure's `LeaseIdMismatch` |
| `terminal` | `FAILED_PRECONDITION` |
| `invalid` | `INVALID_ARGUMENT`; `400` |
| `not_found` | `NOT_FOUND` |
| `unknown_schema` | none: its own class (JOB-D7) |

### The conformance transcript

```bash
bash scripts/behaviour-conformance.sh
```

`behaviour-conformance.sh` gives every implementation the same scripted
operations and its own store, and compares the transcripts byte for byte.
The Go driver is the one registered implementation, judged against the
contract pages. A scenario lives in `abstraction-download/testdata/scenarios/`,
one operation per line, and a driver named `replay` runs it. Each line comes
back as `NN <the operation> -> <verdict> <fields>`, the verdict one of the
store verdicts above.

The fields are always these eleven, in this order:

| field | meaning |
|---|---|
| `state` | `pending`, `running`, `transferred`, `complete`, `failed`, `cancelled` |
| `epoch` | the lease generation, which only rises |
| `held` | `yes` while a live lease exists, `no` otherwise |
| `recall` | the reason the issuer gave with its yield request, or `none`: what a holder branches on |
| `want` | `run`, `pause` or `cancel`: what somebody asked for |
| `done` | progress in the kind's own units |
| `err` | `set` if an error was recorded, `none` otherwise |
| `cp` | the checkpoint, compact JSON, or `none` |
| `content` | what the record declares it carries, comma separated |
| `crit` | the subset of it a reader must understand, comma separated |
| `awake` | `yes` while this driver holds the machine awake for the record's lease |

They are printed after a refused operation too: **what a refusal leaves
behind is the half of it a caller has to live with.** Nothing else is
compared: timestamps, ids and owner strings, mid-transfer progress and
concurrent writers are each outside the transcript, and each language's own
tests hold what they can.

A driver answers `--capabilities` with `store`, or `store transfer` if the
language also has a download runner. The roster is fixed, and a language
that cannot run a scenario is **printed as a gap and counted**, never
skipped. A driver also answers `--models` with the content-set names it can
read, one per line, each marked `critical-ok` or `never-critical`; the
harness diffs the rosters against each other and against the table under
Declarations.

The operations a scenario may write are `submit`, `claim`, `renew`,
`progress`, `hold`, `release`, `finish`, `intent`, `recall`, `orphans`,
`state`, `run`, `stage`, `plant` and `sleep`. `claim` acquires and `recall`
issues a yield request. `hold <alias>` keeps the machine awake for the lease
the record carries, as a runner does at acquisition.
`recall <alias> <owner> <grace-ms> [reason…]` is issued against the epoch
that owner holds, which is what an issuer would have read.
`plant <alias> content|critical <name>` writes a name straight into the
record's declaration on disk, the one thing a conforming writer cannot do:
an implementation refuses to write a record it could not read back, and the
only way to reach its own refusal path is to forge what a newer writer would
have written.

### Every invariant carries a name

Every rule on this page and on
[`abstraction-download/CONTRACT.md`](https://github.com/openabstractions/abstraction-download/blob/main/CONTRACT.md)
carries an id, and a scenario cites that id on the `# expect` line testing
it:

```
# expect 6: terminal [JOB-T1]
```

A tag inside a fenced block, like the one above, is an example and declares
nothing. `behaviour-conformance.sh` reads the ids off both pages, reads the
ids the scenarios cite, and prints the difference. **A rule nothing cites is
printed and counted as UNEXERCISED.** The id is an accounting device and
never a source of truth: nothing is generated from it. A tag marks a rule
about what an implementation must **do** with a record; the transcript
format is the instrument and carries none. This is a **requirements
traceability matrix**: DO-178C's bidirectional trace between a requirement
and the test that exercises it, IEEE 830's numbered shall-statements, the
QUIC interop matrix.

## Bounds

| what | bound | rule |
| --- | --- | --- |
| display label | 1 to 256 bytes of UTF-8 on one line, after trimming | JOB-A12 |
| waiting word | 1 to 64 bytes of `[a-z0-9_.:-]` | JOB-A15 |
| schema identifier | at most 128 bytes | JOB-V1 |
| bare action name | at most 64 bytes | JOB-V2 |
| record nesting, opaque values included | at most 64 levels (`depth_limit`) | JOB-J8 |
| timestamps | exactly six fractional digits | JOB-J5 |
| `ReadResult` `max_bytes` | 1 to 65536 | `operations@1` |
| List page | 1 to 64 snapshots | `inventory@1` |
| history retention | declared per provider; the Go provider declares 24 hours and keeps history indefinitely | JOB-A5 |
| lock | one machine: writers on two machines lose updates | JOB-L2 |

## Divergences

- **JOB-F1.** Refusing an unknown field is the unusual half. Nearly
  everything an adopter has used ignores what it does not know: protobuf
  keeps unknown fields and re-emits them, JSON Schema's
  `additionalProperties` defaults to true, HTTP recipients ignore
  unrecognised headers, and Postel's advice is the general form. This
  record refuses, and the download module one level up ignores unknown
  spec keys on purpose: a record is a contract several languages share, and
  a spec is payload the module above extends.
- **`error`.** `error` is a bare string, a divergence from
  `google.rpc.Status{code, message, details}` (AIP-193) and Problem Details
  (RFC 9457), from the same AIP family this record takes `spec` from. It is
  undermeasured: the *not now* against *no* classification, the most
  load-bearing thing this module decides, is recoverable from a record only
  through `state`, and the reason is prose no machine reads.
- **JOB-C3.** Ranges are half-open, and HTTP's byte ranges are inclusive at
  both ends (RFC 9110 §14.1.2): `[[0, 8]]` is 8 bytes and `bytes=0-8` is 9.
  It is the one divergence on either contract page that produces a wrong
  *number*. Every implementation converts at the wire: it asks for
  `bytes=<start>-<end-1>` and reads a `Content-Range` of `<first>-<last>`
  back as `[first, last+1)`. A source answering `bytes 40-47/64` to a
  request for `bytes=40-` leaves `done=48`:
  [`wire-short-range`](https://github.com/openabstractions/abstraction-download/blob/main/testdata/scenarios/wire-short-range.txt).
- **JOB-J6.** RFC 8785 leaves U+2028 and U+2029 raw; this record escapes
  them, because they are the only characters legal in a JSON string and
  illegal in a JavaScript string literal before ES2019, and because adding
  an escape is exact where removing one is not. A string holding the six
  characters ` ` is itself written `\\u2028`, which a search for the
  escape matches one byte into. RFC 8785 keeps a lone surrogate as
  `\ud800`; this record replaces it, and writes the replacement as an escape
  instead of the U+FFFD glyph, so a reader can see that a byte was lost.
- **JOB-J9.** The duplicate-name restriction is I-JSON's, **RFC 7493 §2.3**:
  *"Objects in I-JSON messages MUST NOT have members with duplicate names."*
  This record adopts that restriction and claims no conformance to the
  I-JSON profile, which also constrains numbers, top-level values and time
  formats on terms this format settles for itself.
- **JOB-V1.** Protocol Buffers' `Any` carries a `type_url` that is a URL by
  construction; this record's schema identifier is a name that cannot be
  one. It took `Any`'s self-describing half and AIP-151's opaque `metadata`,
  and dropped dereferencing. `actions` has no ancestor in that family: the
  nearest are D-Bus, where a method is an (interface, member) pair and the
  interface is reverse-DNS, and Kubernetes RBAC, where a bare verb is scoped
  by a separately named `apiGroup`. Neither carries the declaration on the
  object itself.
- **Awake hold.** polkit's subject is the calling process, and any library
  can ask; `rights` designates an application by its secret, a library
  cannot, and the adopter brings the registration.
- **`orphans()`.** Kubernetes' `orphan` is a deliberately un-parented object
  (`propagationPolicy: Orphan`); this store's orphan is work whose owner
  died, Sidekiq Pro's sense.
- **JOB-S1, JOB-S2.** The state names are BITS' `BG_JOB_STATE_*`, with one
  exception. `pending` is `QUEUED`, `running` is `TRANSFERRING`,
  `transferred` is `TRANSFERRED`, `failed` is `ERROR`, `cancelled` is
  `CANCELLED`. **`complete` is BITS' `ACKNOWLEDGED`**, and the state an
  English reader would call "complete" is the one before it: a reader who
  maps `complete` onto "the transfer finished" is a state early, and a
  runner written that way stops before delivery is taken. It is an
  acknowledgement in the sense AMQP's `basic.ack` and SQS' `DeleteMessage`
  are; Kubernetes and Azure both spell it `Succeeded`, and Celery `SUCCESS`.

## Not built

- **Authorized job discovery** (JOB-A1). A caller that lost its key before a
  receipt arrived reports unknown; a future version recovers it through an
  authorized discovery call.
- **`abstraction.job/result.read`** (JOB-A14): reading another subject's
  result bytes. Version 1 refuses it, and this is the next name the contract
  would reserve.
- **Converting existing jobs** from the deprecated store to managed service
  ownership, including terminal records (The deprecated job store and
  managed ownership).
- **An extension's own envelope** (JOB-V3): actions an extension defines for
  itself.
- **A record written from two machines** (Where the files are): the lock is
  one machine's, and no protocol exists.
- **A kill during a real multi-gigabyte transfer**: no test has made one.
