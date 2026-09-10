# abstraction-job, in Python

A record on disk describing work somebody asked for, and rules for who is allowed
to be doing it right now. Any process, in any of three languages, can pick up work
another one left. No daemon, no database: one directory of files. Standard library
only.

This page is the Python package. The contract, the Go and C++ implementations,
what is measured and what is `UNPROVEN` are on
[the repository](https://github.com/openabstractions/abstraction-job).

## Install

Not on PyPI, and the names on PyPI are not ours. Clone the three repositories
beside each other and install them in dependency order:

    git clone https://github.com/openabstractions/abstraction-cas
    git clone https://github.com/openabstractions/abstraction-watch
    git clone https://github.com/openabstractions/abstraction-job

    pip install ./abstraction-cas/python ./abstraction-watch/python
    pip install ./abstraction-job/python

Python 3.9 or later. The order is not a style: `pyproject.toml` names these
dependencies by the names they would have on an index, and pip can only satisfy
them from what is installed already. Copying the three modules into a `_vendor/`
directory of your own works too — each is pure standard library.

## An example that runs

One worker takes a job, records how far it got, and goes away. A second worker
picks it up and reads what the first one proved.

```python
import abstraction_job as job

store = job.FileStore("jobs")

record = job.Record(id="", kind="example", spec={"fetch": "anything at all"})
job_id = store.submit(record)

mine = store.claim(job_id, "worker-a", 60)

def got_a_megabyte(r):
    r.progress.done = 1 << 20
    r.checkpoint = {"bytes": 1 << 20}

store.update(job_id, mine.lease.epoch, got_a_megabyte)
store.release(job_id, mine.lease.epoch)

nxt = store.claim(job_id, "worker-b", 60)
print("worker-b took over at", nxt.progress.done, "bytes, resuming from", nxt.checkpoint)
```

It prints `worker-b took over at 1048576 bytes, resuming from {'bytes': 1048576}`.
`worker-b` is a second claim in the same program here, but the store does not
require that — it can be another process, another language, or this machine after
a reboot. `store.orphans()` is how the next launch finds the jobs nobody is
working on.

## What an application calls

| call | what it does |
|---|---|
| `FileStore(root)` | the store that ships. `root` is a directory |
| `submit(record)` | write a new job, return its id |
| `load(id)` / `list()` | read one, or all |
| `claim(id, owner, ttl_seconds)` | take ownership for a while. `LeaseHeld` if somebody else holds it |
| `renew(id, epoch, ttl)` / `release(id, epoch)` | extend or give up ownership |
| `update(id, epoch, mutate)` | change the record. The epoch you were given must still be current, or the write is refused with `StaleEpoch` |
| `orphans()` | jobs nobody is working on: the lease lapsed, or was released |
| `set_intent(id, want, by)` | ask the owner to pause, resume or cancel, when you are not the owner |
| `recall(id, epoch, reason, by, grace)` | demand the record back from an owner that has stopped answering |
| `watch(store, kind, budget)` | be told a record changed, and told when nothing has |

A **lease** is time-bounded ownership; the **epoch** on every write is its fencing
token, so a worker that froze and woke up cannot overwrite its successor. A
**checkpoint** is what a successor needs to carry on, **progress** is what was
observed, and **intent** is what somebody wants to happen. `spec` and `checkpoint`
are opaque dicts here and `kind` says who may read them.

Every normative rule is on
[CONTRACT.md](https://github.com/openabstractions/abstraction-job/blob/main/CONTRACT.md),
tagged; none is restated here.

## What may break

- **No tagged release and no package index.** Pin a commit you have read.
- **A store on a network filesystem is only as fresh as the client's cache.**
  Over SMB a record has been read 154 s stale by the Windows redirector —
  [the transcript](https://github.com/openabstractions/abstractions/blob/main/docs/results/SMB1.txt).
- **`watch`, which this depends on, carries no conformance verdict.**
  [What is proven and what is not](https://openabstractions.org/coverage.html).
- Every published transcript was produced on Windows or Linux. macOS is
  `UNPROVEN` throughout.

Apache-2.0. See [LICENSE](LICENSE).
