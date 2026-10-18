# Python job protocol

*For Python applications.*

The same task as [the Go example](../README.md#an-example-that-runs): submit a
download job, show its progress and save the result to a path.

```python
import hashlib
import sys
import time

import abstraction.download.request as request
from abstraction.facade.client import Machine
from abstraction.job.acceptance import RequestIdentity, Submission

url, path = sys.argv[1], sys.argv[2]  # an https URL, and where to save it
# copy_result runs the whole copy under this deadline; a large file needs a long one.
jobs = Machine(deadline=time.monotonic() + 3600).resolve_jobs()
window = jobs.get_history_window()
key = "fetch-" + hashlib.sha256(url.encode()).hexdigest()  # under the 1 KiB key limit
identity = RequestIdentity(key=key, history_epoch=window.history_epoch)
spec = request.encode(request.Request(sources=[request.Source(scheme="https", locator=url)]))
accepted = jobs.submit(Submission(identity=identity, kind="download", spec=spec))
if accepted.outcome != "accepted":
    raise SystemExit(f"not accepted: {accepted.outcome} {accepted.reason}")

while True:  # progress until the job ends
    observed = jobs.observe_work(identity)
    if observed.outcome != "observed":
        raise SystemExit(f"observe: {observed.outcome}")
    job = observed.snapshot
    print(f"\r{job.progress.done} of {job.progress.total} bytes", end="")
    if job.state == "complete":
        break
    if job.state in ("failed", "cancelled"):
        raise SystemExit(f"\ndownload {job.state}")
    time.sleep(1)

# The Python facade has no validated link_result yet (only the raw
# generated call exists), so this copies the bytes.
with open(path, "wb") as file:
    jobs.copy_result(identity, file)
print("\nsaved", path)
```

This package installs generated acceptance, operation and inventory vocabulary.
Application code uses `abstraction.facade.client.Machine.resolve_jobs()` for the
validated facade binding, `resolve_job_operations()` when discovery must require
result/observation support, and `resolve_job_inventory()` for read-only listing.
All use shared native IPC. No Python provider or file-backed client is supplied.

Keep the explicit key, history epoch, logical owner, endpoint, complete required
guarantees and full request before submitting. get_history_window pins an owner;
submit/reconcile verify receipts against it. A lost reply is unresolved and must
be reconciled at the retained binding. Jobs.restore_installed restores caller-retained context using independent
installed-runtime trust. For an explicitly configured host, Jobs.restore accepts
an independent server expectation. Retain that expectation on recovery. Acceptance and inventory are distinct contracts;
resolving inventory does not grant acceptance methods.

## Who owns a job

The runtime files each accepted submission under a caller scope: the
authenticated account and the absolute path of the calling executable as the
operating system reports it. For Python that executable is the interpreter. On
Linux the path is `/proc/<pid>/exe` with symlinks resolved, and a virtual
environment's `python` is commonly a symlink to the base interpreter. Every
Python application of one account on the same interpreter shares a scope;
upgrading or moving that interpreter creates a new one. Identity keys, receipts,
observation and result bytes are visible only inside the scope. Restarts,
reboots and runtime upgrades keep it. Another executable in the same account has
its own scope. Reconciling an identity the caller's scope never accepted returns
`definitely_not_accepted` and seals that identity for the caller.

For continuity across reinstall, run the application on an interpreter at a
stable absolute path. Before `submit`, persist the identity, the complete
submission, the endpoint, the required guarantees and the logical owner outside
the installation directory. After reinstall,
`Jobs.restore_installed(endpoint, owner, required_guarantees=...)` restores the
binding with installed-runtime trust; reconcile the saved identity there. The
runtime has no transfer of work between program scopes.

Configurable timeouts and absolute monotonic deadlines bound waiting. A binding's
with_waiting() supplies a fresh deadline/cancellation policy while sharing its
fixed owner and endpoint. Cancellation stops waiting only. cancel_work explicitly
requests work cancellation; its acknowledgment does not prove effects stopped.

read_result accepts offsets and 1..65536-byte limits. copy_result keeps one chunk
at a time, pins total and operation ID across chunks, and returns written bytes.
On failure it raises ResultCopyError with confirmed and cause. A writer exception
may have written an unreported prefix of its current call; confirmed then counts
prior successful writes. Non-data outcomes remain typed errors without polling,
retry or provider switching. Unknown acceptance remains an explicit outcome.

Inventory returns at most 64 snapshots per call. Retain its opaque cursor,
including across empty scan pages. A gap requires explicit restart. Mutable
inventories can change between pages; callers deduplicate if assembling a view.
New inventory guarantees are not imposed on previously accepted receipts.

Install the coordinated identity, job and facade `py/` packages
plus the native IPC library as described by the facade README. Package 0.0.0 is
source development metadata. Windows runtime evidence is in the private Python
jobs fixture; no published release or native macOS support is implied.
