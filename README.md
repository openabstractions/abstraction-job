# abstraction-job

Keep long work running after the application closes or the service restarts.
The job service accepts the work, reports progress and retains the result for
recovery. A timeout ends only the application's wait; cancelling the work is a
separate request.

For safe retry after a lost reply, the application keeps the original
idempotency key, service binding and receipt. The receipt is the service's
evidence that a submission was accepted, naming the job and the logical owner
it was accepted for, and the subject the service files it under; holding one
grants no authority by itself. The application then observes or reconciles
that same job and submits no duplicate.

The subject in that receipt is the authenticated account and the calling
executable's absolute path, as the operating system reports it: the same rule
for every language's client. Rebuilding or moving that executable changes its
subject, and receipts filed under the old subject stay behind it, unreachable
from the new one.

## Application service path

Normal applications use resolved durable jobs and generated download requests
through [the facade](https://github.com/openabstractions/abstraction-facade).
The service owns execution and shared stores. Preserve the caller's
idempotency key and binding for recovery; cancelling a wait leaves the
accepted job alone.

The [acceptance rules](CONTRACT.md) define retries, failures and recovery. The
contract declares each rule; this page names them without restating them:

- **Retry** by submitting the next numbered attempt (the idempotency key's
  `Attempt` field, counting from zero) of the same key (contract rule
  JOB-A7). Attempt N+1 is accepted only after attempt N `failed` or was sealed
  as not accepted, sealed meaning the service recorded durable evidence that
  this key can never later succeed, and it must carry the same arguments.
- **Failures** carry a classification and a typed cause (JOB-A8). `permanent`
  means the provider will not try that job again. Treat an unrecognized cause
  as `other`.
- **`unavailable`** reports a policy decision the provider could not obtain
  (JOB-A9). The call made no admission, seal or cancellation, and the same
  idempotency key may be presented again.
- **`result_lost`** is the permanent cause recorded when a completed job's
  result bytes are gone (JOB-A10). It makes the next attempt eligible.
- **Restore** a saved binding after a restart with Go `Machine.RestoreJobs`,
  Python `Jobs.restore_installed` or `Jobs.restore`. Restoration authenticates the
  saved endpoint and refuses a different logical owner.

```go
// Retry a failed job as the next attempt of the same idempotency key.
observed, err := jobs.ObserveWork(ctx, id)
if err == nil && observed.Outcome == acceptance.ObservationOutcomeObserved &&
	observed.Snapshot.State == acceptance.WorkStateFailed {
	next := id
	next.Attempt++
	result, err := jobs.Submit(ctx, acceptance.Submission{Identity: next, Kind: kind, Spec: spec})
	if err == nil && result.Outcome == acceptance.AcceptanceOutcomeUnavailable {
		// No decision was reached; present the same attempt again later.
	}
}

// Before submitting, persist the binding with the idempotency key.
saved := jobs.Binding()
// After a restart, rebind and reconcile the same idempotency key.
restored, err := facade.Discover().RestoreJobs(ctx, saved)
if err != nil {
	return err
}
reply, err := restored.Reconcile(ctx, id)
if err != nil {
	return err
}
fmt.Println(reply.Outcome)
```

Idempotency keys belong to the calling subject: the account and program. The
[Python job client](py/README.md) documents that subject and the retained
recovery information. Until `acceptance@2` the wire spells the idempotency
key `RequestIdentity` and the methods Cancel, Observe and List as
`CancelWork`, `ObserveWork` and `ListWork`
([CONTRACT.md](CONTRACT.md#reading-this-page)).

The job store examples below are explicitly selected native provider APIs with
separate lifecycle guarantees. Their historical conformance describes that
provider, and it qualifies neither current service packages nor every
platform.


**Deprecated job store.** The job store is deprecated and kept: no release
removing it is named. The Go file store is the runtime job provider's
persistence, and the example below runs as shown. The Python and C++ file stores
were removed on 2026-09-15; the parent project's `docs/REMOVED.md` records them.
No version number is typed on this page: a tag is the only thing that cannot
drift, so
[the tag list](https://github.com/openabstractions/abstraction-job/tags) is the
answer to "which release".

A record on disk describing a job somebody asked for, and rules for who is
allowed to be doing it right now. Any process can pick up a job another one
left.

Long work — a large download, an import, a conversion — is normally held in the
memory of the program that started it, so closing that program, or a crash, or a
reboot destroys the fact that the work was ever wanted. A job puts that fact
somewhere the process does not own: what is wanted, how far it got, what a
successor needs to carry on, and a lease saying who holds it. Sleeping, crashing
and restarting stop being special cases; they are all the owner going away, and
the record is still there when somebody else looks.

One module of [Open Abstractions](https://github.com/openabstractions/abstractions),
the parent project, which holds the scope rules, the method, the measured results
and the conformance suite that judges implementations of this contract.

## Install

Install the runtime first: https://openabstractions.org/adopt.html

    go get github.com/openabstractions/abstraction-job/go

[Releases, newest first](https://github.com/openabstractions/abstraction-job/tags).
`go get` with no version takes the newest; pin the exact tag you tested against.
**Go 1.26 or later is required.**

Python and C++ applications use the generated acceptance vocabulary under
`py/` and `cpp/` through the job service; see
[Recoverable acceptance service contract](#recoverable-acceptance-service-contract).

Whether to adopt this at all, what it costs and what is not proven:
[Adopting](CONTRIBUTING.md#adopting).

## An example that runs

An application submits a job, keeps the receipt, and later checks whether it
finished: the service path every normal application uses.

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	facade "github.com/openabstractions/abstraction-facade/go"
	api "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	jobs, err := facade.Discover().ResolveJobs(ctx, facade.Requirements{})
	if err != nil {
		log.Fatal(err)
	}
	// The epoch an idempotency key is stamped with (GetHistoryWindow's
	// HistoryEpoch) is the owner's current acceptance epoch: closing it fences
	// every submission under it, including a delayed one. It is a different
	// thing from a lease's epoch, below, which fences writes to one record.
	window, err := jobs.GetHistoryWindow(ctx)
	if err != nil {
		log.Fatal(err)
	}

	id := api.RequestIdentity{Key: "example-work", HistoryEpoch: window.HistoryEpoch}
	accepted, err := jobs.Submit(ctx, api.Submission{Identity: id, Kind: "example", Spec: []byte(`{"fetch":"anything at all"}`)})
	if err != nil {
		log.Fatal(err)
	}
	if accepted.Outcome != api.AcceptanceOutcomeAccepted {
		log.Fatalf("submission not accepted: %s %s", accepted.Outcome, accepted.Reason)
	}
	receipt := *accepted.Receipt
	fmt.Println("logical owner:", receipt.LogicalOwner, "operation:", receipt.OperationID)

	// Keep id and jobs.Binding() before waiting; a lost reply is reconciled
	// with the same idempotency key, never resubmitted.
	binding := jobs.Binding()
	_ = binding

	observed, err := jobs.ObserveWork(ctx, id)
	if err != nil {
		log.Fatal(err)
	}
	if observed.Outcome != api.ObservationOutcomeObserved {
		log.Fatalf("observation refused: %s", observed.Outcome)
	}
	fmt.Println("state:", observed.Snapshot.State)

	// A restarted process presents the same idempotency key and never resubmits.
	reconciled, err := jobs.Reconcile(ctx, id)
	if err != nil {
		log.Fatal(err)
	}
	if reconciled.Outcome != api.AcceptanceOutcomeAccepted || reconciled.Receipt.OperationID != receipt.OperationID {
		log.Fatalf("reconcile: %+v", reconciled)
	}

	cancelled, err := jobs.CancelWork(ctx, id)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("cancellation:", cancelled.Outcome)
}
```

`ResolveJobs` binds a `JobsClient` to the service; `GetHistoryWindow` reads the
owner's current epoch and pins the logical owner this binding will accept
receipts from. `Submit` returns the receipt inside `accepted`; Observe
(`ObserveWork`) reads state without changing it; `Reconcile` is what a
restarted process calls with the same idempotency key in place of submitting
again; Cancel (`CancelWork`) asks for cancellation and proves nothing about
effects having stopped.

The deprecated `Store` interface below (`Claim`, which acquires a lease,
`Update`, `Release` and the lease each of those holds) is what a provider
implementer writes against; an application calls the service above. See
[For provider implementers](#for-provider-implementers).

## For provider implementers

`NewFileStore` is the shipped implementation of the deprecated `Store`. One
worker acquires a job, records how far it got, and goes away; a second worker
acquires it and reads what the first one proved:

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
second acquisition in the same program here; it can equally be another
process, another language, or this machine after a reboot.

A lease and a receipt are two different things. A lease is time-bounded,
exclusive ownership a worker holds over one record, released or renewed by the
holder, with its epoch fencing a stale holder's write (contract rule JOB-L1).
A receipt is evidence that the acceptance service accepted a submission, held
by the caller and never by whoever executes the job, and carrying no ownership
of anything; the acceptance provider admits records for existing workers and
uses no lease at all.

## API

**`Store`** is the whole deprecated job store interface; `NewFileStore(root)`
is the implementation that ships.

| call | what it does |
|---|---|
| `Submit(record)` | write a new job, return its id |
| `Load(id)` / `List()` | read one, or all |
| `Claim(id, owner, ttl)` | acquire the lease for a while. Fails if somebody else holds it |
| `Renew` / `Release` | extend or give up ownership |
| `Update(id, epoch, mutate)` | change the record. The epoch you were given must still be current, or the write is refused |
| `Orphans()` | jobs nobody is working on: the lease lapsed, or was released |
| `SetIntent(id, want, by)` | ask the owner to pause, resume or cancel, when you are not the owner |
| `Recall(id, epoch, reason, by, grace)` | send a yield request: ask the holder to give the lease back by a deadline |

A **lease** is time-bounded ownership in the sense Chubby uses (Burrows, OSDI
2006); the **epoch** on every write is its fencing token, so a worker that froze
and woke up cannot overwrite its successor. A **checkpoint** is what a successor
needs to carry on, **progress** is what was observed, and **intent** is what
somebody wants to happen — written by anyone, honoured by the owner.

**`spec` and `checkpoint` are opaque here** and `kind` says who may read them: a
download's artifact, sources and destination live in a download's spec, and this
module parses neither. The ancestor is [AIP-151](https://google.aip.dev/151)'s
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

**Go** 1.26 or later, depending on this project's `cas` and `watch` modules and
nothing else.

**C++** 17 and **Python** 3.9 or later for the generated acceptance vocabulary,
standard library only. `cpp/CMakeLists.txt` installs the header-only
`abstraction_job_acceptance` package:

    cmake -S cpp -B build -DCMAKE_INSTALL_PREFIX=<prefix>
    cmake --install build

and then, in yours:

    find_package(abstraction_job_acceptance 0.1 CONFIG REQUIRED)
    target_link_libraries(your_target PRIVATE abstraction::job_acceptance)

## Licence

Apache-2.0. See [LICENSE](LICENSE).

## Recoverable acceptance service contract

The additive [acceptance.thrift](acceptance.thrift) defines
`abstraction.job/acceptance@1`: stable idempotency keys scoped to a subject,
recoverable receipts, reconciliation and explicit job cancellation. See the
JOB-A rules in [CONTRACT.md](CONTRACT.md). The deprecated job store is
unchanged, and no deprecated provider implements durable acceptance by this
addition.

Generated Go, C++ and Python service vocabulary and codecs live under
`go/abstraction/job/acceptance`, `cpp/abstraction/job/acceptance` and
`py/abstraction/job/acceptance`. The Go decision helper validates trusted owner
evidence without owning storage or initiating work. The generated
[API reference](abstraction.job.acceptance.schema.html) describes the wire API.

Run the focused checks from the repository's `go` directory with
`go test ./abstraction/job/acceptance`, and from `python` with
`py -m unittest test_acceptance`. These exercise the semantic corpus and generated
protocol/codec behavior, not installed provider persistence. A standalone C++
codec driver is `cpp/test/test_acceptance_codec.cpp` (C++17, standard library only).
Set `OA_ACCEPTANCE_CPP` to its compiled executable when running the Python test
to check the same corpus and unknown-enum refusal against C++ as well.

### Service-owned admission provider

The Go `acceptanceprovider` package implements this interface using the existing
FileStore and CAS. The service calls `Open(privateRoot, logicalOwner)`, then
`Bind(authenticatedCallerScope)` with the subject after authenticating and
authorizing the caller.
Applications use the generated service client; they receive no storage path.
This provider admits records for existing workers and does not execute jobs.

Its versioned guarantees are `abstraction.job/caller-exit@1`,
`abstraction.job/service-restart@1` and `abstraction.job/reconciliation@1`.
These cover admission surviving caller/service-process exit and recovering the
same job. They do not promise power-loss durability or deduplication of
external engine effects. State must remain on a supported local filesystem under
service ownership. The minimum history retention is 24 hours; this implementation
retains history and negative seals indefinitely, with no epoch rotation or cleanup.
Published job records must not be removed independently of the admission journal.

Run `go test ./acceptanceprovider` from `go` for focused persistence and protocol
tests, including abrupt test-process exits at the recovery boundaries. Input
limits are exported constants; total retained history currently has no quota.

For runtime composition, `acceptanceprovider.HandleConnection` handles one existing
`listen.Conn` through the shared Program identity boundary and generated service
dispatcher. Supply an authorizer that maps proven peer evidence to an authorized,
restart-stable subject; nil/error authorization refuses without mutation. The
runtime retains listener and activation ownership. Configure shared clients with
`MaxFrameBytes` (2 MiB), which includes base64 expansion of the specification.

### Converting deprecated job store records

`openabstractions jobs migrate-legacy` converts deprecated job store records in
the managed runtime job root into service-owned records. Run it with no arguments for its
help, the mapping file format and exit codes.

    openabstractions jobs migrate-legacy inspect --template > mapping.json
    # fill in caller and request_key for every record
    openabstractions jobs migrate-legacy apply --mapping mapping.json

Every record must be terminal, mapped and unchanged since inspection, or nothing
is written. The command refuses while a runtime job service runs on that root.
Drain active deprecated jobs through jobd first. `inspect` also reports jobd's
separate store; those records stay with jobd and are finished through it.
`abandon` withdraws an interrupted apply.
