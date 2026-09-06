// The machine stays out of idle sleep while this process holds the lease
// `claimed` carries, and not a moment longer: ended by release(), by the lease
// ending in the store — released, lapsed, or the job turning terminal — and by
// the operating system if the process dies. The lease is the lifetime; nothing
// here has one of its own.

#pragma once

#include <abstraction/job/store.h>
#include <abstraction/job/watch.h>

#include <cstdint>
#include <functional>
#include <memory>
#include <mutex>
#include <string>
#include <thread>

namespace abstraction::job {

class KeepAwake {
public:
    KeepAwake(Store& store, const Record& claimed);
    ~KeepAwake();
    KeepAwake(const KeepAwake&) = delete;
    KeepAwake& operator=(const KeepAwake&) = delete;

    bool held() const;
    void release();

private:
    bool alive(const Record* r) const;
    void follow();

    Store& store_;
    std::string id_;
    std::int64_t epoch_;
    TimePoint until_;
    mutable std::mutex mu_;
    std::function<void()> free_;
    std::unique_ptr<Subscription> sub_;
    std::thread follower_;
};

}  // namespace abstraction::job
