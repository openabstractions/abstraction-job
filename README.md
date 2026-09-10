# abstraction-job

**Ready.** Cross-language conformance passes (Go, Python, C++) and the example
below runs as shown. No version number is typed on this page: a tag is the only
thing that cannot drift, so
[the tag list](https://github.com/openabstractions/abstraction-job/tags) is the
answer to "which release".

A record on disk describing work somebody asked for, and rules for who is allowed
to be doing it right now. Any process, in any of three languages, can pick up
work another one left.

Long work — a large download, an import, a conversion — is normally held in the
memory of the program that started it, so closing that program, or a crash, or a
reboot destroys the fact that the work was ever wanted. A job puts that fact
somewhere the process does not own: what is wanted, how far it got, what a
successor needs to carry on, and a lease saying who holds it. Sleeping, crashing
and restarting stop being special cases; they are all the owner going away, and
the record is still there when somebody else looks.

One layer of [Open Abstractions](https://github.com/openabstractions/abstractions),
the parent project, which holds the scope rules, the method, the measured results
and the conformance suite that judges implementations of this contract.

## Install

    go get github.com/openabstractions/abstraction-job/go

[Releases, newest first](https://github.com/openabstractions/abstraction-job/tags).
`go get` with no version takes the newest; pin the exact tag you tested against.
**Go 1.26 or later is required.**

**Python** is in this repository and on no package index —
[what to install, import and call](python/README.md). **C++** is here too, with
no tagged release; see Requirements.

Whether to adopt this at all, what it costs and what is not proven:
[Adopting](CONTRIBUTING.md#adopting).

## An example that runs

One worker takes a job, records how far it got, and goes away. A second worker
picks it up and reads what the first one proved.

```go
package main

import (
	"fmt"
	"time"

	job "github.com/openabstractions/abstraction-job/go"
)

func main() {
	store, _ := job.NewFileStore("jobs")

	rec := job.Record{Kind: "example"}
	rec.SetSpec(map[string]string{"fetch": "anything at all"})
	id, _ := store.Submit(rec)

	mine, _ := store.Claim(id, "worker-a", time.Minute)
	store.Update(id, mine.Lease.Epoch, func(r *job.Record) error {
		r.Progress.Done = 1 << 20
		return r.SetCheckpoint(map[string]int{"bytes": 1 << 20})
	})
	store.Release(id, mine.Lease.Epoch)

	next, err := store.Claim(id, "worker-b", time.Minute)
	if err != nil {
		panic(err)
	}
	fmt.Printf("worker-b took over at %d bytes, resuming from %s\n",
		next.Progress.Done, next.Checkpoint)
}
```

It prints `worker-b took over at 1048576 bytes`, then the checkpoint. Nothing runs
in the background: no daemon, no database, one directory of files. `worker-b` is a
second claim in the same program here, but the store does not require that — it
can be another process, another language, or this machine after a reboot.

## API

**`Store`** is the whole interface; `NewFileStore(root)` is the implementation
that ships. `jobctl`, present in all three languages, drives a store from a
shell, and finds one rather than asking to be told — its own `JOB_STORE`, then
`ABSTRACTION_STORE`, then the machine's `abstraction/config.json`, then
`~/.abstraction`. See
[CONTRACT.md § Where a store comes from](CONTRACT.md#where-a-store-comes-from).

| call | what it does |
|---|---|
| `Submit(record)` | write a new job, return its id |
| `Load(id)` / `List()` | read one, or all |
| `Claim(id, owner, ttl)` | take ownership for a while. Fails if somebody else holds it |
| `Renew` / `Release` | extend or give up ownership |
| `Update(id, epoch, mutate)` | change the record. The epoch you were given must still be current, or the write is refused |
| `Orphans()` | jobs nobody is working on: the lease lapsed, or was released |
| `SetIntent(id, want, by)` | ask the owner to pause, resume or cancel, when you are not the owner |
| `Recall(id, epoch, reason, by, grace)` | demand the record back from an owner that has stopped answering |

A **lease** is time-bounded ownership in the sense Chubby uses (Burrows, OSDI
2006); the **epoch** on every write is its fencing token, so a worker that froze
and woke up cannot overwrite its successor. A **checkpoint** is what a successor
needs to carry on, **progress** is what was observed, and **intent** is what
somebody wants to happen — written by anyone, honoured by the owner.

**`spec` and `checkpoint` are opaque here** and `kind` says who may read them: a
download's artifact, sources and destination live in a download's spec, and this
layer parses neither. The ancestor is [AIP-151](https://google.aip.dev/151)'s
`longrunning.Operation.metadata`, not Kubernetes' `spec`. `job.thrift` states the
record in a notation that is nobody's language, and every normative rule is on
[CONTRACT.md](CONTRACT.md), tagged, with each conformance scenario citing the tag
it tests.

## Status

Experimental. Go is the only language with a tagged release, and no release
carries an API stability promise.

- **A reader that meets a record feature it does not understand refuses the whole
  record** when that feature is marked critical, and carries on when it is not.
  Older binaries are safe against additions — and a field name, once shipped,
  cannot change without stopping every reader already deployed.
- **The store is one directory** and concurrency is the filesystem's, measured on
  the host filesystem. Over a share it is slower than it looks: the Windows SMB
  redirector has served a record 154 s stale
  ([`SMB1.txt`](https://github.com/openabstractions/abstractions/blob/main/docs/results/SMB1.txt)).
- **Platforms.** Every published transcript was produced on Windows or Linux;
  macOS is `UNPROVEN` in all of them.

## Requirements

**Go** 1.26 or later, depending on this project's `cas` and `watch` layers and
nothing else. **Python** 3.9 or later, standard library only, on no package
index — the install line, the example and the API are on
[`python/README.md`](python/README.md).

**C++** 17, standard library only: `cpp/src/json.cpp` is this layer's JSON
reader and there is no third-party dependency. `cpp/CMakeLists.txt` builds it,
runs its tests and installs a `find_package` package:

    cmake -S cpp -B build -DCMAKE_INSTALL_PREFIX=<prefix>
    cmake --build build && ctest --test-dir build
    cmake --install build

and then, in yours:

    find_package(abstraction_job 0.1 CONFIG REQUIRED)
    target_link_libraries(your_target PRIVATE abstraction::job)

`add_subdirectory(cpp)` gives the same `abstraction::job`, so vendoring and
installing are interchangeable at the call site.

`src/store.cpp` includes `<abstraction/cas.h>` and `job/watch.h` includes
`<abstraction/watch/watch.h>`, so that build needs
[`abstraction-cas`](https://github.com/openabstractions/abstraction-cas) and
[`abstraction-watch`](https://github.com/openabstractions/abstraction-watch):
either installed already and on `CMAKE_PREFIX_PATH`, or cloned beside this
repository, in which case they are compiled in and travel in this package.
Nothing is fetched while CMake configures — a build that reaches the network is
a dependency you did not choose, and handing you one would be the thing this
layer exists to stop.

Without a build system, clone those two beside this repository and name the
files:

    g++ -std=c++17 -I cpp/include \
      -I ../abstraction-cas/cpp/include -I ../abstraction-watch/cpp/include \
      cpp/test/test_job_record.cpp cpp/src/json.cpp cpp/src/record.cpp \
      cpp/src/ranges.cpp cpp/src/store.cpp cpp/src/awake.cpp \
      ../abstraction-cas/cpp/src/cas.cpp -o test_job_record

That is this layer's own test, so running it is how you check that your compiler
agrees with ours. Measured 2026-09-09 with g++ 15.2 and with MSVC 19.51, which
takes the same file list under `/std:c++17` and `/I`. Replace the test with your
own translation unit to get a program. `cpp/src/discovery.cpp` and
`cpp/src/discovery_client.cpp` are outside that list; they are needed only to
talk to a running `jobd`.

The timestamps a C++ record may carry are limited by the build's
`std::chrono::system_clock` — 1677 to 2262 on libstdc++, wider on MSVC — and an
instant outside that is refused as `bad_timestamp`, never wrapped. All three
languages read the same record, re-encode it identically, and carry a spec none
of them has a type for
([`CONFORM1.txt`](https://github.com/openabstractions/abstractions/blob/main/docs/results/CONFORM1.txt)).

## Licence

Apache-2.0. See [LICENSE](LICENSE).
