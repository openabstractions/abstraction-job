# job — a handle to work happening somewhere else

You start a 40 GB model download. You close the laptop. You come back and it
starts again from zero.

That happens because the download lives *inside* the application. When the app
sleeps, crashes, or is closed, the work dies with it — there was never anything
else holding it. Every local-AI tool ships its own downloader with this same
shape, and every one of them loses your bytes the same way.

`job` is the layer underneath that fixes it. A job is not a function you call and
wait for. It is a **record on disk that describes work**, plus rules for who is
allowed to do it right now. The application holds a reference; the work itself
belongs to the machine.

Once that is true, sleeping, crashing and restarting stop being special cases.
They are all just *the owner going away*, and a new owner picks up where the last
one left off.

---

## What it knows, and what it deliberately does not

It does not know what the work **is**. `spec` and `checkpoint` are opaque, and
`kind` says who can read them. A download's artifact, sources and destination
live in a *download's* spec, not here.

That separation is why this record can settle. An earlier version carried
artifact, sources and sink directly, so every improvement to downloading —
mirrors, chunk manifests, webseeds — forced a schema change on a record that Go,
Python and eventually C++ all have to agree about. Google's long-running
operations reached the same conclusion with an opaque `metadata` typed by the
method that created it.

```
    application                       the record on disk
    ┌───────────────┐                 ┌──────────────────────────┐
    │  submit(spec) │ ───────────────▶│ id · kind · state        │
    │      ↓        │                 │ spec · checkpoint        │
    │   job id      │◀────────────────│ progress · lease         │
    └───────────────┘                 └──────────────────────────┘
            │                                     ▲
      app is killed                               │
            ✕                        anyone may claim it and continue
                                                  │
                                     ┌────────────┴─────────────┐
                                     │  another process         │
                                     │  another language        │
                                     │  after a reboot          │
                                     └──────────────────────────┘
```

The job id is a plain string. Write it to a config file, print it, pass it to
another program. It stays meaningful after the process that created it is gone.

---

## Try it

Both implementations use one directory. Nothing runs in the background; there is
no daemon and no database.

**Go**

```go
store, _ := job.NewFileStore("/var/lib/jobs")

rec := job.Record{Kind: "download"}
rec.SetSpec(mySpec)                  // opaque here; the download layer reads it
id, _ := store.Submit(rec)

r, _ := store.Claim(id, "my-worker", 30*time.Second)
store.Update(id, r.Lease.Epoch, func(r *job.Record) error {
    r.Progress.Done = 1 << 20
    return r.SetCheckpoint(myCheckpoint)   // what a successor would need
})
```

Most callers never touch this directly — they use a layer that knows a kind, like
[`download.Submit`](../download/README.md).

**Python**

```python
store = FileStore("/var/lib/jobs")

r = store.load(job_id)
print(r.kind, r.state, r.progress.done, r.checkpoint)

for orphan in store.orphans():           # nobody is working on these
    mine = store.claim(orphan.id, "python-worker", ttl_seconds=30)
    resume_from = mine.checkpoint        # what its predecessor proved
```

**Shell** — `jobctl` exists in both languages so scripts can drive either:

```bash
JOB_STORE=/var/lib/jobs jobctl submit --kind download --spec '{"artifact":{}}'
JOB_STORE=/var/lib/jobs jobctl orphans
```

Both pass the spec through untouched. Neither knows what a download is, which is
the same contract the packages themselves keep.

---

## Three rules, and why each exists

### 1. The spec is opaque, and `kind` says who can read it

```json
"kind": "download",
"spec": { "artifact": {}, "sources": [], "sink": {} }
```

This package stores and returns the spec without understanding it. A reader that
meets a `kind` it does not know leaves that job alone rather than guessing.

That is what lets one layer evolve without disturbing the others — and it is the
answer to the fair objection that an abstraction which changes shape every time a
new tool shows up is not an abstraction, it is a union of tools.

### 2. A successor inherits what its predecessor proved

```json
"progress":   { "done": 460 },
"checkpoint": { "verified_prefix": 400 }
```

`progress` is best-effort, explicitly non-monotonic, and nothing may decide
anything on it — a job resuming after a crash can legitimately report a smaller
number than before. The survey went looking for a standard here and found five
systems refusing to define one.

The **checkpoint** is the load-bearing part: whatever a successor needs in order
to continue. Above, the predecessor wrote 460 units but proved only 400, so the
next owner resumes from 400 and the unproven remainder is discarded.

This is Temporal's activity-heartbeat design, copied deliberately: a retried
worker is handed the dead worker's last checkpoint rather than starting over.

### 3. Ownership is a lease with an epoch

```json
"lease": { "owner": "go-worker", "epoch": 2, "expires_at": "…" }
```

The epoch rises by one on every claim, and **every write must present the epoch
it holds**. A process suspended past its own expiry wakes up still believing it
owns the job; its writes carry a stale epoch and are refused.

Without that, two owners work the same job and both believe the result is
correct. That is the damage this design exists to prevent.

Claims are taken by creating `<id>.epoch.<n>` with exclusive-create, which is
atomic on NTFS and POSIX. Because every generation gets its own filename, a token
left behind by a killed process blocks nothing — the next claimant simply takes
the next epoch. A single lockfile would have to be broken by timeout, and
breaking locks by timeout is how two owners end up working one job.

---

## Reclaiming is the mechanism; handing off is only an optimisation

A process killed with `SIGKILL`, or a machine that loses power, never gets to
hand anything over. So the primary path is **adoption**: on start, look for jobs
whose lease has expired and claim them.

```python
for orphan in store.orphans():
    store.claim(orphan.id, "me", ttl_seconds=30)
```

`release()` exists so a polite exit frees the job in seconds instead of after the
expiry — but nothing depends on it, which is exactly the point. A design that
*requires* graceful handoff has no answer for the case that actually loses your
40 GB.

---

## The record is the contract

The Go and Python implementations are not ports of each other and are not
generated from a shared schema. They agree about one thing: **the JSON file on
disk, and the rules for taking it over.** Each language's API looks like that
language.

| field | meaning |
|---|---|
| `schema` | refused if unknown, never guessed at |
| `id` | opaque, sortable by creation time, safe to pass around |
| `kind` | what this job is, and who can read `spec` and `checkpoint` |
| `state` | `pending` · `running` · `transferred` · `complete` · `failed` · `cancelled` |
| `spec` | the immutable description of the work. **Opaque here** |
| `checkpoint` | what a successor needs to resume. **Opaque here** |
| `progress` | `done`, `total` — best-effort, decide nothing on it |
| `lease` | `owner`, `epoch`, `expires_at` |
| `delegation` | set when an external system owns the work |
| `requires` | capabilities an implementation must have to take this job |

**`delegation` is the architecture, not an accommodation for one tool.** When an
app's worker hands off to a system service, or that service hands off to a NAS,
the handle that finds the work again is `{system, external_id}` — for Windows
BITS, a job GUID that survives a reboot. When it is set, `progress` is a *cache*
of what the external system last reported; the external system is the truth.

**`transferred` is not bureaucracy.** It means the work is finished and proven,
but the result has not been taken delivery of. BITS has the same two-phase shape
for the same reason, and will not hand over a file until you call `Complete()`.
Collapse the two states and you cannot express *"the service finished this while
ComfyUI was closed"*, which is the case this is built for.

---

## What is proven, and what is not

```bash
bash scripts/xlang-job.sh
```

**Proven** ([`docs/results/XLANG2.txt`](../docs/results/XLANG2.txt)) — a job is
created in Go with a spec **neither tool understands**, worked on in Go, abandoned
without release, found as an orphan by **Python**, adopted at epoch 2, resumed
from the checkpoint its predecessor proved, finished in Python, and read back in
Go. The stale Go owner is refused when it returns with its old epoch.

30 tests across the two implementations, including the zombie-owner and
expired-lease cases.

**Not proven yet.** No bytes move in that test, deliberately — resume over real
bytes is tested in [`download/`](../download/README.md). What nothing has tested
is a kill during a real multi-gigabyte transfer, which needs the service tier.

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

Full surveys, with sources: [`research/async/`](../research/async/) and
[`research/transfer/`](../research/transfer/).

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
