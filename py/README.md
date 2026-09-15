# Python job protocol

This package installs generated acceptance, operation and inventory vocabulary.
Application code uses `abstraction.facade.client.Machine.resolve_jobs()` for the
validated facade binding, `resolve_job_operations()` when discovery must require
result/observation support, and `resolve_job_inventory()` for read-only listing.
All use shared native IPC. No Python provider or file-backed client is supplied.

Keep the explicit key, history epoch, logical owner, endpoint, complete required
guarantees and full request before submitting. GetHistoryWindow pins an owner;
Submit/Reconcile verify receipts against it. A lost reply is unresolved and must
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
stable absolute path. Before `Submit`, persist the identity, the complete
submission, the endpoint, the required guarantees and the logical owner outside
the installation directory. After reinstall,
`Jobs.restore_installed(endpoint, owner, required_guarantees=...)` restores the
binding with installed-runtime trust; reconcile the saved identity there. The
runtime has no transfer of work between program scopes.

Configurable timeouts and absolute monotonic deadlines bound waiting. A binding's
with_waiting() supplies a fresh deadline/cancellation policy while sharing its
fixed owner and endpoint. Cancellation stops waiting only. CancelWork explicitly
requests work cancellation; its acknowledgment does not prove effects stopped.

ReadResult accepts offsets and 1..65536-byte limits. CopyResult keeps one chunk
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
