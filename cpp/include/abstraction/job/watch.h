#pragma once

// A live view of the jobs of one kind, and quiet once nothing visible has
// changed for the budget. The file binding has nothing to push, so it is
// asked, at most every kPollEvery.

#include <abstraction/job/store.h>
#include <abstraction/watch/watch.h>

#include <chrono>
#include <memory>
#include <optional>
#include <sstream>
#include <string>
#include <utility>
#include <vector>

namespace abstraction::job {

constexpr std::chrono::milliseconds kPollEvery{750};

struct Notice {
    std::vector<Record> records;
    bool quiet = false;
    std::chrono::milliseconds silence{0};
};

// What "changed" means: identity, state, progress, owner and error. Not
// updated_at -- a lease renewal moves it and nothing a person can see.
inline std::string fingerprint(const std::vector<Record>& records) {
    std::ostringstream o;
    for (const Record& r : records) {
        o << r.id << '|' << r.state << '|' << r.progress.done << '/' << r.progress.total << '|'
          << r.lease.owner << '|' << r.error << '\n';
    }
    return o.str();
}

class Subscription {
public:
    using Inner = abstraction::watch::Subscription<std::vector<Record>>;

    Subscription(const Store& store, std::string kind, std::chrono::milliseconds budget)
        : inner_(Inner::poll(
              [&store, kind]() {
                  std::vector<Record> mine;
                  for (Record& r : store.list()) {
                      if (r.kind == kind) mine.push_back(std::move(r));
                  }
                  std::string stamp = fingerprint(mine);
                  return std::make_pair(std::move(mine), std::move(stamp));
              },
              budget.count() > 0 ? std::min(kPollEvery, budget) : kPollEvery, budget)) {}

    std::vector<Record> records() const { return inner_->current(); }

    std::optional<Notice> next(std::optional<std::chrono::milliseconds> timeout = std::nullopt) {
        auto n = inner_->next(timeout);
        if (!n) return std::nullopt;
        return Notice{std::move(n->now), n->quiet, n->silence};
    }

    bool closed() const { return inner_->closed(); }
    void close() { inner_->close(); }

private:
    std::unique_ptr<Inner> inner_;
};

inline Subscription watch(const Store& store, std::string kind,
                          std::chrono::milliseconds budget = std::chrono::milliseconds{0}) {
    return Subscription(store, std::move(kind), budget);
}

}  // namespace abstraction::job
