#pragma once

#include <chrono>
#include <cstdint>
#include <functional>
#include <abstraction/job/record.h>
#include <string>
#include <vector>

namespace abstraction {
namespace job {

// An identifier that sorts by creation time, so a directory listing comes back
// in roughly submission order without anything having to record that.
std::string new_id();

// Store is where jobs live: the lease protocol and nothing else. A claim is
// exclusive, an epoch only ever increases, every write presents the epoch it
// holds, and a successor continues only from what a predecessor proved. Not one
// clause of that mentions a byte, a file or a directory, which is what lets a
// service binding replace the file one without a caller noticing. C++ has one
// implementation today, FileStore.
//
// A single-process job engine — named steps, a cursor, a pause button — is not
// a Store and cannot be adapted into one: without a lease it has nothing to be
// exclusive against a supervisor on another machine. It sits above this and
// submits here; its pause and resume are set_intent.
class Store {
public:
    virtual ~Store() = default;

    virtual std::string submit(Record r) = 0;
    virtual Record load(const std::string& id) const = 0;
    virtual std::vector<Record> list() const = 0;
    virtual bool claimable(const Record& r) const = 0;
    virtual std::vector<Record> orphans() const = 0;
    virtual Record claim(const std::string& id, const std::string& owner,
                         std::chrono::milliseconds ttl) = 0;
    virtual Record renew(const std::string& id, std::int64_t epoch,
                         std::chrono::milliseconds ttl) = 0;
    virtual void release(const std::string& id, std::int64_t epoch) = 0;
    virtual Record update(const std::string& id, std::int64_t epoch,
                          const std::function<void(Record&)>& mutate) = 0;

    // Says what should happen, WITHOUT holding the lease. The only write that
    // presents no epoch: whoever wants a job stopped is not the process doing
    // it — a person clicking cancel in this server's UI while a supervisor on
    // another machine moves the bytes — and requiring a lease would mean
    // stealing the job in order to stop it.
    virtual Record set_intent(const std::string& id, const std::string& want,
                              const std::string& by) = 0;

    // Asks the holder at `epoch` — the epoch the caller OBSERVED, not one it
    // holds — for the lease back by now+grace, for a reason it can act on. The
    // lease lapses at that deadline either way: that is the fallback, and what
    // makes this a demand rather than a suggestion. Not an intent.
    virtual Record recall(const std::string& id, std::int64_t epoch, const std::string& reason,
                          const std::string& by, std::chrono::milliseconds grace) = 0;
};

// LocalStore is an OPTIONAL capability: a store whose binding happens to be a
// filesystem can offer an area on it.
//
// Separate from Store on purpose. A store backed by this server's own engine
// answers no, and a caller then has to have a real answer for that rather than
// assuming a directory exists. Reaching through a concrete store for its root
// is what let a binding name itself all the way up in the Go implementation.
class LocalStore {
public:
    virtual ~LocalStore() = default;
    virtual const std::string& root() const = 0;
    virtual std::string work_path(const std::string& id) const = 0;
};

// FileStore is the FILE BINDING of Store: one record per job under
// <root>/jobs/<id>.json, every write a cas change on that file, so writers in
// any process on this host apply their edit to the truth. <root>/work/<id> is
// the name job <id> may spend on scratch. The layout is normative for anything
// sharing the directory and is written in job/README.md; nothing written
// against Store may depend on it.
class FileStore : public Store, public LocalStore {
public:
    explicit FileStore(std::string root);

    const std::string& root() const override { return root_; }

    // Scratch space a job may use while it runs. What goes there is the kind's
    // business; the store only guarantees the path is derived from the id, so a
    // successor can find what a predecessor left.
    std::string work_path(const std::string& id) const override;

    // Records a new job and returns its id. The id is the handle: a plain
    // string that can be written to a config file or handed to another program,
    // and the process that submitted the job need not be alive for it to remain
    // meaningful.
    std::string submit(Record r) override;

    // Never takes the lock. Any process may do this, including one that holds
    // no lease and never will; that is what makes progress observable from
    // outside.
    Record load(const std::string& id) const override;

    std::vector<Record> list() const override;

    bool claimable(const Record& r) const override;

    // The jobs nobody is working on. This is the primary reclamation path, not
    // a fallback: a process that is killed, or a machine that loses power,
    // never gets to hand anything over.
    std::vector<Record> orphans() const override;

    // Takes ownership for ttl and returns the record carrying the caller's new
    // epoch. Every later write must present that epoch.
    Record claim(const std::string& id, const std::string& owner,
                 std::chrono::milliseconds ttl) override;

    // Takes ownership of the job AS THE CALLER LAST READ IT: refused with
    // LeaseHeld if the job changed hands since, so a sweeper that already holds
    // a record from `orphans` is told rather than quietly claiming a job that is
    // no longer the one it decided about. The record written is the one on disk
    // under the lock, never the caller's copy, so an intent set since the
    // caller read is kept.
    Record claim_from(Record seen, const std::string& owner, std::chrono::milliseconds ttl);

    // Extends a lease the caller still holds. Refuses once expired even when
    // the epoch still matches and nobody else has claimed: a process suspended
    // for an hour wakes believing it is still the owner, and forcing it to
    // re-claim bumps the epoch so anything it had in flight is refused rather
    // than landing on top of work a different owner may since have done.
    Record renew(const std::string& id, std::int64_t epoch,
                 std::chrono::milliseconds ttl) override;

    // Gives up a lease early so the job can be taken immediately. A courtesy:
    // everything still works without it, just more slowly. It is an update, so
    // it is refused on a finished job — there is nothing left to hand over, and
    // the lease lapses on its own.
    void release(const std::string& id, std::int64_t epoch) override;

    // The single gate every change passes through, so staleness is checked in
    // exactly one place instead of once per call site — and the one place a
    // finished job is protected from being written over.
    //
    // Terminal is judged on the record as LOADED, never on what mutate leaves
    // behind, and that is how a job ever finishes: the write that MAKES a record
    // complete, failed or cancelled lands, and the write after it does not. An
    // owner therefore puts everything it wants recorded — the error text, the
    // last checkpoint — into the same update as the final state, because nothing
    // of its epoch may write again.
    Record update(const std::string& id, std::int64_t epoch,
                  const std::function<void(Record&)>& mutate) override;

    // Says what should happen, WITHOUT holding the lease.
    //
    // The only write here that presents no epoch, and deliberately so: whoever
    // wants a job stopped is not the process doing it — a person clicks cancel
    // in this server's UI while a supervisor on another machine moves the bytes
    // — and requiring a lease would mean stealing the job in order to stop it,
    // which is the one thing the lease exists to prevent.
    //
    // Idempotent, and refused once the job is terminal, because nothing reopens
    // finished work. Asking for something the current owner cannot do is NOT an
    // error here: only the owner knows what it can do.
    Record set_intent(const std::string& id, const std::string& want,
                      const std::string& by) override;

    Record recall(const std::string& id, std::int64_t epoch, const std::string& reason,
                  const std::string& by, std::chrono::milliseconds grace) override;

    // Injectable so lease tests are neither slow nor flaky.
    void set_clock(std::function<TimePoint()> now) { now_ = std::move(now); }

private:
    std::string record_path(const std::string& id) const;
    Record change(const std::string& id, const std::function<void(Record&)>& edit) const;
    TimePoint now() const { return now_(); }

    std::string root_;
    std::function<TimePoint()> now_;
};

}  // namespace job
}  // namespace abstraction
