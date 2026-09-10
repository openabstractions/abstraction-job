# job — behavioural specification

What an implementation of the job layer must do with a record, and nothing
else. Fields and their types are the schema's business; files, sockets and
locks are the binding's; examples are the corpus's. See `METHOD.md` § 11 for
the four jurisdictions and which artefact holds each.

**Status.** Drawn from `job/README.md`'s tagged rules, `job/job.thrift`,
`job/go/interface.go`, the three stores, and the scenarios under
`download/testdata/scenarios/`. Where those disagree the disagreement is
recorded in § 12, not resolved by fiat. Nothing here is invented: a behaviour
none of them settles is in § 11, *Undecided*, and nowhere else.

**Reading it.** Every rule carries a tag. A `JOB-` tag that already appears on
`job/README.md` is the same rule, cited by the same scenarios. A tag minted
here is listed in § 13 so the harness can be pointed at this page; until it is,
those rules are counted UNEXERCISED by `behaviour-conformance.sh` whatever a
scenario does. Each rule names the scenario that fails it, or says none does.

Verdict classes are the contract; their wording is not:
`ok` · `not-found` · `lease-held` · `stale-epoch` · `lease-expired` ·
`terminal` · `unknown-model` · `invalid` · `refused` (JOB-D7; § 6).

---

## 1. Vocabulary

- **record** — one job, identified by an opaque `id` that outlives every
  process. Google's `longrunning.Operation` (AIP-151): a handle to work you
  did not start, with an opaque payload typed by `kind`.
- **holder** — the party whose `lease.owner` and `lease.epoch` the record
  carries while `expires_at` is in the future.
- **epoch** — Chubby's sequencer (§ 2.4); DDIA's fencing token. Rises by one on
  every claim, never otherwise.
- **issuer** — whoever wrote a `recall`; a party that read the record and
  holds nothing.
- **kind** — who may read `spec` and `checkpoint`. This layer never does
  [JOB-K1].
- **terminal** — `complete`, `failed`, `cancelled` [JOB-T1].

## 2. The state machine

**2.1 [JOB-S1]** The store writes exactly two states: `pending` on submit and
`running` on a claim of a `pending` or `running` record. — `lease` 9,
`matrix` 8.

**2.2 [JOB-T2]** A claim of a `transferred` record leaves it `transferred`.
`transferred` is not terminal; it is BITS `TRANSFERRED` waiting for
`Complete()`, and it is claimable so the requester can take delivery. —
`matrix` 32, `orphans` 15.

**2.3 [JOB-S2]** `transferred`, `complete` and `failed` are written by the
holder through the epoch-checked update. The store cannot decide them: it
does not know what the work is. — `matrix` 20, 23, 36.

**2.4 [JOB-S3] [JOB-S4]** `cancelled` is written by the holder honouring
`intent: cancel`, or, when nobody holds the lease, by the asker taking the
lease for one write. — `cancel-adoption` 7; § 4.

**2.5 [JOB-T1]** A terminal record refuses claim, update and intent with
`terminal`. — `matrix` 40–45, 49–54, 58–63; `terminal` 4–7.

**2.6 [JOB-T3] [JOB-T4]** Terminal is judged on the record as read. The write
that makes a record terminal lands; the next write of that epoch is refused.
So the final update carries everything — error, checkpoint, `done`. —
`terminal` 3, 6.

**2.7 [JOB-T6]** `renew` is the one write a terminal record accepts. It changes
nothing a reader branches on. — `matrix` 41, 50, 59.

**2.8 [JOB-L6]** `release` turns `running` into `pending`. **All three
implementations exempt a delegated record**, which stays `running` because a
sweep that saw `pending` would start the work a second time; the README states
no exemption. The undelegated half is tested (`matrix` 17); the delegated half
is asserted by nothing in the corpus. See § 12.1.

**2.9 [JOB-M1]** The store guards no transition except terminality. A holder
may write any non-terminal state onto any non-terminal record, backwards
included — `transferred` to `pending`, `running` to `pending` — and the
download kind does so when a delegate disappears. Which moves are legal for a
kind is that kind's rule; this layer refuses only writes onto a terminal
record. — `matrix` 7 is refused for the lease, not the direction. No scenario
asserts a backward move lands. Whether the store should refuse one is § 11.1.

**2.10 [JOB-M2]** Every state is reachable through the contract: the corpus
drives all six with `finish` (`matrix`). *Four of six states are unreachable
from the contract* is no longer true of the contract.
It remains true of C++ product code, which writes `pending` and `running` and
nothing else; the C++ conformance driver writes all six. See § 12.6.

Transitions the store allows, holder-written unless marked:

| from | to | by |
|---|---|---|
| — | `pending` | submit (store) |
| `pending`, `running` | `running` | claim (store) |
| `running` | `pending` | release (store), unless delegated |
| any non-terminal | any state | holder's update |
| non-terminal | `cancelled` | a momentary claimant, when unheld |
| terminal | — | nothing |

## 3. Ownership

A lease **in the Chubby sense**: time-bounded ownership kept by renewing and
lost by lapsing, never by being asked. `coordination.k8s.io/v1 Lease`, etcd's
TTL, SQS's visibility timeout, beanstalkd's TTR. The epoch-checked write is
`If-Match` with a strong ETag (RFC 9110 § 13.1.1).

**3.1 [JOB-L1]** Every write except intent presents the epoch it holds; a
mismatch is `stale-epoch`. — `lease` 8.

**3.2 [JOB-L2]** A claim is a compare-and-set under the record's lock: read,
refuse unless the epoch is the one the claimant read and no live lease stands
in its way, write with epoch + 1. — `lease` 3, `recall` 7.

**3.3 [JOB-L3]** The lock file is never deleted. — `cas/mixed.py --job`; no
scenario can see it.

**3.4 [JOB-L4]** A claim writes the record as found under the lock, never the
claimant's copy: an intent set since the claimant read survives, and a record
that moved is refused `lease-held`. — `lease` 3; the intent-survives half is
asserted by Go's and Python's `a_claim_keeps_what_was_written_since_the_caller_read`,
by no C++ test found, and by no scenario.

**3.5 [JOB-O1]** A live lease held by another owner refuses a claim
`lease-held`. The plainest thing a lease does, cited by `lease` 3 and `matrix`
11, stated on no page until now.

**3.6 [JOB-O2]** The current holder may claim again while its lease is live,
and the epoch rises; it may not while recalled [JOB-R5]. Three implementations
do this; no page says so; `recall` 7 tests the refusal and nothing tests the
re-claim. It let one process race itself and report a delivered download as a
failed install. Whether a
same-owner re-claim should be a no-op returning the held epoch is § 11.2.

**3.7 [JOB-O3]** A claim needs a non-empty owner. Refused; the verdict class is
not fixed — Go answers a bare error, Python and C++ a `JobError`. § 11.3.

**3.8 [JOB-O4]** `expires_at` is the claim's instant plus the ttl the claimant
asked for, on the store's own clock. A lease is held while `owner` is
non-empty and now is before `expires_at`. Nothing here says whose clock: the
lock is one machine's (`README` § Where the files are), and a record held from
two hosts has no protocol. § 11.4.

**3.9 [JOB-L5]** Expiry signals nothing and stops nothing. A holder past its
expiry learns of it at its next write, which is refused `lease-expired` while
nobody has claimed since and `stale-epoch` once somebody has — the verdict
tells a lapsed holder whether it has a successor. — `lease` 6, 8.

**3.10** Renew refuses `stale-epoch` on a wrong epoch and `lease-expired` on a
lapsed one even when the epoch matches, because a process suspended past its
expiry wakes up believing it is the owner; re-claiming bumps the epoch and
fences whatever it had in flight. — `recall` 10; the wrong-epoch half is
untested in the corpus.

**3.11 [JOB-C1] [JOB-P1]** Work in flight when a lease lapses is lost to the
successor except what the checkpoint proved. A successor resumes from
`checkpoint`, never from `progress`, which decides nothing. Temporal's
activity heartbeat. — `lease` 6–9 (`done` reverts to the last write that
landed), `download/README.md` [DL-R1].

**3.12 [JOB-O5]** A second claimant after a lapse gets epoch + 1 and the
record otherwise as the predecessor left it: state, checkpoint, error, intent,
extensions. Only `recall` is cleared [JOB-R7]. — `lease` 9, `recall` 13.

**3.13 [JOB-I11] [JOB-I12] [JOB-T2]** `orphans` offers a record that is
claimable and stranded: not terminal, not held, not `transferred`, and not
paused — unless it is still `running`, which is an owner that died between a
pause being asked and carried out. — `orphans` 4–15.

**3.14 [JOB-O6]** `orphans` does not read `delegation`. A delegated record
whose lease has lapsed is offered, and the kind must decline it — the download
runner skips delegated orphans because the external system is the truth
[JOB-G1]. No scenario drives a delegated orphan. Whether the store itself
should withhold it is § 11.5.

**3.15 [JOB-D1] [JOB-D7]** A record whose `critical` names a model this reader
lacks is refused `unknown-model` on load, claim and intent, and is not offered
by `orphans`. The record is fine; this reader is too old. — `unknown-model`
5–9.

### Recall

The half a Chubby lease lacks: WDDM's budget signal, Android's trim. Addressed
to one holding, so it presents the epoch the issuer *read*.

**3.16 [JOB-R1] [JOB-R2]** Refused `stale-epoch` if the record moved,
`invalid` without a reason, `lease-expired` when nobody holds it, `terminal`
on a finished job. — `recall` 3, 4, 16, 19.

**3.17 [JOB-R3] [JOB-R4]** A recall moves `expires_at` to `until` where
earlier; renew never extends past `until`; the lapse is the eviction and
nothing else is. — `recall` 5, 10.

**3.18 [JOB-R5] [JOB-R6] [JOB-R7] [JOB-R8]** The holder cannot shed a recall by
re-claiming; a recall survives release and expiry; a new claim carries none;
`intent` is untouched. — `recall` 7, 11, 13, 15; 5.

## 4. Cancellation

One word, four operations, and a caller has to know which it is holding.

| operation | what it is | who | crosses a process boundary |
|---|---|---|---|
| `intent: cancel` | a request written to the record | anyone, no epoch [JOB-I3] | yes — that is its purpose |
| `state: cancelled` | the outcome, written by a holder | the holder [JOB-S3], or a momentary claimant [JOB-S4] | it is the record |
| stopping the execution | the holder stopping itself at its next intent check | the holder [JOB-I8] | never — nothing here reaches into a process |
| stopping the wait | a caller's local cancellation (a Go `ctx`) | the caller | no; writes nothing to the record |

**4.1 [JOB-I3] [JOB-I4] [JOB-I5]** Intent is the one write with no epoch; it is
idempotent; it is refused `terminal` once the job is over. — `matrix` 6, 14,
45.

**4.2 [JOB-I8] [JOB-I9]** A holder checks intent before it starts and at least
as often as it checkpoints, and moves toward it. Cancel is honoured by every
implementation. — `cancel-adoption` 6 (checked on adoption, before any byte);
mid-run honouring is asserted by `download/go/intent_test.go`
(`TestCancelStopsALiveTransfer`) and by no scenario.

**4.3 [JOB-X1]** Cancelling never stops an execution. It records a request;
the acknowledgement is the holder's own write of `cancelled`. A caller that
must know the work stopped waits for the state, never for the intent call to
return — Go's `handle.Cancel` returns success when somebody holds the lease,
because recording the request *is* the success. Drawn from `job/go/handle.go`;
no scenario asserts the return value.

**4.4 [JOB-S4]** When nobody holds the lease, the asker claims for one write
and writes `cancelled` itself, so a cancelled job is not shown `running` until
a sweep notices. The lease is not released after: the record is terminal and
the lease lapses on its own. — `cancel-adoption` 7 tests the adopter's write;
the asker's own write is Go-only and untested in the corpus.

**4.5 [JOB-X2]** Delegated work is stopped by whoever reconciles the
delegation, by calling the external system's abandon (the download layer's
`Delegator.Abandon`; BITS `Cancel`, which also deletes what arrived). The job
layer has no rule and no scenario for it. § 11.6.

**4.6 [JOB-I10] [JOB-L6]** Pause: a holder that cannot pause fails the job
with a reason; one that can releases, so the record reads `pending`. —
`pause-adoption` 6, `download/README.md` [DL-R27]. Pause on a delegated
record: no rule, § 11.6.

**4.7** Store and handle disagree on a double cancel: the store refuses
`intent` on a `cancelled` record `terminal` [JOB-I5]; Go's handle absorbs it
as a double click. The transcript verdict is `terminal`. § 12.10.

## 5. Acceptance

The instant ownership of a request transfers. Before it, an uncertain outcome
means nothing was taken and retrying is free. After it, an uncertain outcome
is **unknown**, and a retry starts a second job.

**5.1 [JOB-A1]** A submission is accepted when the record exists under its
id. The store mints the id unless the submitter supplies one; a supplied id
that already exists is refused `invalid`, in all three. Not in the corpus;
each language's own tests.

**5.2 [JOB-A2]** So a submitter whose answer was lost can tell only if it
named the id: `invalid` then means *accepted, load it*. A submitter that let
the store mint the id has no way to ask, and retrying makes a second job. The
download kind deduplicates above this on the artifact (`inFlight`), which is
the kind's rule, not this layer's.

**5.3 [JOB-A3]** A claim transfers ownership at the instant its write lands
under the lock [JOB-L2]. A retried claim is safe by construction: if the first
landed, the retry is either `lease-held` (another claimant) or a same-owner
re-claim at one epoch higher [JOB-O2]; if it did not, the retry is the claim.

**5.4 [JOB-A4]** An update whose answer was lost is unknown, and the store
offers no way to ask. A retried final write is answered `terminal`, which is
the one case that tells the caller it landed. A retried checkpoint lands twice
and moves `updated_at`, which backoff reads (§ 8). There is no request
identity on the write path. § 11.7.

**5.5 [JOB-I4]** A retried intent is free: idempotent by rule.

**5.6 [JOB-A5]** For a handoff to an external system, the download layer
writes the delegation *before* handing over, with `external_id` equal to the
record id meaning *unsettled*, and asks the delegate's `Locate` when the
answer is lost — three answers: a handle, a certain never, or unknown. The
request identity is the record id, so it is the same string on every attempt
in every language. XA's `xa_recover` shape. This is Go-only; the job layer
can express *delegated* and not *possibly delegated*, so the equality is a
second spelling of a state. § 11.8, § 12.9.

**5.7 [JOB-A6]** A retried recall rewrites `at` and `until` from the retry's
instant. The record's `expires_at` is only ever lowered, so a retry can leave
`until` later than `expires_at`. Whether a repeated recall may extend the
deadline is § 11.9.

## 6. The answers, and the fourth shape

Three answers that are not two, from `demo/answer`: **unavailable** (nothing
that could do this is here now; the request stands), **forbidden** (something
answered no; asking again is pointless), **unknown** (no answer arrived — not
a no, not a yes, not a later). Download spells the first two *not now* and
*no* [DL-E1] [DL-E2] and the third `ErrOutcomeUnknown`.

**6.1 [JOB-Q1]** The verdict classes map as follows, and what each obliges is
the whole reason they are separate:

| verdict | answer | the caller must |
|---|---|---|
| `lease-held`, `lease-expired`, `stale-epoch` | unavailable, to this caller | re-read the record and decide again; create nothing |
| `terminal`, `invalid`, `not-found` | forbidden | end this request; a retry is a new request |
| `unknown-model` | neither | leave the record for a newer reader; never discard it, never retry here |
| no verdict | unknown | reconcile before retrying: load, `Locate`, or resubmit by id (§ 5) |

`not-found` is forbidden on the file binding, where absence is definitive on
one machine. Across a share it is unexamined.

**6.2 [JOB-Q2]** The transcript format cannot express *unknown*: a transcript
is one process on its own store. The job layer has no sentinel for it either;
only the download layer does. The socket binding was measured returning `ok` for
writes that left the record in a different state. § 11.10.

**6.3** `refused` remains in the verdict list after `unknown-model` was split
from it. What it names now is § 11.11.

**6.4 [JOB-Q3]** A partial answer — some records read, some unreadable — is a
fourth shape that neither a value nor an error carries alone. Go's `List` and
`Orphans` return the records beside an `ErrUnreadable` naming the ids and
unwrapping to the first reason. **Python and C++ skip an unreadable record
and return success.** Elasticsearch's `_shards` and Kubernetes'
`RemainingItemCount` are the shape. Live divergence; the cross-language rule is
§ 11.12.

## 7. Release

Documented as a courtesy in `interface.go`, `job.thrift` and the README;
implemented as an update, refusing `stale-epoch`, `lease-expired` and
`terminal` [JOB-T5]. An advisory operation cannot have a refusal that means
something, so it is one or the other.

**7.1 [JOB-T5]** **Release is an epoch-checked write, not an advisory one.**
Its refusals mean exactly what update's do: `stale-epoch` and `lease-expired`
say the caller holds nothing; `terminal` says the job is over and the lease
lapses by itself. *Courtesy* means only that nothing depends on it — a holder
that never releases costs one ttl of delay, never correctness. — `terminal` 8,
`matrix` 43, 52, 61; `bindings_test.go`, `test_bindings.py`,
`test_job_record.cpp`.

**7.2 [JOB-T4]** Consequently a holder does not release after its final write:
the state write ends its epoch. `defer release()` around a run that finishes
is a call that cannot succeed, and `download/go/runner.go` has discarded that
error on every successful job. Release on
every exit that is not a final write. A distinguished *nothing to release*
answer is § 11.13.

**7.3** Effects: `expires_at` = now, `owner` = empty, `running` → `pending`
unless delegated [JOB-L6, § 2.8]; `recall` survives [JOB-R6]. Release on a
`transferred` record lands and leaves it `transferred` in all three; `matrix`
27 marks it undecided. § 11.14.

## 8. Retries and backoff

Retry classification, as old as SMTP's `4yz`/`5yz`: BITS `TRANSIENT_ERROR`
against `ERROR`, a soft bounce against a hard one.

**8.1 [JOB-B1]** This layer classifies nothing. `error` is a bare string
(declared divergence from `google.rpc.Status`, README § The record); the only
machine-readable *no* is `state: failed` [DL-E2], and *not now* is an error
on an adoptable record [DL-E1]. The kind decides, at the place each error is
defined, never in a list [DL-E3]. — `failure-endings`.

**8.2 [JOB-B2]** The attempt count is the lease epoch: it rises on every claim,
exists in three languages, and needs no field. The download kind reads
`updated_at + min(15 s · 2^(epoch−1), 15 min)` for a record that carries an
error, and adopts a record without one at once — a killed owner wrote nothing.
Go and Python (`RetryAfter`, `retry_after`); C++ has no runner. The constants
are the kind's tuning. Tagged nowhere, cited by no scenario. § 11.15.

**8.3 [JOB-B3]** Taking a lease in order to publish a refusal is a write: it
bumps the epoch and moves `updated_at`, so it charges the submitter's backoff
and stamps this machine's policy onto a record another machine could still
serve. The download runner therefore declines to claim, during a sweep, a job
whose sink it cannot write, and refuses out loud only when asked by name. —
`download/go/installtakesaway_test.go`; no scenario.

**8.4 [JOB-B4]** A delegate that vanished without attempting the bytes writes
no error, so it is not an attempt and does not back off. — `delegator.go`
Reconcile; no scenario.

**8.5** Lease verdicts are *not now* to the claimant only: `lease-held` retries
after `expires_at`; `lease-expired` and `stale-epoch` retry by re-claiming,
never by re-presenting the epoch.

## 9. Declarations and version skew

**9.1 [JOB-V1]** There is no version on the wire and no negotiation. A peer
may assume nothing about a peer built on a different day. Measured 2026-09-07,
on that day's build: four combinations of a client and a service one day apart
all connect, none refuses, and two finish the same script with the job in a
different state.

**9.2 [JOB-D1] [JOB-D6] [JOB-D9]** The only version-shaped thing is the
record's `content` and `critical`: JOSE `crit` (RFC 7515 § 4.1.11) for the
shape, JSON Schema `$vocabulary` for naming a *rule*. `terminal@1` and
`recall@1` name rules; a reader lacking one refuses the record rather than
breaking it. What a peer may assume is therefore: what the record's `critical`
declares, and only when the peer reads through the declaring decoder.

**9.3 [JOB-F1] [JOB-D5] [JOB-D4]** On the file binding, an unknown field is
refused, an unknown legacy `schema` integer is refused, and 3, 4, 5 are read
and never written. — `unknown-model`.

**9.4 [JOB-V2]** On the socket binding an unknown request field is ignored and
an unknown op is `unknown_op`. The socket ships to nobody (`job.Serve` has one
caller, a test). Whether a connection must declare what it enforces is § 11.16.

> **Corrected 2026-09-08, and the correction is the lesson.** This rule
> previously also said `content` and `critical` are *stripped in both
> directions* and that an unknown record field is *dropped where the disk would
> refuse it*. **Both were false when written.** Probed against the live server:
> both fields are present in every response, a record arriving without them is
> refused `invalid: a record must say what it contains`, and an unknown record
> field is refused `invalid: json: unknown field`. The clauses were taken from a
> measurement of an older build and read as though a measurement of behaviour
> stays true. **An instrument's number is a
> series, not a fact — and so is an instrument's *verdict*.** A specification
> that cites a measurement cites its date, or it is quoting a value.

## 10. Watching and the hold

**10.1 [JOB-N1]** A change is a visible one: identity, state, `progress.done`,
`progress.total`, the lease owner, the error. A renewal is not a change. —
`notice-*` scenarios.

**10.2 [JOB-H1]** A hold on the machine's idle sleep lives exactly as long as
one epoch's lease on non-terminal work: it ends when the lease is released,
lapses, or the job turns terminal, and the same owner claiming again after a
lapse does not revive it. A queued or delegated job holds nothing. — `awake`;
tagged on no page.

## 11. Undecided

Each with what the three implementations do today, where they agree, and what
would settle it. An adopter meets these as surprises.

1. **Backward state moves.** All three let a holder write `transferred` →
   `pending` or `running` → `pending`. Settled by a scenario asserting either
   `ok` or a refusal class — and by naming the class, which does not exist.
2. **Same-owner re-claim while live.** All three bump the epoch. Settled by a
   scenario; the alternative is returning the held epoch unchanged.
3. **Verdict class for an empty owner.** Go: bare error; Python, C++:
   `JobError`. Neither is in the verdict list.
4. **Whose clock.** Each store reads its own. Nothing says what two hosts on
   one share may assume; the lock does not cross hosts either.
5. **Delegated orphans.** The store offers them; the download kind declines.
   Whether `delegation` withholds a record from `orphans` in the store.
6. **Intent on a delegated record.** Who honours cancel and pause when the
   holder released to a delegate. Download's Reconcile does; no rule, no
   scenario.
7. **Request identity on the write path.** An update's lost answer cannot be
   asked about. Idempotency key (Stripe), operation name (AIP-151) or recovery
   scan (XA) — the accept feedback picked XA for delegation; nothing picked for
   the store.
8. **Possibly delegated.** `external_id == id` is Go's encoding of unsettled.
   A nullable handle plus an `accepted` flag on `delegation` is the schema
   change that would say it; three-language.
9. **A repeated recall.** Rewrites `at`/`until`; may leave `until` past
   `expires_at`. Extend, shorten, or refuse.
10. **`unknown` in the job layer.** No sentinel, no verdict class, no
    transcript shape.
11. **What `refused` names** now that `unknown-model` exists.
12. **Partial enumeration across languages.** Go names the unreadable ids
    beside the result; Python and C++ skip silently.
13. **A distinguished answer for release after a final write.**
14. **`matrix` 25–27:** update, intent and release on `transferred`. All three
    answer `ok`; no page decides.
15. **Where backoff lives.** In the download kind today, on the epoch. Whether
    the job layer owns the formula or only the counter.
16. **The socket binding's decoder and a connection-level declaration.**
17. **`matrix` 2, 12:** renew on a never-held record (all three:
    `lease-expired`) and renew on a live lease (all three: `ok`).

## 12. Where the sources disagree

1. **[JOB-L6]** README: release turns `running` into `pending`. Go, Python,
   C++: unless delegated. Corpus tests only the undelegated case.
2. **Release.** `interface.go`, `job.thrift`, README: a courtesy. Three stores
   and three test suites: refuses `terminal`, `stale-epoch`, `lease-expired`.
   Decided § 7.1 in the code's favour, with the cost named.
3. **`job.thrift` exception lists** predated the terminal rules: `claim` listed
   no `Invalid`, `release` no `LeaseExpired` or `Terminal`, `renew` and
   `update` no `Terminal`. Closed 2026-09-08: the file is now the generated
   definition, it declares no exceptions and no per-operation `throws` list, and
   the eleven verdict words are one flat enum every operation may answer with.
4. **Partial enumeration.** Go beside; Python and C++ skip. § 6.4.
5. **`refused`** survives in the verdict list; README says Go used it for
   what is now `unknown-model`.
6. **Reachability.** *Four of six states are unreachable* is stale for the
   contract; still true of C++ product code. § 2.10.
7. **Intent absent from the contract** and **no backoff anywhere** are answered
   by [JOB-I1..I12] and § 8.2.
8. **Same-owner re-claim.** Three implementations, no page, one measured
   corruption. § 3.6.
9. **Unsettled delegation as an equality.** Go only; a Python or C++ reader
   sees a settled delegation and can never settle it. § 5.6.
10. **Double cancel.** Store: `terminal`; Go handle: `ok`. § 4.7.
11. **Delegated orphans.** Store offers; download declines; README silent.
    § 3.14.
12. **`lease` 3 says it:** *no rule on either page says a live lease refuses
    another claim.* Now [JOB-O1].

## 13. Tags minted here

Not on `job/README.md`; the harness counts them UNEXERCISED until it reads
this page.

`JOB-M1` `JOB-M2` · `JOB-O1` `JOB-O2` `JOB-O3` `JOB-O4` `JOB-O5` `JOB-O6` ·
`JOB-X1` `JOB-X2` · `JOB-A1` `JOB-A2` `JOB-A3` `JOB-A4` `JOB-A5` `JOB-A6` ·
`JOB-Q1` `JOB-Q2` `JOB-Q3` · `JOB-B1` `JOB-B2` `JOB-B3` `JOB-B4` ·
`JOB-V1` `JOB-V2` · `JOB-H1`

Rules cited by a scenario today and by this page: every `JOB-` tag in § 2–§ 4
and § 9 that also appears on the README. Rules with no scenario anywhere:
3.3, 3.6 (the re-claim half), 3.14, 4.3, 4.4 (the asker's write), 4.5, 5.1,
5.4, 5.6, 5.7, 8.2–8.4, 10.2's tag.
