# abstraction-job

A job is a record of background work, kept outside the process that started it,
so a different process — in another language, or after a reboot — can take the
work over and continue it.

## The problem

An application that runs a long transfer inside its own process keeps the state
of that transfer in memory: how far it got, and how much of that has been
verified. When the process exits the state goes with it and the next run starts
from nothing. This package puts the state in a record and defines who may write
to it, so a second process can find unfinished work and resume from the last
point the first one proved — see [the handoff](#cross-language-handoff) below.

## Install

`git clone https://github.com/openabstractions/abstraction-job.git` gets
everything; to depend on one language's part of it:

**Go.** `go get github.com/openabstractions/abstraction-job/go`. The module path
ends in `/go` because the Go code lives in a subdirectory; without that suffix
`go get` fails. The package name is `job`.

**Python.** Not on PyPI. `python/abstraction_job.py` is one file with no
dependencies: copy it next to your code, or put `python/` on `PYTHONPATH`.

**C++.** With CMake, pinning a commit:
```cmake
include(FetchContent)
FetchContent_Declare(abstraction_job
    GIT_REPOSITORY https://github.com/openabstractions/abstraction-job.git
    GIT_TAG <a commit sha> GIT_SHALLOW TRUE)
FetchContent_MakeAvailable(abstraction_job)
target_link_libraries(your_target PRIVATE abstraction::job)
```

## A minimal example

Each program submits a job, claims it, writes progress and a checkpoint, and
prints id, state, epoch and progress. A **kind** names what the work is and says
who may read the **spec** (the description of the work) and the **checkpoint**
(what a successor needs to continue); both are opaque here.

```go
package main

import (
	"fmt"
	"log"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

func main() {
	store, err := job.NewFileStore("./jobs")
	if err != nil { log.Fatal(err) }
	rec := job.Record{Kind: "download", Spec: []byte(`{"url":"https://example.invalid/m.bin"}`)}
	id, err := store.Submit(rec)
	if err != nil { log.Fatal(err) }
	held, err := store.Claim(id, "worker-a", 30*time.Second)
	if err != nil { log.Fatal(err) }
	done, err := store.Update(id, held.Lease.Epoch, func(r *job.Record) error {
		r.Progress.Done = 460
		return r.SetCheckpoint(map[string]int64{"verified_prefix": 400})
	})
	if err != nil { log.Fatal(err) }
	fmt.Println(done.ID, done.State, done.Lease.Epoch, done.Progress.Done)
}
```
```python
from abstraction_job import FileStore, Record

store = FileStore("./jobs")
job_id = store.submit(Record(id="", kind="download",
                             spec={"url": "https://example.invalid/m.bin"}))
held = store.claim(job_id, "worker-a", ttl_seconds=30)

def mutate(r):
    r.progress.done = 460
    r.checkpoint = {"verified_prefix": 400}

done = store.update(job_id, held.lease.epoch, mutate)
print(done.id, done.state, done.lease.epoch, done.progress.done)
```
```cpp
#include <abstraction/job/record.h>
#include <abstraction/job/store.h>
#include <chrono>
#include <iostream>

namespace job = abstraction::job;
int main() {
    job::FileStore store("./jobs");
    job::Record rec;
    rec.kind = "download";
    rec.spec = job::Json{{"url", "https://example.invalid/m.bin"}};
    const std::string id = store.submit(std::move(rec));
    job::Record held = store.claim(id, "worker-a", std::chrono::seconds(30));
    const job::Record done = store.update(id, held.lease.epoch, [](job::Record& r) {
        r.progress.done = 460;
        r.checkpoint = job::Json{{"verified_prefix", 400}};
    });
    std::cout << done.id << " " << done.state << " " << done.lease.epoch << " "
              << done.progress.done << "\n";
}
```

## Cross-language handoff

`jobctl` drives the same store from all three languages. This claims a job in Go,
abandons it without releasing the lease, adopts it in Python, and refuses the
stale Go owner when it comes back. From the repository root:

```bash
export JOB_STORE="$PWD/demo-store"
ID=$(cd go && go run ./cmd/jobctl submit --kind download --spec '{"url":"x"}' --total 1000)
(cd go && go run ./cmd/jobctl claim "$ID" --owner go-worker --ttl 2)
(cd go && go run ./cmd/jobctl progress "$ID" --epoch 1 --done 460 --checkpoint '{"verified_prefix":400}')
sleep 3                                                 # the two-second lease lapses
(cd python && python jobctl.py orphans)                 # the abandoned job is listed
(cd python && python jobctl.py claim "$ID" --owner py-worker --ttl 30)   # epoch=2
(cd python && python jobctl.py finish "$ID" --epoch 2 --state complete)
(cd go && go run ./cmd/jobctl progress "$ID" --epoch 1 --done 999)       # refused
```

The Python claim carries the checkpoint Go wrote, and the last command exits
non-zero with `stale epoch, this owner no longer holds the lease`.

## API overview

A **binding** is one implementation of the `Store` interface: three in Go (a
directory, a socket client, a map), two in Python, one in C++. All implement the
these. Python uses `snake_case` names, C++ the same with `std::chrono` durations.

| operation | meaning |
|---|---|
| `Submit(Record) (id, error)` | records new work; returns the id |
| `Load(id)`, `List()` | one record, or every job oldest first. No lease needed, at any time |
| `Orphans()`, `Claimable(*Record) bool` | work to adopt: jobs nobody holds a lease on that are neither finished nor paused. `Claimable` answers only about observed state, so it still returns true for a paused job that `Orphans` leaves out |
| `Claim(id, owner, ttl)` | takes ownership; returns the record at a new epoch |
| `Renew(id, epoch, ttl)`, `Release(id, epoch)` | extends a lease, or gives it up early. Renew refuses once the lease has expired even if the epoch still matches; release is optional, and without it a lease simply lapses on its own |
| `Update(id, epoch, mutate)` | the only way to change a record. Refused unless the caller still holds the lease at the epoch it presents |
| `SetIntent(id, want, by)` | the one write that presents no epoch, so a job can be paused or cancelled by someone who is not its owner |

Go adds `Open(store, id, owner)` for a per-job handle with `Cancel`/`Pause`/
`Resume`, `Watch(store, kind)` polling every 750 ms, `Serve` and `Dial` for the
socket binding, `NewMemory()`, and `Scratch` (`Root`, `WorkPath`) on `FileStore`.

### Record fields

The keys a record carries. All three implementations write the same keys, in the
same order, with the same timestamp format.

| field | meaning |
|---|---|
| `content`, `critical` | the data models this record carries, as namespaced names such as `abstraction.job/base@1`, and the subset a reader must understand or refuse the record entirely. These replaced an integer `schema` field; records carrying `schema` 3, 4 or 5 are still read, and the integer is no longer written |
| `id`, `kind` | an opaque string sortable by creation time, and what the job is |
| `state`, `intent` | what is happening — `pending`, `running`, `transferred`, `complete`, `failed`, `cancelled` — and what somebody wants to happen: `want` (`run`, `pause`, `cancel`), `by`, `at`, absent meaning run. `transferred` means finished but not yet collected, which is how a record says an external service finished while the application was closed |
| `spec`, `checkpoint` | the work's description, and what a successor needs to resume. Opaque here |
| `progress` | `done`, `total`, `updated_at`, optional `step`. Best-effort and not monotonic: nothing may decide anything on it |
| `lease` | `owner`, `epoch`, `expires_at`. A lease is the time-bounded right to write; the epoch rises by one on every claim, and a write presenting a stale epoch is refused, so an owner suspended past its expiry cannot overwrite its successor |
| `delegation` | `system`, `external_id`, `delivered`, when an external system owns the work. `progress` is then a cache of what that system reported |
| `requires`, `error`, `extensions` | capabilities an implementation needs to take this job; the last failure, kept so it outlives the process that hit it; and data this layer does not understand, keyed by a name that says who does, which a reader that cannot read it must preserve on write |
| `created_at`, `updated_at` | UTC, ISO-8601, exactly six fractional digits, trailing `Z` |

## Status

Experimental. No tagged release and no compatibility promise: the record format,
the `Store` interface and the `jobctl` commands may all change. Tested at this
commit, recountable with the commands below: 41 Go test functions (among them the
exclusive claim, stale-epoch refusal, lease expiry, and one body of assertions
run against all three Go bindings), 21 Python tests, and two C++ test binaries
printing 83 assertions between them.

Not covered: no test moves real bytes, so resume across a kill during a large
transfer is unverified here. `delegation` round-trips through the record but has
no adapter here, and the socket binding has no authentication and is meant for a
loopback socket. `job.thrift` is a design sketch: nothing is generated from it,
and its header lists where it differs. The CMake build also produces
`abstraction::discovery`, a supervisor client; `abstraction::job` does not link it.

## Requirements

**Go** 1.26 or newer, as declared in `go/go.mod`; standard library only.
**Python** 3.8 or newer, tested on 3.12; standard library only. **C++17**, CMake
3.16 or newer, and [nlohmann/json](https://github.com/nlohmann/json) 3.9 or newer,
fetched by CMake if absent. All three built and tested on Windows 11, MSVC for C++.

```bash
(cd go && go build ./... && go test ./...)
(cd python && python -m unittest discover -p "test_*.py")
cmake -S . -B build && cmake --build build && ctest --test-dir build
```

## Licence

Apache-2.0. See [LICENSE](LICENSE).
