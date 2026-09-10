# Contract

Every rule this layer states, each carrying a tag, in the order they were
decided. A conformance scenario cites the tag it tests on its `# expect` line,
and a citation that resolves to no rule here is a defect in one of the two.

[README.md](README.md) is the door — what this layer is, how to obtain it, one
example that runs. No rule on that page carries a tag.

## Four rules, and why each exists

### 1. The spec is opaque, and `kind` says who can read it [JOB-K1]

```json
"kind": "download",
"spec": { "artifact": {}, "sources": [], "sink": {} }
```

This package stores and returns the spec without understanding it — byte for
byte, down to how each number and escape was spelled [JOB-E7]. A reader that
meets a `kind` it does not know leaves that job alone rather than guessing.

That is what lets one layer evolve without disturbing the others — and it is the
answer to the fair objection that an abstraction which changes shape every time a
new tool shows up is not an abstraction, it is a union of tools.

**[JOB-M1] A lease does not move what the work IS, and every binding refuses a
write that tries, with `invalid`.** The set is `id`, `kind`, `spec`,
`created_at` and `envelope`: each is written once, at submit. A lease is the
right to record what HAPPENED to the work — `state`, `progress`, `checkpoint`,
`delegation`, `error`, `extensions`, and `requires`, which a holder may widen
when it hands the work to a system that demands more of a successor. Move the
spec instead and every checkpoint already written becomes a proof about a
different job, which a successor then resumes from and is wrong about while
every command reports success. The comparison of an opaque half is on its
compact form, because the whitespace inside one belongs to the record format
[JOB-E1] and not to the payload; every escape and every number is still compared
exactly [JOB-E7]. This is [JOB-V4] widened past the envelope, and it is stated
here because it was believed and not held: the in-process bindings ran the
caller's closure and checked only the envelope afterwards, so a changed spec was
written and reported as success, while the service binding left these fields out
of the fields a write copies and so SILENTLY DISCARDED the same change. Two
bindings answering one call differently is the failure this whole page exists to
prevent, and neither half of it was reachable by a test that asked one binding
one question. `TestNoBindingLetsALeaseMoveWhatTheWorkIs` and
`TestALeaseStillWritesWhatALeaseIsFor` in `job/go` are the pair: one refusal,
compared across all three bindings, and the write a holder is still owed.

**[JOB-M2] A record handed across an ownership boundary is owned by whoever
receives it, to every depth.** Submit keeps nothing of the caller's record and
Load returns nothing of the store's, including the byte slices inside
`extensions`. An implementation that hands out its own state has an unleased
write: a caller assigning into what Load returned changes the store with no
epoch presented, nothing validated and no new `updated_at` — and the same
aliasing defeats rollback, because a mutation that touches shared data and then
fails has already landed. The in-memory binding is where this is easy to get
wrong and where it WAS wrong; the file binding gets it by decoding fresh bytes.
Not being durable exempts a store from nothing else it promises.
`TestNoBindingHandsOutItsOwnState` and `TestAFailedUpdateLeavesTheStoreUnchanged`
in `job/go` are the pair.

### 2. A successor inherits what its predecessor proved [JOB-C1]

```json
"progress":   { "done": 460 },
"checkpoint": { "verified_prefix": 400 }
```

`progress` is `BG_JOB_PROGRESS`'s `BytesTransferred`/`BytesTotal`: best-effort,
explicitly non-monotonic, and nothing may decide anything on it [JOB-P1] — a job
resuming after a crash can legitimately report a smaller number than before. The survey went looking for a standard here and found five
systems refusing to define one.

The **checkpoint** is the load-bearing part: whatever a successor needs in order
to continue. Above, the predecessor wrote 460 units but proved only 400, so the
next owner resumes from 400 and the unproven remainder is discarded.

This is Temporal's activity-heartbeat design, copied deliberately: a retried
worker is handed the dead worker's last checkpoint rather than starting over.
`verified_prefix` is tus' `Upload-Offset` pointing the other way down the wire —
a committed offset a successor resumes at, in Kafka's sense of committed — and
the range set below is BitTorrent's piece bitfield (BEP 3), which aria2 keeps in
its `.aria2` control file for the same reason.

A checkpoint may also say **which** ranges are proven, not only how many leading
bytes — one prefix can describe a single stream appending to a file and nothing
else, and every parallel fetcher lands bytes at scattered offsets.

```json
"checkpoint": { "verified_prefix": 8, "verified": [[0, 8], [16, 20]] }
```

Declared as `abstraction.download/ranges@1`, and **advisory** [JOB-C2]: a reader
that knows nothing about `verified` resumes from `verified_prefix` and re-fetches
the rest, which is what it did before ranges existed. A reader strips a critical
marking on it rather than merely declining to add one — before it checks the
list for anything else, and without refusing [JOB-D10].

> ### `[[0, 8]]` is 8 bytes. `bytes=0-8` is 9.
>
> **Our ranges are half-open; HTTP's byte ranges are inclusive at both ends**
> (RFC 9110 §14.1.2), and this page sends a reader to that RFC. Same word,
> different arithmetic, and it is the only divergence on either contract page
> that produces a wrong *number* rather than a wrong expectation. Every
> implementation converts at the wire: it asks for `bytes=<start>-<end-1>` and
> reads a `Content-Range` of `<first>-<last>` back as `[first, last+1)`.
>
> Half-open is what gives the merge rule below a canonical form at all: the
> inclusive spelling has to notice that `[0,3]` and `[4,7]` touch without
> overlapping. It was not chosen from a measurement, and saying otherwise would
> be inventing one. What is measured is the conversion: a source answering
> `bytes 40-47/64`
> to a request for `bytes=40-` leaves `done=48` in all three implementations —
> [`wire-short-range`](https://github.com/openabstractions/abstraction-download/blob/main/testdata/scenarios/wire-short-range.txt).

Ranges are half-open, non-overlapping, sorted, and merged where they **touch**
as well as where they overlap [JOB-C3] — `[[0,4],[4,8]]` and `[[0,8]]` are the
same proven bytes, so admitting both would leave no canonical form to compare
across implementations. Empty ranges are dropped; a fractional offset is
refused [JOB-C4], because JSON has one number type and a decoder that widens
`4194304` to a float re-emits it as one. Where a stored prefix and stored ranges
disagree they are unioned [JOB-C5]: that is what a writer predating ranges
leaves behind.

### 3. Ownership is a lease with an epoch

```json
"lease": { "owner": "go-worker", "epoch": 2, "expires_at": "…" }
```

**A lease in the Chubby sense**: time-bounded ownership the holder keeps by
renewing and loses by lapsing, never by being asked to give it up — the same
thing `coordination.k8s.io/v1` Lease spells `holderIdentity` + `renewTime`, etcd
spells as a TTL, SQS calls a visibility timeout and beanstalkd calls TTR.
**The epoch is Chubby's sequencer** (§2.4), which DDIA names a fencing token,
Kafka names a leader epoch and the Kubernetes Lease counts as
`leaseTransitions`. **The epoch-checked write is `If-Match` with a strong
ETag** (RFC 9110 §13.1.1) — etcd's `mod_revision`, Kubernetes' `resourceVersion`.

The epoch rises by one on every claim, and **every write must present the epoch
it holds** [JOB-L1]. A process suspended past its own expiry wakes up still
believing it owns the job; its writes carry a stale epoch and are refused, and so
is any write made once the lease has lapsed [JOB-L5].

Without that, two owners work the same job and both believe the result is
correct. That is the damage this design exists to prevent.

A claim is a compare-and-set on the record under its lock [JOB-L2]: the record
is read, refused unless its epoch is the one the claimant read and nobody holds
a live lease, and written with the epoch one higher — all while the claimant
holds byte 0 of `<id>.json.lock`, the lock `cas` names, which the kernel
releases when the holder dies, so no lock is ever broken by a timeout. Two
claimants that read the same epoch cannot both write it: the second finds the
first's lease under the lock and is refused. The lock file is never deleted
[JOB-L3] — a deleted lock file is a new inode, and holders of two inodes
exclude nobody.

The **lock is as much of the agreement as the record**, and this is the one
place where leaving it unwritten fails silently. An implementation that locks a
different byte, a different file, or through a different API — `fcntl` against
`flock` on Linux — passes every single-language test and loses updates against
the others. `cas/README.md` § *What a fourth implementation must do* is the
contract; `cas/mixed.py --job` is the instrument, and it fails a foreign lock on
the first run.

And a claim writes the record as it is under the lock, never the claimant's
copy [JOB-L4]. A claimant that read the record before two other claims went
through is refused rather than writing its version over theirs — otherwise the
epoch goes backwards, an owner whose lease lapsed at that number passes the
staleness check again, and two processes work one job while every command
reports success. And a claim from a record read a moment ago keeps an intent
set since: the compare-then-rename this replaced compared the epoch and wrote
the caller's copy, which erased the intent while reporting success.

### Where the files are

Normative for anything sharing a directory with this store:

```
<root>/jobs/<id>.json          the record
<root>/jobs/<id>.json.lock     the lock every write to the record holds; never deleted
<root>/jobs/<id>.json.*.tmp    a record being written, renamed over the record
<root>/work/<id>               the name job <id> may spend on scratch
<root>/services.json           the discovery registry
```

**The lock is one machine's.** A byte-range lock taken over SMB and a `flock`
on the server's own volume never meet: writers on two hosts on one record lost
147–149 of 2150 updates in three runs of three (`cas/README.md`). Several
processes on one host, in any of the three languages, lose nothing. A record
written from two hosts needs a protocol this store does not have.

**`work/<id>` is a name, not a shape** [JOB-W1]. The store guarantees that the
name is derived from the id, so a successor finds what a predecessor left, and
that nothing else will use it. Whether the job makes it a file or a directory is
the job's business: the download layer writes its partial there as a file, and a
kind that needs several scratch files makes it a directory. The store creates
`work/`; it does not create `work/<id>` [JOB-W3].

This is the layout of the **file binding**, not of the abstraction. Nothing
written against the interface may depend on any of it — a binding that speaks to
a daemon has no directory at all — but two participants that mean to share one
directory have to agree on it exactly, and that agreement is a contract like any
other.

**The root is a shared namespace.** Layers above put files beside `jobs/` and
`work/` — the download supervisor's `supervisor.json` and `supervisor.sock` are
there today. So "everything not listed here is not the store's" is true and does
not mean it is free: a layer that lets a caller name a destination inside this
directory has to ask, and `job.Reserved(owner, path)` (`reserved` in Python,
`abstraction::job::reserved` in C++) is the answer [JOB-W2]. Without it a destination of
`jobs/<id>.json` overwrites a record and `work/<other>` overwrites another job's
scratch — both contained, both accepted, until 2026-09-06. See
[`download/CONTRACT.md`](https://github.com/openabstractions/abstraction-download/blob/main/CONTRACT.md).

`work/<id>` is a workspace in the sense `GITHUB_WORKSPACE` and Nomad's alloc dir
are: a scratch area the runner is given rather than one it picks. `Reserved` is
a **reserved namespace** guarding a **path traversal** (CWE-22, and the Zip Slip
family) — Git refuses to check out a path under `.git/` for exactly this reason,
and containment alone is exactly what did not stop it.

---

## Watching, and what counts as a change

`Watch` (`watch` in Python and C++) is a live view of one kind's jobs, over the
[watch](https://github.com/openabstractions/abstraction-watch) layer: the present first, then every change, and
quiet once nothing has changed for a budget. **A change is a visible one:
identity, state, `progress.done`, `progress.total`, the lease owner or the
error** — a lease renewal moves `updated_at` and nothing a person can see, and
a view that redraws on every heartbeat flickers, so a renewal is not a change
[JOB-N1]. Filtering on `kind` is what keeps the opaque spec opaque: the layer
selects on the field it owns and never reads the one it does not.

## Awake, for exactly as long as a lease

`KeepAwake(store, claimed)` keeps the machine from idling into sleep while
this process holds the lease `claimed` carries, and not a moment longer. A
runner takes it at claim and releases it when its run ends; underneath, the
hold watches the record and lets go when the lease does — released, lapsed,
or the job turned terminal — and the operating system lets go if the process
dies. The lease is the lifetime; nothing here has one of its own.

It was added because a measurement demanded it: Windows put a laptop into
Modern Standby with work in flight and the work ran at a third of its speed
for four minutes, because nothing in the chain had told the platform a job
was running. The abstraction is the only party that knows. That the hold is
taken, follows the lease and no longer, and reports which of the two reasons it
took nothing, is held by `go/awake_test.go` — `TestHoldFollowsLease` reads the
platform's own execution state rather than this package's opinion of it.

The assertion is the weakest that answers that: `PowerRequestSystemRequired`
on Windows, `caffeinate -i` on macOS, `systemd-inhibit --what=idle:sleep` on
Linux. The screen may go off; the job carries on. On Windows a power request
holds the machine indefinitely on mains and for at most five minutes past the
sleep timeout on battery — the platform's own promise about a battery, not
ours. macOS and Linux are written and not run. The Linux hold also blocks an
explicit suspend, one notch stronger than the other two, because a desktop's
idle action does not reliably honour `idle` alone; the machine unlocks it by
lid or power button as it always did.

A queued job holds nothing. A delegated job holds nothing here, which is the
point of delegation: the NAS's fetch is the NAS's power.

### Who may hold is a service's decision, and this is where it is enforced

`KeepAwakeVia(holder, store, claimed)` asks before it holds. The ancestor is
[polkit](https://www.freedesktop.org/software/polkit/docs/latest/polkit.8.html):
a policy decision point answering *may this subject do this action*, and a
policy enforcement point in every program that acts on the answer. The
decision point is [`rights`](https://github.com/openabstractions/abstraction-rights);
`rights.Registration` — the service and the secret a person's approval gave the
application — is the `Holder` that asks it for `awake` and lets the service
keep the platform request on the application's behalf. What the service
records is the hold itself: `rights holds` shows the application, the right,
the lease owner and the job's kind and id as the reason, when, and the program
the kernel says asked; and the service's log carries the same line on `hold`,
`released` and `revoked`. `rights revoke` ends a live hold the same second.

Three answers, and each has one meaning:

- **A refusal holds nothing, and the caller is told why.** Not registered, not
  granted, an unidentifiable caller: `Held()` is false and `Why()` is what the
  service said, verbatim. The platform is not asked instead.
- **Nobody answering means the platform.** With no service at the endpoint the
  hold takes the platform's inhibitor itself, exactly as `KeepAwake` does, and
  `Why()` is nil. Absent means permitted here, because the measurement this
  section opens with was of work that slept; an absent service must not put it
  back to sleep. Absent is *nothing listened* and nothing else: every answer
  the service gives, including *refused*, is a decision.
- **Decided once, at the moment of asking.** A hold the service grants and
  later takes away — a person revoked the right, or the service stopped, and
  the client cannot tell which — ends, `Why()` is `ErrTakenAway`, and nothing
  falls back to the platform behind the person's back.

`KeepAwake(store, claimed)` is `KeepAwakeVia(Platform, …)`: it asks nobody,
and it is what every caller in this tree still does. One deliberate divergence
from the ancestor: polkit's subject is the calling process, so any library can
ask; `rights` designates an application by its secret, so a library cannot,
and the adopter brings the registration. The rules above are held by
`TestARefusalHoldsNothingAndSaysWhy`, `TestNobodyAnsweringMeansThePlatform`
and `TestAHoldTakenAwayEndsAndDoesNotFallBack` in `go/`, and against the
running service by `TestAJobAsksBeforeItHolds`,
`TestNoServiceMeansThePlatformHolds` and
`TestAStoppedServiceTakesItsHoldsWithIt` in `rights/go`; the conformance
driver has no policy service in its vocabulary, so no scenario file can reach
them, and the `awake` scenario's every `hold` is the absent case.

### 4. The issuer can ask for it back

```json
"lease": { "owner": "resident@lmstudio", "epoch": 2, "expires_at": "…",
           "recall": { "reason": "doubled: lemonade holds qwen3.6-35b-a3b",
                       "by": "resident-broker", "at": "…", "until": "…" } }
```

A Chubby lease expires; nothing recalls it. Both systems that genuinely
arbitrate a resource have that other half: **WDDM2** signals a process that
its video-memory budget changed, expects it to trim, and evicts it if it does
not; **Android** tells a process to trim and kills it if it does not. Without
it a lease is a promise to wait, and the residency broker could name a model
loaded twice and evict neither copy.

A recall is **the issuer's demand about the resource**, and it is not an
`intent`, which is the user's wish about the job: `want` is untouched by a
recall [JOB-R8]. It is addressed to one holding, so it presents the epoch the
issuer *read* — the fencing token pointed the other way — and is refused
`stale-epoch` if the record has moved [JOB-R1]. It needs a reason, a live lease
and an unfinished job: refused `invalid` without a reason, `lease-expired`
where nobody holds the lease, `terminal` on a finished record [JOB-R2].
`reason` is opaque here, chosen by the issuer for the holder's kind exactly as
`kind` scopes `spec`; `by` says who asked.

**The fallback is the lease lapsing.** A recall moves `expires_at` to `until`
where `until` is earlier, and the holder's next write after the deadline is
refused as any lapsed write is — that lapse is the eviction, and nothing else
is [JOB-R3]. A renew on a recalled lease never extends past `until` [JOB-R4],
and the holder cannot shed a recall by re-claiming while its lease is held
[JOB-R5]. Until then it may renew and checkpoint, because yielding takes time
and a holder that cannot record what it proved on the way out loses it. This
is WDDM's shape and not Android's: a lease cannot kill a process on another
machine, but it can stop believing in it, and everything that already refuses
a lapsed epoch enforces the recall for free.

A recalled job whose lease has lapsed or been released is an orphan, and **the
recall survives release and expiry** [JOB-R6] — so a later reader can tell
*nobody renewed* (no recall) from *asked and yielded* (recall, no owner) from
*asked and evicted* (recall, owner still named, lease lapsed). A claim starts a
new holding and carries no recall [JOB-R7]; the issuer recalls the new holder
if it still wants the resource, which is the policy loop WDDM runs too.

```bash
jobctl recall <id> --epoch N --reason "doubled: lemonade holds it" [--grace SECONDS] [--by who]
```

`--epoch` is the one the caller saw, not one it holds: a third party recalling
a residency it has only read. The residency broker in
[`model/`](https://github.com/openabstractions/abstraction-model) is the first issuer and the first holder.

---

## Reclaiming is the mechanism; handing off is only an optimisation

A process killed with `SIGKILL`, or a machine that loses power, never gets to
hand anything over. So the primary path is **adoption**: on start, look for jobs
whose lease has expired and claim them.

```python
for orphan in store.orphans():
    store.claim(orphan.id, "me", ttl_seconds=30)
```

**`claim` is `acquire`** — Azure Blob's lease verb and the Kubernetes Lease's,
beanstalkd's `reserve`, SQS' receive-under-a-visibility-timeout. **`orphans()`
is not Kubernetes' `orphan`**, which is a deliberately un-parented object
(`propagationPolicy: Orphan`); ours is Sidekiq Pro's `super_fetch` orphan check
— work whose owner died. `Adopt` above it is the controller's, taken from the
same place `ownerReferences` adoption is.

`release()` exists so a polite exit frees the job in seconds instead of after the
expiry — but nothing depends on it, which is exactly the point. A design that
*requires* graceful handoff has no answer for the case that actually loses your
40 GB.

---

## The record is the contract

The Go, Python and C++ implementations are not ports of each other and are not
generated from a shared schema. They agree about one thing: **the JSON file on
disk, and the rules for taking it over.** Each language's API looks like that
language.

The table is in the order the keys are written, because that order is part of
the agreement — see *How it is written* below.

| field | meaning |
|---|---|
| `content` | the data models this record carries, by name. Always present |
| `critical` | the subset of `content` a reader must understand or refuse the record |
| `id` | opaque, sortable by creation time, safe to pass around |
| `kind` | what this job is, and who can read `spec` and `checkpoint` |
| `envelope` | `schema`, `actions` — which schema the opaque halves follow and what may be asked of a job of this kind. Optional, written once |
| `state` | `pending` · `running` · `transferred` · `complete` · `failed` · `cancelled` |
| `spec` | the immutable description of the work [JOB-M1]. **Opaque here** |
| `checkpoint` | what a successor needs to resume. **Opaque here**. Omitted until something is proven |
| `progress` | `done`, `total`, `updated_at`, optional `step` — best-effort, decide nothing on it |
| `lease` | `owner`, `epoch`, `expires_at`, and `recall` — `reason`, `by`, `at`, `until` — while the issuer wants it back |
| `delegation` | `system`, `external_id`, `delivered` — set when an external system owns the work |
| `requires` | capabilities an implementation must have to take this job |
| `error` | the last failure, so a person can see why a job stopped |
| `intent` | `want`, `by`, `at` — what somebody WANTS, written without a lease |
| `extensions` | data this layer does not understand, keyed by a name that says who does |
| `created_at`, `updated_at` | UTC, exactly six fractional digits, trailing `Z` |

A field that is not in this table is **refused, not ignored** [JOB-F1] — at the top level
and inside `progress`, `progress.step`, `lease`, `lease.recall`, `delegation`,
`envelope` and `intent` alike. Continuing a job whose description you only partly understand is the risk
this whole mechanism exists to avoid, and a newer writer's addition is far more
likely than a typo. Anything a participant needs to carry that is not here goes
in `extensions`, under a namespaced key, and every reader must write back
untouched what it could not read [JOB-F2] — which is protobuf's unknown-field
preservation, and the role `annotations` plays in Kubernetes and OCI.

**Declared as a divergence, because refusing is the unusual half.** Nearly
everything an adopter has used ignores what it does not know: protobuf keeps
unknown fields and re-emits them, JSON Schema's `additionalProperties` defaults
to true, HTTP recipients ignore unrecognised headers, and Postel's advice is
the general form. We refuse, and the download layer one level up ignores
unknown spec keys on purpose — a record is a contract three languages share, a
spec is payload the layer above extends.

The measured cost is zero and the measured limit is the point. The published
`go/v0.1.0` was run against this tree over five format changes, the record
corpus, and 19 records from a real store — **nothing was refused at decode by
either version**, because the envelope never moved (the same sixteen top-level
fields throughout). The one change that *did* break an older reader was a rule,
not a field: a `v0.1.0` lease holder walks a `complete` record back to
`pending`. Strictness about fields cannot see a rule, which is what `critical`
below is for, and it is why refusing unknown fields is not the whole of the
mechanism.

**`error` is a bare string, and that is a divergence from
`google.rpc.Status{code, message, details}` (AIP-193) and Problem Details
(RFC 9457)** — from the same AIP family this record takes `spec` from. It is
undermeasured and it is stated here rather than defended: the *not now* against
*no* classification, the most load-bearing thing this layer decides, is
recoverable from a record only through `state`, and the reason is prose no
machine reads. The classification itself is specified —
[`download/CONTRACT.md`](https://github.com/openabstractions/abstraction-download/blob/main/CONTRACT.md#two-endings) — and no field carries
it.

### How it is written

The bytes are compared between implementations, so the encoding is fixed:
indented with two spaces, keys in the order above, one trailing newline, UTF-8,
LF [JOB-E1]. `content`, `id`, `kind`, `state`, `spec`, `progress.done`,
`progress.updated_at`, the whole `lease`, `created_at` and `updated_at` are
always present; every other field and sub-field is **omitted when it is empty**,
so a record with no intent has no `intent` key rather than a null one [JOB-E2].
A reader must treat absent and empty as the same thing [JOB-E3]. A writer must
not spell the same state two ways [JOB-E4].

**A string is escaped exactly here and nowhere else [JOB-E6]:** `"` and
`\\`; U+0008, U+0009, U+000A, U+000C and U+000D as `\b`, `\t`, `\n`,
`\f`, `\r`, and every other character below U+0020 as `\u00xx` in
lower-case hex; U+2028 and U+2029 as `\u2028` and `\u2029`. **Everything
else is raw UTF-8** — `&`, `<`, `>`, `/`, U+007F, every non-ASCII character, and
every character above U+FFFF as its four UTF-8 bytes rather than as a surrogate
pair. A byte that can neither begin nor continue a well-formed UTF-8 sequence
is replaced, one byte for one, by the escape `\ufffd`.

That is **RFC 8785's string serialisation** (JSON Canonicalization Scheme, which
takes it from ECMAScript `JSON.stringify`), with two declared divergences. RFC
8785 leaves U+2028 and U+2029 raw; we escape them, because they are the only
characters legal in a JSON string and illegal in a JavaScript string literal
before ES2019, and because adding an escape is exact where removing one is not —
a string holding the six characters `\u2028` is itself written `\\u2028`,
which a search for the escape matches one byte into. RFC 8785 keeps a lone
surrogate as `\ud800`; we replace it, and write the replacement as an escape
rather than as the U+FFFD glyph, so a reader can see that a byte was lost.

Both defaults we turned off were reasonable and neither was declared. Go escapes
`&`, `<` and `>` so a document is safe inside an HTML `<script>`; Python escapes
every non-ASCII character so a document survives a transport that cannot carry
high bytes. A record is neither of those things — it is a UTF-8 file a
supervisor reads — and until 2026-09-08 one ampersand in a download URL made the
three implementations write three different records.

This governs what a writer **spells**. It does not reach the bytes of an opaque
`spec` or `checkpoint`, and the next rule says what does.

**An implementation must reproduce the exact bytes of an opaque value it was
given [JOB-E7].** Every escape as it was spelled, every number as it was
written, every member in the order it arrived — inside `spec`, `checkpoint` and
every `extensions` value, all the way down. A writer that reads `1.50` and
writes `1.5`, or reads `"a\/b"` and writes `"a/b"`, has imposed its own policy on
somebody else's document, and the next reader downstream sees the changed one.
`scripts/verdict-conformance.sh` hands one record carrying all of it to all three
writers and compares what each carried against what it was handed.

**An opaque field contains one syntactically valid JSON value [JOB-E8]. The
reader validates its syntax and retains its original encoded bytes. It does not
normalise the value or validate its kind-specific schema.** Validation is of the
whole value including everything nested in it: correct string escapes,
delimiters, separators, literals and number grammar (RFC 8259 §§4–7); no
unescaped control characters, trailing commas, comments or truncation; valid
UTF-8; and the record's own size and depth limits. So `"a\qb"` is refused —
`\q` is not one of the eight escapes JSON has — and `"a\\qb"`, which is a
backslash followed by `q`, is accepted.

**Validate, retain, reproduce — not decode into ordinary objects and
reconstruct.** A validating scanner can record the value's byte span; a lossless
parser also works; bracket matching alone cannot, because it cannot see an
illegal escape. **No opaque number is converted into a host numeric type merely
to validate it**: syntactic validity does not require fitting a `double` or a
job counter, and a reader that converted `123456789012345678901234567890` to
check it would either lose it or refuse it.

Two interoperability questions ordinary JSON grammar leaves open, answered here
rather than left to whichever library each implementation reached for.

**Object member names MUST be unique within each object, including objects
nested in opaque values [JOB-E9]. Names are compared after JSON escape decoding,
case-sensitively, without Unicode normalization. Accepted opaque values retain
their original encoded bytes.** So `{"x":1,"\u0078":2}` is refused — both names
decode to `x` — while the same name in two *separate* objects is fine,
`{"a":1,"A":2}` is accepted because the comparison is case-sensitive, and
`{"\u00e9":1,"e\u0301":2}` is accepted because it is not normalizing.

**This is a rule of this wire profile, not a claim about JSON grammar.** RFC 8259
permits a repeated name; what it says is that a receiver's behaviour is
unpredictable, and unpredictable receiver behaviour is the whole damage. Two
readers handed identical bytes read different documents out of them, one keeping
the first name and one the last. Measured rather than argued:
`abstraction-download` accepted a record whose `spec` names `sources` twice and
downloaded from the second source, so a first-wins reader in another language
downloads a different artifact from the same record. [JOB-E7] keeps the
bytes intact in transit and can do nothing about disagreement at the moment they
are interpreted.

The precedent is I-JSON, **RFC 7493 §2.3** (Bray, Standards Track, March 2015):

> Objects in I-JSON messages MUST NOT have members with duplicate names. In this
> context, "duplicate" means that the names, after processing any escaped
> characters, are identical sequences of Unicode characters.

**We adopt that restriction. We do not claim conformance to the I-JSON profile**,
which also constrains numbers, top-level values and time formats on terms this
format settles for itself.

**Decoding a name in order to compare it does not license rewriting the payload
that was retained.** The comparison is over the decoded name; what is written
back out is what arrived, escape for escape ([JOB-E7]). An implementation that
decodes to compare and then re-encodes from its own parse satisfies [JOB-E9] and
breaks [JOB-E7], and `TestOpaqueBytesSurviveNameComparison` in `job/go` is the
test that separates the two.

[JOB-E9] is stated here in full and not left to be inherited from
`duplicate_keys = "refuse"` in `job.thrift`. That setting is an encoding
instruction: it says what the generated readers do, and it stands. A rule that
exists only as an inheritance from a setting is not a rule an implementer can
read, and this one binds every implementation, generated or not.

**An unpaired surrogate escape inside an opaque value is refused**, exactly as it
is elsewhere in the record — outside a payload [JOB-E6] replaces one with
`�` on write, and inside one a reader may not rewrite what it carries, so
refusing is the only answer that does not invent a second policy. **Every
record-wide restriction applies inside an opaque value on the same terms, and no
implementation relaxes one there.**

If arbitrary non-JSON bytes must be carried they go in a JSON string or in an
artifact the record points at. A raw opaque value is not a route for embedding
malformed JSON inside an otherwise valid record: the record is a UTF-8 file a
supervisor reads, and one bad escape at any depth makes the whole file
unreadable to every parser that is not ours.

Two things [JOB-E7] does **not** promise, both because they are somebody's rule
rather than somebody's data. The payload's own **whitespace** is not carried: the
record is canonical by construction under [JOB-E1], which fixes the indent for
the whole file, so a payload written on one line and a payload written on six are
the same document and are written the same way. And **U+2028 and U+2029** are
escaped inside a payload as everywhere else, because the reason above is about a
document being read by a JavaScript parser and does not stop at a field boundary.

~~carrying an opaque payload byte for byte is not implementable in a language
whose only representation of JSON is a parsed value~~
2026-09-08: false, and it had stopped three readers from looking. The sentence
was true and its premise was not — a parsed value is not the only representation
any of the three has. Go's stdlib does it, Python's `raw_decode` returns an end
index you can slice at, and .NET's `JsonElement.GetRawText()` proves a *tree* can
do it, because a document that indexes offsets over the original buffer is still
a tree. What separates the readers is not tree against token; it is whether the
reader can still address a value's byte range in the input. `go/opaque_test.go`
is where that is held: `TestOpaqueBytesSurvive` submits a spec spelling every
scalar a way this package would not choose — a redundant solidus escape in a
value and in a key, `1.50`, `1e2`, `-0.0`, an integer past float64, members in
an order nothing sorts them into — and fails if any of it comes back re-spelled.
All three now carry the bytes: Go held every
token as it received it all along, the C++ reader had already kept a **number**
as the literal text it arrived as — so `1.50`, `1e2`, `-0.0` and an integer past
float64 survived it before this rule existed, and the page never said so — and
what each of the other two had to learn was a **string's** spelling.

~~Go carries those as it received them~~
2026-09-08: Go compacts them — the function is called `marshalCompact` — and then
the record encoder re-indents whatever it is handed. No implementation preserves
a payload's whitespace and none ever did, which is why [JOB-E1] and not
[JOB-E7] is the rule that governs it. Go preserved *more* than the other two,
never everything.

### What a record declares, and the name of every model

There is **no `schema` field**. There was — an integer, bumped five times — and
the version conflated *what changed* with *what you must understand*, so the only
safe response to an unknown number was to refuse a whole record over an addition
the reader could have ignored. It is replaced by two lists:

```json
"content":  ["abstraction.job/base@1", "abstraction.job/intent@1"],
"critical": ["abstraction.job/base@1", "abstraction.job/intent@1"]
```

`content` names every data model in this record — a **profile declaration**, in
the sense of JSON-LD's `@context` and RFC 6906's `profile`: the document says
which vocabularies it is written in, and a reader looks them up rather than
guessing from the keys. `critical` is the subset a reader MUST understand: an
unknown name **there** means refuse the record, an unknown name only in
`content` means carry it and act on the rest [JOB-D1].
X.509 settled the *fallback* decades ago with critical certificate extensions
(RFC 5280 §4.2: reject on an unrecognised critical extension, "MAY be ignored"
otherwise), and that half is taken straight from it. It did not settle the
*unit*: an X.509 extension is always a value, so criticality there only says
what to do when you cannot parse one. The mechanism that names a rule is JSON
Schema's `$vocabulary` (2020-12 §8.1.2) — a required vocabulary is a set of
keywords with semantics, and an implementation that does not recognise one
"MUST refuse to process any schemas that declare this meta-schema". That is the
ancestor of `abstraction.job/terminal@1`, not X.509.

**The shape is JOSE's `crit`** (RFC 7515 §4.1.11): a list naming things carried
elsewhere in the same header that a recipient must understand or reject the
whole object. Two parallel lists, exactly like ours.

**What happens when the two lists disagree is COSE's answer, not JOSE's, and the
binding citation is RFC 9052 §3.1.** Both were read against the primary
documents on 2026-09-08 because this page had it the other way round. RFC 7515
§4.1.11 puts the obligation on the writer — *"Producers MUST NOT include Header
Parameter names … that do not occur as Header Parameter names within the JOSE
Header in the `crit` list"* — and gives a recipient only a **MAY**: *"Recipients
MAY consider the JWS to be invalid …"*. RFC 9052 §3.1 is the one that binds a
reader: *"If the `crit` value list includes a label for which the header
parameter is not in the protected-header-parameters bucket, this is a fatal
error in processing the message."* We refuse, so COSE is the ancestor we
implement and JOSE is the shape we borrowed.

So **`critical` is a subset of `content`** [JOB-D8]: a name marked
critical and not carried is a declaration about nothing, and all three
implementations already drop such a name on write rather than let the two lists
drift — it was enforced in code and stated on no page. A reader may take
`content` as the whole roster and `critical` as a marking on it.

**The two rules that read `critical` are applied in one order [JOB-D10]:** a
name the table marks *never* is removed from the list first, then what is left
is checked for a name the reader does not know ([JOB-D1], refuse) and for a name
absent from `content` ([JOB-D8], refuse). Stripping first is what makes
[JOB-C2] true against [JOB-D8]: a writer that wrongly marked an advisory model
critical cannot stop a reader that would otherwise have done the work, and the
marking is gone from what that reader writes back. **It is not a refusal, and it
is not permission to ignore criticality** — a name outside the table still
refuses the whole record. A prohibited marking is a diagnostic for whoever
wrote it, which is where every ancestor puts it: RFC 5280 §4.2 says *"Conforming
CAs MUST mark this extension as non-critical"* and tells no reader to reject a
certificate over a CA that did not.

The names are normative, and this is the whole list:

| name | covers | in `critical` |
|---|---|---|
| `abstraction.job/base@1` | `id`, `kind`, `state`, `spec`, `checkpoint`, `progress`, `lease` | always |
| `abstraction.job/intent@1` | `intent` | whenever present |
| `abstraction.job/delegation@1` | `delegation` | whenever present |
| `abstraction.job/step@1` | `progress.step` | never — advisory, and a reader that ignores it is correct about everything that matters |
| `abstraction.download/ranges@1` | `checkpoint.verified` — which bytes are proven, as against how many leading ones [JOB-C2] | never — a marking on it is stripped, not refused [JOB-D10] |
| `abstraction.job/terminal@1` | the rule that a terminal record refuses its own holder's update, release and intent | whenever `state` is terminal |
| `abstraction.job/recall@1` | `lease.recall`, and the rules that a renew stops at `until`, that the holder cannot re-claim, and that the lapse is the eviction [JOB-D9] | whenever `lease.recall` is present |
| `abstraction.job/envelope@1` | `envelope`, and the rule that it is written once and never moved by a lease holder [JOB-V5] | whenever `envelope` is present |
| an extension key | that extension's value | only if its writer says so |

`abstraction.job/terminal@1` names a **rule**, not a field [JOB-D6] — and it is
not alone: the row below it, `abstraction.job/recall@1`, names a field and three
rules about writes at once, and `abstraction.job/base@1` names seven fields.
~~and it is the only one here that does~~ 2026-09-08: false in its own table,
one row down, since the day the table was written. **The unit of `critical` here
is a feature — a named bundle of obligations, some fields, some rules — and that
is what distinguishes it from every ancestor**, four of which mark only a value
carried in the document. Naming a rule is deliberate and it is the mechanism's
missing half: terminal enforcement was added to this format under an unchanged
`abstraction.job/base@1`, so a reader published before it walks a `complete`
record back to `pending` — measured, not argued: a `go/v0.1.0` lease holder did
exactly that, and the `terminal` conformance scenario is what refuses it now.
A shape can be ignored; a rule about writes cannot, because the reader that does
not know it is precisely the reader that breaks it. So a change to what a record
*means* is declarable here on the same terms as a change to what it carries.

Both lists are **derived from what the record actually carries** on every write,
never remembered [JOB-D2], so a declaration cannot drift from the data. A record
that gained an intent since it was last written says so on the next write without
anybody updating a list. Extension keys are appended to `content`
sorted [JOB-D3], because this output is compared byte for byte between
implementations.

The integer is still **read**, because stores full of version 3 and 4 records
exist on real disks and on a NAS. It is never written again [JOB-D4]. The mapping
is exact rather than a guess — those versions are frozen and it is known what each
one could contain:

| `schema` | means `content` of | plus `delegation@1` if the record has one |
|---|---|---|
| 3 | `base@1` | yes |
| 4 | `base@1`, `intent@1` | yes |
| 5 | `base@1`, `intent@1`, `step@1` | yes |

Everything else — 1, 2, 6, 99 — is refused [JOB-D5]. `step@1` is the one model
the mapping does not mark critical.

### `envelope` — which schema, and what may be asked

```json
"envelope": {
  "schema": "nas.example/transfer@2",
  "actions": ["cancel", "nas.example/transfer@2#re-mirror"]
}
```

`kind` says WHO may read `spec` and `checkpoint`. It does not say WHAT they are
and it is not versioned, so a supervisor that did not create a job could do
exactly two things with it: find it orphaned and reclaim it. This is the missing
half, and it is what lets a supervisor written by somebody else act on a kind it
was never built for — without ever opening the opaque halves.

**Ancestor.** Protocol Buffers' `Any` and its `type_url`, and AIP-151's opaque
`metadata`, which this record's `spec` already copies. We took the opaque half
and not the self-describing half, and this is the half. The divergence is
declared and it is the whole safety argument: `Any`'s `type_url` is a URL by
construction, and ours is a name that cannot be one. **`actions` has no ancestor
in that family at all** — the nearest is D-Bus, where a method is an
(interface, member) pair and the interface is reverse-DNS, and Kubernetes RBAC,
where a bare verb is scoped by a separately named `apiGroup`. Neither carries
the *declaration* on the object itself.

**[JOB-V1] A schema identifier is a name, and nothing dereferences it.** It
matches `namespace "/" name "@" version`, where a namespace is dot-separated
labels, a label is `[a-z0-9]` with interior `-`, a version is `[1-9][0-9]*`, and
the whole is at most 128 bytes — the same spelling as a content name, so this
record has one grammar for names and not two. There is no scheme, no authority,
no percent-escape, no backslash, no query, and exactly one `/` and one `@`, so
`https://example.invalid/x@1`, `\\host\share`, `../../etc` and `file:///x` are
not discouraged values but illegal ones, refused with `invalid` before any
supervisor sees them. **No implementation offers a function from a schema
identifier to a schema**, in any binding. A supervisor that fetched an address
out of somebody else's record would be running attacker-chosen content as a
service on the owner's machine, and the reason this rule is a grammar rather
than a warning is that a warning is not checkable.

**[JOB-V2] An action name is a name a supervisor matches, and never anything
executable.** It is either `bare` — `[a-z0-9]` with interior `-` and `_`, at
most 64 bytes — or `<schema>#<bare>`. Nothing in the record can say *how* to do
anything, and no implementation performs an action; matching is the only
operation defined on the field. The separator is `#` because it is the one
component of a URI reference defined never to be sent anywhere (RFC 3986 §3.5):
a fragment names something inside a document rather than a document to fetch.

**[JOB-V3] Every action name resolves to a schema the record declares.** A bare
name belongs to `abstraction.job/base@1` and MUST be one of its four words —
`pause`, `resume`, `cancel`, `recall`, each naming a store operation the kind
declares it honours. A qualified name's schema part MUST equal this record's own
`envelope.schema`. Spelling a base action the long way
(`abstraction.job/base@1#pause`) is refused, so one action has one spelling and
`supports` is a comparison rather than a negotiation. Anything else is `invalid`.
Two vendors will otherwise both define `cancel`, and a supervisor matching on the
bare name alone would perform one of them believing it had performed the other.
An extension that wants actions of its own needs its own envelope; that field
does not exist yet and the rule above is what keeps room for it.

**[JOB-V4] A supported action is a property of the kind, not of the instance.**
The envelope is written at submit and never moves, and **every binding refuses a
write that moves it, with `invalid`**. The in-process bindings compare the
envelope before and after the caller's own closure; the service binding compares
the record it holds against the one the message carries. The service binding
would have got the rule for free by leaving the envelope out of the fields a
write may carry — and that would have made it *silently ignore* what the other
two *refuse*, which is the same application on two bindings behaving
differently. This is also why the struct has two fields and no third: anything
reporting what is true *now* would make a supervisor try to pause a job whose
worker died an hour ago.

**[JOB-V5] A record carrying an envelope declares
`abstraction.job/envelope@1` and marks it critical.** A reader that ignored the
envelope would carry on and then write the record back without it, deleting the
kind's own declaration — and the loss would be invisible, because the thing
destroyed is the description.

**[JOB-V6] A supervisor asks; it never resolves.** The answer to *may I do this*
is computed from the record's declaration and from the schemas and actions the
asking process already implements, which it passes in. Three refusals, and they
are three different questions: `unknown_schema` — the record follows a schema
this supervisor was not written against, so leave the job alone;
`not_supported` — the kind does not declare this action, or this supervisor has
not built it; `invalid` — the name is not an action name and somebody should be
told. Every refusal names what was refused, because a supervisor that declines in
silence is indistinguishable from one that quietly did the wrong thing.

**Backward compatibility, precisely.** A record written before this field existed
loads unchanged and re-encodes byte for byte: `envelope` is optional and omitted
when absent, so nothing that does not use it moved. A record that DOES carry an
envelope is **refused by a reader too old to know the field**, because every
struct in this format refuses unknown fields [JOB-F1]. That refusal is the
feature: an old supervisor that ignored the field would rewrite the record
without it, and [JOB-V5] is the same argument said from the other side. The cost
is that adopting the envelope for a kind is a flag day for that kind's readers,
and it is paid per kind rather than per store.

### `intent` — what somebody wants, written without a lease

```json
"intent": { "want": "pause", "by": "comfyui@desktop:9184", "at": "2026-09-05T09:12:44.180000Z" }
```

**This is the `spec` half of Kubernetes' `spec`/`status` split, and
`deletionTimestamp` in particular**: a field anyone with write access may set,
that the party doing the work converges on in its own time. Argo spells the same
thing `spec.suspend` and `spec.shutdown`. The three values are BITS' Resume,
Suspend and Cancel. Ours is a separate field rather than the record's `spec`
because `spec` here is opaque and immutable [JOB-M1].

`want` is exactly one of `run`, `pause`, `cancel`; anything else is refused
rather than treated as `run` [JOB-I1]. **Absent means `run`** [JOB-I2], so a
reader must not distinguish "nobody asked" from "somebody asked for it to run" — that is what
keeps version 3 records, which have no intent at all, readable.

This is the **one write that presents no epoch** [JOB-I3], and the exemption is the whole
point: the party who wants a job stopped is not the process doing it, and
requiring a lease would mean stealing the job in order to stop it — the single
thing the lease exists to prevent. It is idempotent [JOB-I4], and refused once
`state` is terminal [JOB-I5], because nothing reopens finished work. Asking for
something the current owner cannot do is **not** an error here [JOB-I6]: only the
owner knows what it can do.

`run` **records a value; it does not delete the field** [JOB-I7]. Resuming leaves
`{"want":"run", "by":…, "at":…}` behind, the record keeps declaring
`abstraction.job/intent@1` in `content` and `critical` from then on, and a reader
too old to know that model correctly refuses the record rather than working on a
job somebody may have asked to stop.

An owner MUST check the intent at least as often as it checkpoints **and before
it starts**, and move toward it [JOB-I8]. `cancel` must be honoured by
everything — stopping is universal [JOB-I9]. An implementation that cannot honour
`pause` must FAIL the job with a reason rather than carry on [JOB-I10], because a
pause that quietly does nothing is worse than no pause button.

And **a paused job is not an orphan** [JOB-I11]: a sweep that adopted it would
restart the work seconds after a person stopped it. **Unless its `state` is still
`running`** [JOB-I12].
An owner that honours a pause releases the lease, and releasing turns `running`
back into `pending` [JOB-L6] — so a record left `running` with nobody holding it is an
owner that died between the pause being asked for and the pause being carried
out. That one IS abandoned, and excluding it makes the state permanent: nothing
may claim the job, so nothing may pause, resume or cancel it ever again.

The check-before-starting and the sweep exception are **one rule in two places**
and an implementation needs both. Only sweeping restarts a download seconds after
a person stopped it; only checking leaves the record unreachable forever.

On the command line, in every implementation:

```bash
jobctl intent <id> <run|pause|cancel> [--by who]
```

No `--epoch`, and that absence is the feature.

### Who moves the state

`jobctl` and the store together produce exactly two of the six states:
**`pending`** on submit, and **`running`** on a successful claim [JOB-S1].
Nothing else in this layer ever sets one.

`transferred`, `complete` and `failed` are written by **the lease holder**,
through the ordinary epoch-checked update [JOB-S2] — `jobctl finish --epoch N
--state transferred|complete|failed` is that write, and nothing else. This layer cannot
decide them, because deciding them means knowing whether the work is done, and
`spec` and `checkpoint` are opaque here. Only the kind above knows.

`cancelled` is written by the owner honouring `intent: cancel` [JOB-S3]. Where
nobody holds the lease, the caller takes it for a moment and writes the state
itself [JOB-S4], so
a cancelled job does not sit visibly running until some supervisor notices —
Go's `job.Open(store, id, me).Cancel()` does exactly that, and the intent alone
is what reaches an owner on another machine.

`complete`, `failed` and `cancelled` are terminal: no claim, no update, no
intent [JOB-T1]. `transferred` is deliberately **not** terminal, so the requester
can still claim it to take delivery [JOB-T2].

**Terminal is judged on the record as read, not on what the write leaves
behind** [JOB-T3], and that is the only reason a job can ever finish: the update that
*makes* a record terminal is an ordinary epoch-checked write onto a record that
is not yet terminal, and it lands. The write after it is refused. So **an owner
puts everything it wants recorded into the same update as the final
state** [JOB-T4] — the error text, the last checkpoint, the final `done`. Nothing
of that epoch writes again, and an implementation that checkpoints *after*
finishing has the ordering wrong rather than a store that is too strict.
`release` is an update and is refused too [JOB-T5]: a finished job has nothing to
hand over, and the lease lapses on its own.

`renew` is the one write a terminal record still accepts [JOB-T6], because it is the one
write that changes nothing a reader may branch on: only the current holder's own
expiry, on a record nobody may claim, update or intent regardless. Refusing it
would give a lease keeper racing its owner's final write a failure to interpret,
for no observable difference. Said here because the list above is otherwise a
list of three, and a second implementer reading it has to guess.

**`delegation` is the architecture, not an accommodation for one tool.** When an
app's worker hands off to a system service, or that service hands off to a NAS,
the handle that finds the work again is `{system, external_id}` — for Windows
BITS, a job GUID that survives a reboot. It is Camunda's external task and CSI's
external provisioner, and the field is the external id that Stripe and
Salesforce mean by the phrase. When it is set, `progress` is a *cache*
of what the external system last reported; the external system is the
truth [JOB-G1].

**`transferred` is not bureaucracy.** It means the work is finished and proven,
but the result has not been taken delivery of. BITS has the same two-phase shape
for the same reason, and will not hand over a file until you call `Complete()`.
Collapse the two states and you cannot express *"the service finished this while
ComfyUI was closed"*, which is the case this is built for.

**The state names are BITS' `BG_JOB_STATE_*`, with one exception, and the
exception is a trap.** `pending` is `QUEUED`, `running` is `TRANSFERRING`,
`transferred` is `TRANSFERRED`, `failed` is `ERROR`, `cancelled` is `CANCELLED`.
**`complete` is not `Complete()`** — BITS calls this state `ACKNOWLEDGED`, and
the state an English reader would call "complete" is the one before it. So a
reader who maps `complete` onto "the transfer finished" is a state early, and a
supervisor written that way stops before delivery is taken. It is an
acknowledgement in the sense AMQP's `basic.ack` and SQS' `DeleteMessage` are;
Kubernetes and Azure both spell it `Succeeded`, and Celery `SUCCESS`.

---

## What is proven, and what is not

```bash
bash scripts/xlang-job.sh
```

Or, across every implementation that exists:

```bash
bash scripts/conformance.sh          # add JOBCTL_CPP=... for a C++ one
```

**Proven** ([`docs/results/XLANG2.txt`](https://github.com/openabstractions/abstractions/blob/main/docs/results/XLANG2.txt)) — a job is
created in Go with a spec **neither tool understands**, worked on in Go, abandoned
without release, found as an orphan by **Python**, adopted at epoch 2, resumed
from the checkpoint its predecessor proved, finished in Python, and read back in
Go. The stale Go owner is refused when it returns with its old epoch.

30 tests across the two implementations, including the zombie-owner and
expired-lease cases.

**And one thing only the cross-language test could find**
([`docs/results/CONFORM1.txt`](https://github.com/openabstractions/abstractions/blob/main/docs/results/CONFORM1.txt)). Go encodes
`time.Time` as RFC3339**Nano**, which trims trailing zeros, so Go wrote
`…T06:23:11.22275Z` where Python wrote `…T06:23:11.222750Z` for the same instant.
Both are valid RFC 3339, both parse, and every unit test in both languages
passed — but the record changed bytes every time the two took turns, so a diff
of a job's history meant nothing. The timestamp format is now pinned to exactly
six fractional digits and a `Z` [JOB-E5]; six, not nine, because Python's datetime holds
microseconds and the contract is set by the least precise participant.

**Not proven yet.** No bytes move in that test, deliberately — resume over real
bytes is tested in [`download/`](https://github.com/openabstractions/abstraction-download). What nothing has tested
is a kill during a real multi-gigabyte transfer, which needs the service tier.

### The transcript, exactly

```bash
bash scripts/behaviour-conformance.sh   # add REPLAY_CPP=... for a C++ one
```

`conformance.sh` passes one record round a relay and proves the implementations
can continue each other's work. It cannot prove they would each have done the
same thing alone: whoever reaches a branch second inherits the first one's
answer. So `behaviour-conformance.sh` gives every implementation the same
scripted operations and its own store, and compares the transcripts byte for
byte. A scenario lives in `download/testdata/scenarios/`, one operation per
line, and a driver named `replay` runs it.

Each line comes back as `NN <the operation> -> <verdict> <fields>`.

The verdict is `ok`, or one of `not-found`, `lease-held`, `stale-epoch`,
`lease-expired`, `terminal`, `unknown-model`, `invalid`, `refused`. **Which
class of refusal happened is the contract; its wording is not** — pinning
wording would make every improved message a cross-language breakage.

Each has a name outside: `stale-epoch` is gRPC `ABORTED`, whose own gloss is a
sequencer check failure, and HTTP's `412 Precondition Failed`; `lease-held` is
`409 Conflict` and Azure's `LeaseAlreadyPresent`; `lease-expired` is Azure's
`LeaseIdMismatch`; `terminal` is `FAILED_PRECONDITION`; `invalid` is
`INVALID_ARGUMENT` and `400`; `not-found` is `NOT_FOUND`.

`unknown-model` is its own class and not a shade of `invalid` [JOB-D7]. They ask
for opposite things: `invalid` says the record is wrong and a caller may
discard it, `unknown-model` says the record is fine and **this reader is too
old**, so the only correct response is to leave it alone for something newer.
Collapsing them is how a critical name gets acted on as corruption. Go called
it `refused` and the other two called it `invalid` for as long as the mechanism
existed, because no scenario could reach it.

The fields are always these eleven, in this order:

| field | meaning |
|---|---|
| `state` | `pending`, `running`, `transferred`, `complete`, `failed`, `cancelled` |
| `epoch` | the lease generation, which only rises |
| `held` | `yes` while a live lease exists, `no` otherwise |
| `recall` | the reason the issuer gave, or `none` — what a holder branches on |
| `want` | `run`, `pause` or `cancel` — what somebody asked for |
| `done` | progress in the kind's own units |
| `err` | `set` if an error was recorded, `none` otherwise |
| `cp` | the checkpoint, compact JSON, or `none` |
| `content` | what the record declares it carries, comma separated |
| `crit` | the subset of it a reader must understand, comma separated |
| `awake` | `yes` while this driver holds the machine awake for the record's lease |

They are printed after a refused operation too: **what a refusal leaves behind
is the half of it a caller has to live with.**

Nothing else is compared. Not timestamps, ids or owner strings — three
implementations cannot agree on a clock and nothing branches on the rest. Not
mid-transfer progress, which is advisory, and pinning it would make a buffer
size a contract. Not concurrent writers, which are non-deterministic by
construction, so no transcript of them could be compared at all; that property
belongs in each language's own tests.

A driver answers `--capabilities` with `store`, or `store transfer` if the
language also has a download runner. The roster is fixed at three, and a
language that cannot run a scenario is **printed as a gap and counted**, never
skipped: a gap that reads as a pass is the failure this instrument exists to
remove.

A driver also answers `--models` with the content-set names it can read, one per
line, each marked `critical-ok` or `never-critical`. The harness diffs
the three rosters against each other and against the table above, so *which
names exist and which may be critical* is a gate answer rather than a claim on a
page. Three implementations that do not know the same names are not three
implementations of one contract, whatever the transcripts say.

The operations a scenario may write are `submit`, `claim`, `renew`, `progress`,
`hold`, `release`, `finish`, `intent`, `recall`, `orphans`, `state`, `run`,
`stage`, `plant` and `sleep`. `hold <alias>` keeps the machine awake for the
lease the record carries, as a runner does at claim. `recall <alias> <owner> <grace-ms> [reason…]` is issued
against the epoch that owner holds, which is what an issuer would have read.

`plant <alias> content|critical <name>` writes a name straight into the
record's declaration on disk, which is the one thing a conforming writer cannot
do: an implementation refuses to write a record it could not read back, so the
only way to reach its own refusal path is to forge what a newer writer would
have written. Without it the mechanism has no scenario, and it had
none for as long as it existed.
An operation no driver implements is an operation whose divergences no scenario
can ever see, so the list is here rather than only in three sources: `renew` was
missing from all three for as long as they existed, and the only thing that
found it was a person reading five stores by hand.

### Every invariant carries a name

Every rule on this page and on
[`download/CONTRACT.md`](https://github.com/openabstractions/abstraction-download/blob/main/CONTRACT.md) ends with a tag in square
brackets, and a scenario cites that tag on the `# expect` line testing it:

```
# expect 6: terminal [JOB-T1]
```

A tag inside a fenced block, like the one above, is an example and declares
nothing.

`behaviour-conformance.sh` reads the tags off both pages, reads the tags the
scenarios cite, and prints the difference. **A rule nothing cites is printed and
counted as UNEXERCISED**, which is the one thing a harness comparing three
transcripts can never notice: silence about a rule reads exactly like agreement
about it. The tag is an accounting device and never a source of truth — nothing
is generated from it, because three implementations generated from one file
would agree by construction and prove nothing.

A tag marks a rule about what an implementation must **do** with a record. The
transcript format above is the instrument, not the subject, and carries none.

This is a **requirements traceability matrix** and it is worth calling one:
DO-178C's bidirectional trace between a requirement and the test that exercises
it, IEEE 830's numbered shall-statements, the QUIC interop matrix. The
UNEXERCISED count is the untraced half of it, which is the half that instrument
exists to print. Nameless, it reads as a house eccentricity.

---

## Prior art this was taken from

Nothing here is invented where something already worked:

- **Windows BITS** — persistent jobs with a GUID any process can open, ownership
  transfer, resume across reboot. The yardstick, and on Windows probably the
  implementation to wrap rather than replace.
- **Google `longrunning.Operation`** — the standard shape for polling a handle
  you did not create, and the opaque-`metadata` idea this record's `spec` copies.
- **iOS background `URLSession`, Android `WorkManager`** — hand work to an OS
  service, get killed, re-attach by identifier. Shipped in 2013; what is missing
  is a portable, cross-language version.
- **Temporal activity heartbeats** — one channel carrying liveness, progress and
  the resume checkpoint.
- **rsync `--append-verify`** — never append to a prefix you have not proven.
- **Chubby** — the lease, and the sequencer this record calls an epoch.
- **JOSE `crit` (RFC 7515) and JSON Schema `$vocabulary`** — a list of names a
  reader must understand or refuse the whole document, for a value and for a
  rule respectively.
- **tus and BitTorrent BEP 3** — a committed offset, and a bitfield of proven
  pieces.

**Every concept on this page names its ancestor or says it has none.** A name
with no ancestor is either something nobody has built, which needs defending, or
a rename by accident, which is the commoner case: of 61 concepts surveyed across
this page and `download/README.md`, exactly one had no prior art and six that
looked novel turned out to be badly named. Where our name and the established
one differ, this page says so, because the alternative is an adopter learning us
before they can use us.

Full surveys, with sources: [`openabstractions/research`, `async/`](https://github.com/openabstractions/research/blob/main/async/) and
[`transfer/`](https://github.com/openabstractions/research/blob/main/transfer/).

---

## Where a store comes from

Nothing above this line says where a root is; every rule on this page governs
what is inside one. That silence shipped: an installer put `dl` and `jobctl` on
one machine's `PATH`, `dl` found the store and `jobctl` refused for want of an
environment variable nothing sets, and the first install ever performed died
there.

**A store root is discovered, not demanded.** A tool handed no root resolves one,
and never refuses for want of being told:

1. its own override, if it has one — `JOB_STORE` for `jobctl`, `MODELGET_STORE`
   for `jobd`. That rung exists for a harness pointing several implementations at
   one directory, and for a container;
2. `ABSTRACTION_STORE`;
3. `store` in the per-user `abstraction/config.json`, at the location the OS
   designates — `%APPDATA%` on Windows, `~/Library/Application Support` on macOS,
   `$XDG_CONFIG_HOME` or `~/.config` elsewhere;
4. `<home>/.abstraction`, whether or not it exists yet.

**A tool that cannot open the root it resolved names that root and says which
rung produced it.** The failure that removes is a person who cannot find out
which directory a tool was looking at, and so cannot find the file to edit.

This carries no invariant tag, because no sequence of store calls can observe it:
every harness on this page is handed a root. Its test is the `verify` job of the
release workflow in `openabstractions/service-jobd`, which installs the package,
fetches one file with `dl`, and requires `jobctl list` to name the record `dl`
just wrote — once with the store redirected, then again with nothing set at all.

Discovery is the subject of
[`abstraction-config`](https://github.com/openabstractions/abstraction-config),
and `job` reimplements the order above in each language rather than depending on
it. That duplication is a known cost, not a design, and the copies already
differ: `config.JobStore` also reads a machine-wide file and an older
`~/.modelget`, and no `jobctl` does.

---

## Layout

```
job/
  go/         Go implementation      go test ./...
    cmd/jobctl/   command-line driver
  python/     Python implementation  python -m unittest
    jobctl.py     the same driver
```

Standard library only, both sides. An abstraction that needs a dependency to read
a JSON file has misjudged its own weight.
