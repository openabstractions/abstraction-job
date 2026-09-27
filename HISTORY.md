# History: abstraction.job/acceptance@1, operations@1, inventory@1, operator@1 and the job record

Linked from [CONTRACT.md](CONTRACT.md)'s "Reading this page."

## Superseded ids

`JOB-E1`..`JOB-E9`, the rules for how a record is written, are retired as ids
on 2026-09-24 and continue as `JOB-J1`..`JOB-J9` (JSON encoding), number for
number. `E` is reserved for error and outcome in every contract
(`research/vocabulary/DECISION.md` S12), and `B`, the next free letter the
rename plan's job row names, was already `SPEC.md`'s backoff letter.

| retired | now | rule |
| --- | --- | --- |
| `JOB-E1` | JOB-J1 | the encoding is fixed |
| `JOB-E2` | JOB-J2 | an empty field is omitted |
| `JOB-E3` | JOB-J3 | absent and empty are the same |
| `JOB-E4` | JOB-J4 | one state has one spelling |
| `JOB-E5` | JOB-J5 | a timestamp carries exactly six fractional digits |
| `JOB-E6` | JOB-J6 | a string is escaped exactly here and nowhere else |
| `JOB-E7` | JOB-J7 | the exact bytes of an opaque value are reproduced |
| `JOB-E8` | JOB-J8 | an opaque field contains one syntactically valid JSON value |
| `JOB-E9` | JOB-J9 | object member names are unique within each object |
| `JOB-X1` | JOB-I13 | `SPEC.md` § 4.3: cancelling never stops an execution |
| `JOB-X2` | JOB-G2 | `SPEC.md` § 4.5: delegated work is stopped by whoever reconciles it |
| `JOB-Q1` | JOB-E10 | `SPEC.md` § 6.1: the verdict classes map to obligations |
| `JOB-Q2` | JOB-E11 | `SPEC.md` § 6.2: a transcript cannot express unknown |
| `JOB-Q3` | JOB-E12 | `SPEC.md` § 6.4: a partial answer |

`JOB-E1`..`JOB-E9` are never reused. The reserved letter continues at
`JOB-E10`.

[SPEC.md](SPEC.md) minted fifteen ids of its own. Ten repeated one of
`CONTRACT.md`'s ids with a different rule, two used the reserved `X`, and
three stated outcomes under a letter other than the reserved `E`. Every
citation outside `SPEC.md` meant `CONTRACT.md`'s rule, and `SPEC.md`'s ids
move:

| `SPEC.md` § | was | now | why |
| --- | --- | --- | --- |
| 2.9 | `JOB-M1` | JOB-S5 | repeated `CONTRACT.md`'s JOB-M1; a state rule |
| 2.10 | `JOB-M2` | JOB-S6 | repeated `CONTRACT.md`'s JOB-M2; a state rule |
| 4.3 | `JOB-X1` | JOB-I13 | `X` is reserved for extension; an intent rule |
| 4.5 | `JOB-X2` | JOB-G2 | `X` is reserved for extension; a delegation rule |
| 5.1–5.4, 5.6, 5.7 | `JOB-A1`..`JOB-A6` | JOB-U1..JOB-U6 | repeated `CONTRACT.md`'s admission ids; a lost answer |
| 6.1, 6.2, 6.4 | `JOB-Q1`..`JOB-Q3` | JOB-E10..JOB-E12 | outcome rules, the reserved `E` |
| 9.1 | `JOB-V1` | JOB-D11 | repeated `CONTRACT.md`'s envelope JOB-V1; version skew |
| 9.4 | `JOB-V2` | JOB-D12 | repeated `CONTRACT.md`'s envelope JOB-V2; version skew |

Every other id keeps its letter and number. Each declaration moved from an
inline `[JOB-L2]` at whichever sentence end it fell to `**[JOB-L2] Title.**`
at the head of the rule paragraph (S3). `JOB-K1` and `JOB-C1` moved from
section headings into rule paragraphs; `JOB-D9` moved from a table row into
a rule paragraph; `JOB-L6` moved from the intent section to the lease
section; `JOB-J5` moved from "What is proven" to the encoding rules; and
`JOB-D7` moved from the transcript section to the declarations.

## Applied from research/vocabulary/RENAME-PLAN.md "job (13 steps)"

1. **Id form and letter table.** Applied: every rule carries
   `**[JOB-XN] Title.**` at its head, and "Reading this page" declares the
   letters for `CONTRACT.md` and `SPEC.md` together. The plan's letter list
   (A, K, C, L, R, E, D, I, T, M, V, S, W, F, G, N, B) omits `P`, `O`, `H`
   and `U`, which the pages use or now use, and its `B` is `SPEC.md`'s
   backoff letter; `E` moved to `J` (above).
2. **operation, work → job (D1).** Applied in prose on all three pages. The
   wire names (`operation_id`, `OperationSnapshot`, `WorkState`,
   `WorkFailure`, `ListAccountWork`, `CancelOperation`) and the methods
   `CancelWork`, `ObserveWork` and `ListWork` stay, mapped in the
   prose-to-wire table: the methods become Cancel, Observe and List at
   `acceptance@2` (RENAME-PLAN §4 "first"). `ref:51`, `ref:59–60`,
   `ref:113–130` (the site glossary), `serve/jobs.go:66` and
   `monitor/work_page.go` are outside this change's writes.
3. **scope (caller), principal → subject (D16).** Applied. The Go parameter
   `authenticatedCallerScope` and `acceptance.thrift`'s doc strings keep
   "caller scope" and are mapped in the prose-to-wire table.
4. **claim → acquire (D24).** Applied in prose. `Store.Claim`, the Python
   `claim`, the scenario operation `claim` and the test name
   `a_claim_keeps_what_was_written_since_the_caller_read` keep the word;
   the deprecated interface keeps its name until removal (D24).
5. **recall → yield request (D49).** Applied in prose. The record field
   `lease.recall`, `Store.Recall`, the content name
   `abstraction.job/recall@1`, the transcript field and the scenario
   operation keep the word.
6. **supervisor → runner (D38).** Applied. The download runner's files
   `supervisor.json` and `supervisor.sock` keep their names.
7. **request key → idempotency key (D57).** Applied in prose;
   `RequestIdentity` → `IdempotencyKey` waits for `acceptance@2` and is in
   the prose-to-wire table. The Panel's field text
   (`monitor/work_page.go:11`) belongs to the Panel step.
8. **legacy records, Store → deprecated job store (D30, D33).** Applied. No
   removal release is named in `RENAME-PLAN.md` or `DECISION.md`, and the
   job store is recorded "kept", as config and asks recorded theirs. The
   command `openabstractions jobs migrate-legacy` and the Go error
   `ErrLegacyOwnership` keep their names.
9. **binding (language) → implementation (D28).** Applied. "Binding" in
   the D27 sense (a client restoring its binding to the service) stays.
   The test names `TestNoBindingLetsALeaseMoveWhatTheWorkIs` and
   `TestNoBindingHandsOutItsOwnState` keep theirs.
10. **layer → module (D6).** Applied on all three pages.
11. **Lease's job sense stated once with JOB-L1 (D89).** Applied in
    JOB-L1: a job lease is exclusive, carries an epoch, and ends by lapsing;
    a yield request moves the expiry earlier. The glossary's link to
    JOB-L1 is the site's (`ref`) and outside this change's writes.
12. **history window, holder, epoch, kind kept (D76).** Nothing to change.
13. **Prior art, Why, Where a store comes from, Layout, transcript →
    `HISTORY.md`.** Applied for Prior art, Why, Where a store comes from,
    Layout, What is proven and the transcript's rationale (below). The
    transcript notation itself (the eleven fields, the verdicts,
    `--capabilities`, the scenario operations) stays in `CONTRACT.md` under
    Outcomes: `scripts/behaviour-conformance.sh` fails unless each field and
    verdict it compares, and `--capabilities`, appears in backticks on
    `CONTRACT.md` itself.

## Design records

- `research/vocabulary/DECISION.md` §2: D1, D6, D16, D24, D28, D30, D33,
  D38, D49, D57, D63 (a waiting budget → deadline, in JOB-A6), D73 (host →
  machine where a machine is meant), D76, D89.
- `research/vocabulary/DECISION.md` §3 (S1–S13): the page shape. S2's order
  puts admission first, then the record's rules, then Outcomes, Bounds,
  Divergences and Not built. S6 gathers the divergences the old page stated
  in place: refusing unknown fields, `error` as a bare string, half-open
  ranges, RFC 8785's two, I-JSON, `Any`'s `type_url`, polkit's subject,
  `orphans()` and BITS' `complete`.

## Moved here

### Rationale cut from the rules

- **JOB-K1.** "That is what lets one layer evolve without disturbing the
  others — and it is the answer to the fair objection that an abstraction
  which changes shape every time a new tool shows up is not an abstraction,
  it is a union of tools."
- **JOB-M1.** "It is stated here because it was believed and not held: the
  in-process bindings ran the caller's closure and checked only the envelope
  afterwards, so a changed spec was written and reported as success, while
  the service binding left these fields out of the fields a write copies and
  so SILENTLY DISCARDED the same change. Two bindings answering one call
  differently is the failure this whole page exists to prevent, and neither
  half of it was reachable by a test that asked one binding one question."
- **JOB-M2.** "The in-memory binding is where this is easy to get wrong and
  where it WAS wrong."
- **JOB-P1.** "The survey went looking for a standard here and found five
  systems refusing to define one."
- **JOB-C3.** "It was not chosen from a measurement, and saying otherwise
  would be inventing one." The one measured part, the conversion at the
  wire, stays under Divergences.
- **JOB-L4.** "And a claim from a record read a moment ago keeps an intent
  set since: the compare-then-rename this replaced compared the epoch and
  wrote the caller's copy, which erased the intent while reporting success."
- **Awake.** "It was added because a measurement demanded it: Windows put a
  laptop into Modern Standby with work in flight and the work ran at a third
  of its speed for four minutes, because nothing in the chain had told the
  platform a job was running. The abstraction is the only party that knows."
- **JOB-F1.** "The measured cost is zero and the measured limit is the
  point. The published `go/v0.1.0` was run against this tree over five
  format changes, the record corpus, and 19 records from a real store —
  **nothing was refused at decode by either version**, because the envelope
  never moved (the same sixteen top-level fields throughout). The one change
  that *did* break an older reader was a rule, not a field: a `v0.1.0` lease
  holder walks a `complete` record back to `pending`."
- **JOB-D7.** "Go called it `refused` and the other two called it `invalid`
  for as long as the mechanism existed, because no scenario could reach it."
- **JOB-V4.** "The service binding would have got the rule for free by
  leaving the envelope out of the fields a write may carry — and that would
  have made it *silently ignore* what the other two *refuse*, which is the
  same application on two bindings behaving differently."

### Declarations: where `content` and `critical` came from

There is **no `schema` field**. There was — an integer, bumped five times —
and the version conflated *what changed* with *what you must understand*, so
the only safe response to an unknown number was to refuse a whole record over
an addition the reader could have ignored. It is replaced by two lists.

X.509 settled the *fallback* decades ago with critical certificate
extensions (RFC 5280 §4.2: reject on an unrecognised critical extension,
"MAY be ignored" otherwise), and that half is taken straight from it. It did
not settle the *unit*: an X.509 extension is always a value, so criticality
there only says what to do when you cannot parse one. The mechanism that
names a rule is JSON Schema's `$vocabulary` (2020-12 §8.1.2) — a required
vocabulary is a set of keywords with semantics, and an implementation that
does not recognise one "MUST refuse to process any schemas that declare this
meta-schema". That is the ancestor of `abstraction.job/terminal@1`, not
X.509.

**The shape is JOSE's `crit`** (RFC 7515 §4.1.11): a list naming things
carried elsewhere in the same header that a recipient must understand or
reject the whole object. Two parallel lists, exactly like ours.

**What happens when the two lists disagree is COSE's answer, not JOSE's, and
the binding citation is RFC 9052 §3.1.** RFC 7515 §4.1.11 puts the
obligation on the writer — *"Producers MUST NOT include Header Parameter
names … that do not occur as Header Parameter names within the JOSE Header
in the `crit` list"* — and gives a recipient only a **MAY**: *"Recipients
MAY consider the JWS to be invalid …"*. RFC 9052 §3.1 is the one that binds a
reader: *"If the `crit` value list includes a label for which the header
parameter is not in the protected-header-parameters bucket, this is a fatal
error in processing the message."* We refuse, so COSE is the ancestor we
implement and JOSE is the shape we borrowed.

The four ancestors mark only a value carried in the document; the unit of
`critical` here is a feature, a named bundle of obligations (JOB-D6).

### What is proven, and what is not

The cross-language relay (`scripts/xlang-job.sh`, `scripts/conformance.sh`)
was removed with the Python and C++ stores in 0.1.8; its transcripts stay as
evidence.

**Proven** before that removal
([`docs/results/XLANG2.txt`](https://github.com/openabstractions/abstractions/blob/main/docs/results/XLANG2.txt)):
a job is created in Go with a spec **neither tool understands**, worked on in
Go, abandoned without release, found as an orphan by **Python**, adopted at
epoch 2, resumed from the checkpoint its predecessor proved, finished in
Python, and read back in Go. The stale Go owner is refused when it returns
with its old epoch. 30 tests across the two implementations, including the
zombie-owner and expired-lease cases.

**And one thing only the cross-language test could find**
([`docs/results/CONFORM1.txt`](https://github.com/openabstractions/abstractions/blob/main/docs/results/CONFORM1.txt)).
Go encodes `time.Time` as RFC3339**Nano**, which trims trailing zeros, so Go
wrote `…T06:23:11.22275Z` where Python wrote `…T06:23:11.222750Z` for the
same instant. Both are valid RFC 3339, both parse, and every unit test in
both languages passed — but the record changed bytes every time the two took
turns, so a diff of a job's history meant nothing. The timestamp format is
now pinned to exactly six fractional digits and a `Z` (JOB-J5).

**Not proven yet.** No bytes move in that test, deliberately — resume over
real bytes is tested in
[`download/`](https://github.com/openabstractions/abstraction-download). What
nothing has tested is a kill during a real multi-gigabyte transfer, which
needs the service tier.

### The transcript: why it is shaped this way

A relay between implementations proves they can continue each other's work.
It cannot prove they would each have done the same thing alone: whoever
reaches a branch second inherits the first one's answer. That is why
`behaviour-conformance.sh` gives every implementation its own store. Since
0.1.8 the Go driver is the one registered implementation.

Nothing else is compared. Not timestamps, ids or owner strings — three
implementations cannot agree on a clock and nothing branches on the rest.
Not mid-transfer progress, which is advisory, and pinning it would make a
buffer size a contract. Not concurrent writers, which are non-deterministic
by construction, so no transcript of them could be compared at all; that
property belongs in each language's own tests.

The roster was fixed at three, and a language that cannot run a scenario is
printed as a gap and counted, never skipped: a gap that reads as a pass is
the failure this instrument exists to remove. Three implementations that do
not know the same names are not three implementations of one contract,
whatever the transcripts say.

`plant`: without it the mechanism has no scenario, and it had none for as
long as it existed. An operation no driver implements is an operation whose
divergences no scenario can ever see, so the list is on the contract page
rather than only in three sources: `renew` was missing from all three for as
long as they existed, and the only thing that found it was a person reading
five stores by hand.

The UNEXERCISED count is the untraced half of the requirements traceability
matrix, which is the half that instrument exists to print. Nameless, it
reads as a house eccentricity.

### Prior art this was taken from

Nothing here is invented where something already worked:

- **Windows BITS** — persistent jobs with a GUID any process can open,
  ownership transfer, resume across reboot. The yardstick, and on Windows
  probably the implementation to wrap rather than replace.
- **Google `longrunning.Operation`** — the standard shape for polling a
  handle you did not create, and the opaque-`metadata` idea this record's
  `spec` copies.
- **iOS background `URLSession`, Android `WorkManager`** — hand work to an OS
  service, get killed, re-attach by identifier. Shipped in 2013; what is
  missing is a portable, cross-language version.
- **Temporal activity heartbeats** — one channel carrying liveness, progress
  and the resume checkpoint.
- **rsync `--append-verify`** — never append to a prefix you have not
  proven.
- **Chubby** — the lease, and the sequencer this record calls an epoch.
- **JOSE `crit` (RFC 7515) and JSON Schema `$vocabulary`** — a list of names
  a reader must understand or refuse the whole document, for a value and for
  a rule respectively.
- **tus and BitTorrent BEP 3** — a committed offset, and a bitfield of
  proven pieces.

**Every concept on the contract page names its ancestor or says it has
none.** A name with no ancestor is either something nobody has built, which
needs defending, or a rename by accident, which is the commoner case: of 61
concepts surveyed across that page and `abstraction-download/README.md`,
exactly one had no prior art and six that looked novel turned out to be
badly named. Where our name and the established one differ, the contract
says so under Divergences.

Full surveys, with sources:
[`openabstractions/research`, `async/`](https://github.com/openabstractions/research/blob/main/async/)
and [`transfer/`](https://github.com/openabstractions/research/blob/main/transfer/).

### Where a store comes from

Nothing on the contract page says where a root is; every rule there governs
what is inside one. The installed runtime opens its managed job root,
`<state-dir>/jobs`, and `openabstractions jobs migrate-legacy` converts
deprecated job store records found there. No shipped tool discovers any
other store since `dl`, `jobd` and `jobctl` were removed in 0.1.8; the
discovery locations they followed (`JOB_STORE`, `MODELGET_STORE`,
`ABSTRACTION_STORE`, the per-user settings' `store`, then
`<home>/.abstraction`) are recorded in docs/REMOVED.md in the abstractions
repository. A program that embeds the store names its root. This carries no
rule id, because no sequence of store calls can observe it: every harness is
handed a root.

### Layout

```
job/
  go/         Go implementation      go test ./...
```

The Python and C++ file-store implementations and the `jobctl` drivers were
removed in 0.1.8 (docs/REMOVED.md in the abstractions repository); the Go
store is the runtime job provider's persistence. Standard library only. An
abstraction that needs a dependency to read a JSON file has misjudged its
own weight.

### Why

The rationale behind a rule, kept out of the rule statement.

**JOB-L1.** A lease is a lease in the Chubby sense: time-bounded ownership
the holder keeps by renewing and loses by lapsing — the same thing
`coordination.k8s.io/v1` Lease spells `holderIdentity` + `renewTime`, etcd
spells as a TTL, SQS calls a visibility timeout and beanstalkd calls TTR. The
epoch is Chubby's sequencer (§2.4), which DDIA names a fencing token, Kafka
names a leader epoch and the Kubernetes Lease counts as `leaseTransitions`.
The epoch-checked write is `If-Match` with a strong ETag (RFC 9110 §13.1.1) —
etcd's `mod_revision`, Kubernetes' `resourceVersion`.

**JOB-R8.** A Chubby lease expires; nothing asks for it back. Both systems
that genuinely arbitrate a resource have that other half: WDDM2 signals a
process that its video-memory budget changed, expects it to trim, and evicts
it if it does not; Android tells a process to trim and kills it if it does
not.

**JOB-R5.** This is WDDM's shape and not Android's: a lease cannot kill a
process on another machine, but it can stop believing in it, and everything
that already refuses a lapsed epoch enforces the yield request for free.

## Not carried forward here

These files cite a retired `JOB-E` id and are outside this change, and
`scripts/check.baseline` records each bracketed citation as a
`contract-tags-stray` row:

- `openabstractions-flat/abstraction-download/CONTRACT.md` (`JOB-E8`) and
  `openabstractions-flat/abstraction-download/go/cmd/replay/main.go`
  (`JOB-E1`): the download module's own rename owns these; its citations
  read JOB-J8 and JOB-J1, and its rows drop from the baseline with them.
- `openabstractions-flat/abstraction-job/job.thrift` (`JOB-E6`, `JOB-E7`,
  `JOB-E8`): a definition file, left unedited in this change; its comments
  read JOB-J6, JOB-J7 and JOB-J8.
- `openabstractions-flat/adopter-comfyui/_vendor/abstraction_job.py`: a
  byte-for-byte copy of a published tag (`scripts/vendor-comfyui.sh`),
  refreshed only from its source.
- `VISION.md`, `docs/results/` and the dated run transcripts under
  `research/`: historical record, quoting the id each carried on its day.
  `docs/BASE-PROTOCOL-CHANGES.md` entry 15 cites `JOB-E8` unbracketed for
  the same reason.

`monitor/work_page.go` (steps 2 and 7) belongs to the Panel step, and the
site glossary's entries (steps 2 and 11) to the site; neither is changed
here.
