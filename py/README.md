# Python job protocol

This package installs generated acceptance, operation and inventory vocabulary.
Application code uses `abstraction.facade.client.Machine.resolve_jobs()` for the
validated facade binding, `resolve_job_operations()` when discovery must require
result/observation support, and `resolve_job_inventory()` for read-only listing.
All use shared native IPC. No Python provider or file-backed client is supplied.

Keep the explicit key, history epoch, logical owner, endpoint, complete required
guarantees and full request before submitting. GetHistoryWindow pins an owner;
Submit/Reconcile verify receipts against it. A lost reply is unresolved and must
be reconciled at the retained binding. Jobs.restore restores caller-retained
context without discovery. Acceptance and inventory are distinct contracts;
resolving inventory does not grant acceptance methods.

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

Install the coordinated identity, logging, config, job and facade `py/` packages
plus the native IPC library as described by the facade README. Package 0.0.0 is
source development metadata. Windows runtime evidence is in the private Python
jobs fixture; no published release or native macOS support is implied.
