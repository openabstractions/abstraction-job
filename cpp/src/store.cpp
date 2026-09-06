#include <abstraction/job/store.h>

#include <abstraction/cas.h>

#include <algorithm>
#include <filesystem>
#include <optional>
#include <random>
#include <system_error>
#include <utility>

namespace fs = std::filesystem;

namespace abstraction {
namespace job {

namespace {

fs::path path_of(const std::string& utf8) {
#if defined(_WIN32) && defined(__cpp_lib_char8_t)
    return fs::path(std::u8string(utf8.begin(), utf8.end()));
#elif defined(_WIN32)
    return fs::u8path(utf8);
#else
    return fs::path(utf8);
#endif
}

std::string utf8_of(const fs::path& p) {
    const auto s = p.u8string();
    return std::string(s.begin(), s.end());
}

std::string random_hex(int bytes) {
    std::random_device rd;
    std::string out;
    out.reserve(static_cast<std::size_t>(bytes) * 2);
    static const char* kHex = "0123456789abcdef";
    for (int i = 0; i < bytes; ++i) {
        const unsigned value = rd() & 0xFFu;
        out.push_back(kHex[(value >> 4) & 0xF]);
        out.push_back(kHex[value & 0xF]);
    }
    return out;
}

template <typename Fn>
auto on_disk(const std::string& what, Fn&& fn) -> decltype(fn()) {
    try {
        return fn();
    } catch (const std::system_error& e) {
        throw JobError(what + ": " + e.what());
    }
}

}  // namespace

std::string new_id() {
    const auto millis = std::chrono::duration_cast<std::chrono::milliseconds>(
                            Clock::now().time_since_epoch())
                            .count();
    return std::to_string(millis) + "-" + random_hex(10);
}

FileStore::FileStore(std::string root)
    : root_(std::move(root)), now_([]() { return Clock::now(); }) {
    std::error_code ec;
    for (const char* sub : {"jobs", "work"}) {
        fs::create_directories(path_of(root_) / sub, ec);
        if (ec) {
            throw JobError("cannot create job store under " + root_ + ": " + ec.message());
        }
    }
}

std::string FileStore::record_path(const std::string& id) const {
    return utf8_of(path_of(root_) / "jobs" / (id + ".json"));
}

std::string FileStore::work_path(const std::string& id) const {
    return utf8_of(path_of(root_) / "work" / id);
}

std::string FileStore::submit(Record r) {
    if (r.id.empty()) {
        r.id = new_id();
    }
    const TimePoint moment = now();
    r.describe();
    if (r.state.empty()) {
        r.state = state::kPending;
    }
    r.created_at = moment;
    r.updated_at = moment;
    r.progress.updated_at = moment;
    try {
        cas::write(path_of(record_path(r.id)), std::nullopt, r.encode());
    } catch (const cas::Moved&) {
        throw Invalid("job " + r.id + " already exists");
    } catch (const std::system_error& e) {
        throw JobError("cannot write " + record_path(r.id) + ": " + e.what());
    }
    return r.id;
}

Record FileStore::load(const std::string& id) const {
    const fs::path p = path_of(record_path(id));
    const cas::Value body = on_disk("cannot read " + utf8_of(p), [&] { return cas::read(p); });
    if (!body) {
        throw NotFound(id);
    }
    return Record::decode(*body);
}

Record FileStore::change(const std::string& id, const std::function<void(Record&)>& edit) const {
    const fs::path p = path_of(record_path(id));
    std::optional<Record> out;
    on_disk("cannot change " + utf8_of(p), [&] {
        cas::change(p, [&](const cas::Value& cur) {
            if (!cur) {
                throw NotFound(id);
            }
            Record r = Record::decode(*cur);
            edit(r);
            out = r;
            return r.encode();
        });
    });
    return *out;
}

std::vector<Record> FileStore::list() const {
    std::vector<std::string> ids;
    std::error_code ec;
    for (const auto& entry : fs::directory_iterator(path_of(root_) / "jobs", ec)) {
        const std::string name = utf8_of(entry.path().filename());
        if (name.size() > 5 && name.compare(name.size() - 5, 5, ".json") == 0) {
            ids.push_back(name.substr(0, name.size() - 5));
        }
    }
    std::sort(ids.begin(), ids.end());

    std::vector<Record> out;
    out.reserve(ids.size());
    for (const auto& id : ids) {
        try {
            out.push_back(load(id));
        } catch (const JobError&) {
            continue;
        }
    }
    return out;
}

bool FileStore::claimable(const Record& r) const {
    return !r.terminal() && !r.lease.held(now());
}

std::vector<Record> FileStore::orphans() const {
    std::vector<Record> out;
    for (auto& r : list()) {
        if (claimable(r) && r.stranded()) {
            out.push_back(std::move(r));
        }
    }
    return out;
}

Record FileStore::claim(const std::string& id, const std::string& owner,
                        std::chrono::milliseconds ttl) {
    return claim_from(load(id), owner, ttl);
}

Record FileStore::claim_from(Record seen, const std::string& owner,
                             std::chrono::milliseconds ttl) {
    if (owner.find_first_not_of(" \t\r\n") == std::string::npos) {
        throw JobError("claim requires an owner");
    }
    return change(seen.id, [&](Record& r) {
        if (r.lease.epoch != seen.lease.epoch) {
            throw LeaseHeld("the record moved to epoch " + std::to_string(r.lease.epoch) +
                            " since it was read at " + std::to_string(seen.lease.epoch));
        }
        if (r.terminal()) {
            throw TerminalState(r.id + " is " + r.state);
        }
        const TimePoint moment = now();
        if (r.lease.held(moment) && (r.lease.owner != owner || r.lease.recalled())) {
            throw LeaseHeld(r.lease.owner + " holds it until " + format_rfc3339(r.lease.expires_at));
        }
        r.lease.owner = owner;
        r.lease.epoch += 1;
        r.lease.expires_at = moment + std::chrono::duration_cast<TimePoint::duration>(ttl);
        r.lease.recall.reset();
        if (r.state == state::kPending || r.state == state::kRunning) {
            r.state = state::kRunning;
        }
        r.describe();
    });
}

Record FileStore::renew(const std::string& id, std::int64_t epoch, std::chrono::milliseconds ttl) {
    return change(id, [&](Record& r) {
        if (r.lease.epoch != epoch) {
            throw StaleEpoch("record is at epoch " + std::to_string(r.lease.epoch) +
                             ", caller holds " + std::to_string(epoch));
        }
        if (!r.lease.held(now())) {
            throw LeaseExpired("expired at " + format_rfc3339(r.lease.expires_at) +
                               ", re-claim instead");
        }
        // The recall's deadline caps a renewal; without the cap a holder keeps
        // its lease alive through any recall simply by renewing.
        TimePoint want = now() + std::chrono::duration_cast<TimePoint::duration>(ttl);
        if (r.lease.recall && r.lease.recall->until < want) {
            want = r.lease.recall->until;
        }
        r.lease.expires_at = want;
    });
}

Record FileStore::recall(const std::string& id, std::int64_t epoch, const std::string& reason,
                         const std::string& by, std::chrono::milliseconds grace) {
    if (reason.find_first_not_of(" \t\r\n") == std::string::npos) {
        throw Invalid("a recall needs a reason the holder can act on");
    }
    return change(id, [&](Record& r) {
        if (r.terminal()) {
            throw TerminalState(id + " is " + r.state);
        }
        if (r.lease.epoch != epoch) {
            throw StaleEpoch("record is at epoch " + std::to_string(r.lease.epoch) +
                             ", recall was decided against " + std::to_string(epoch));
        }
        const TimePoint moment = now();
        if (!r.lease.held(moment)) {
            throw LeaseExpired("nobody holds " + id);
        }
        Recall rc;
        rc.reason = reason;
        rc.by = by;
        rc.at = moment;
        rc.until = moment + std::chrono::duration_cast<TimePoint::duration>(grace);
        r.lease.recall = rc;
        if (rc.until < r.lease.expires_at) {
            r.lease.expires_at = rc.until;
        }
        r.updated_at = moment;
        r.describe();
    });
}

void FileStore::release(const std::string& id, std::int64_t epoch) {
    update(id, epoch, [this](Record& r) {
        r.lease.expires_at = now();
        r.lease.owner.clear();
        // A delegated job stays running: releasing means "no longer watching",
        // and demoting it to pending would invite a second tier to start it
        // again.
        if (r.state == state::kRunning && !r.delegated()) {
            r.state = state::kPending;
        }
    });
}

Record FileStore::update(const std::string& id, std::int64_t epoch,
                         const std::function<void(Record&)>& mutate) {
    return change(id, [&](Record& r) {
        if (r.terminal()) {
            throw TerminalState(id + " is " + r.state);
        }
        if (r.lease.epoch != epoch) {
            throw StaleEpoch("record is at epoch " + std::to_string(r.lease.epoch) +
                             ", caller holds " + std::to_string(epoch));
        }
        if (!r.lease.held(now())) {
            throw LeaseExpired("expired at " + format_rfc3339(r.lease.expires_at));
        }
        mutate(r);
        r.updated_at = now();
    });
}

Record FileStore::set_intent(const std::string& id, const std::string& want,
                             const std::string& by) {
    if (!is_valid_want(want)) {
        throw Invalid("intent \"" + want + "\"");
    }
    return change(id, [&](Record& r) {
        if (r.terminal()) {
            throw TerminalState(id + " is " + r.state);
        }
        Intent in;
        in.want = want;
        in.by = by;
        in.at = now();
        r.intent = in;
        r.updated_at = now();
    });
}

}  // namespace job
}  // namespace abstraction
