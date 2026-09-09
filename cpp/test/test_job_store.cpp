#include <abstraction/job/store.h>

#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <random>
#include <string>
#include <thread>

namespace fs = std::filesystem;
using namespace abstraction::job;

static int failures = 0;

static void check(bool cond, const std::string& what, const std::string& why = "") {
    std::printf("  %s  %s\n", cond ? "PASS" : "FAIL", what.c_str());
    if (!cond) {
        std::printf("        %s\n", why.c_str());
        ++failures;
    }
}

static std::string utf8_of(const fs::path& p) {
    const auto s = p.u8string();
    return std::string(s.begin(), s.end());
}

static int role(const std::string& name, const fs::path& path, int n) {
    try {
        FileStore store(utf8_of(path.parent_path().parent_path()));
        const std::string id = utf8_of(path.stem());
        const std::int64_t epoch = store.load(id).lease.epoch;
        if (name == "writer") {
            for (int i = 0; i < n; ++i) {
                store.update(id, epoch, [](Record& r) { r.progress.done += 1; });
                store.renew(id, epoch, std::chrono::hours(1));
            }
        } else if (name == "reader") {
            for (std::int64_t last = 0; last < n;) {
                const std::int64_t done = store.load(id).progress.done;
                if (done < last) {
                    std::fprintf(stderr, "progress went backwards: %lld after %lld\n",
                                 static_cast<long long>(done), static_cast<long long>(last));
                    return 3;
                }
                last = done;
            }
        }
        return 0;
    } catch (const std::exception& e) {
        std::fprintf(stderr, "%s: %s\n", name.c_str(), e.what());
        return 2;
    }
}

static void a_renewing_owner_does_not_overwrite_progress_or_a_pause() {
    static std::random_device seed;
    const fs::path root = fs::temp_directory_path() / ("abstraction-job-store-" + std::to_string(seed()));
    FileStore store(utf8_of(root));
    Record r;
    r.kind = "test";
    const std::string id = store.submit(std::move(r));
    const std::int64_t epoch = store.claim(id, "owner", std::chrono::minutes(1)).lease.epoch;

    std::atomic<bool> stop{false};
    std::thread renewing([&] {
        while (!stop) {
            store.renew(id, epoch, std::chrono::minutes(1));
        }
    });
    const int steps = 50;
    for (int n = 1; n <= steps; ++n) {
        store.update(id, epoch, [n](Record& rec) { rec.progress.done = n; });
    }
    store.set_intent(id, want::kPause, "ui");
    stop = true;
    renewing.join();

    const Record after = store.load(id);
    check(after.progress.done == steps, "a renewal does not put the checkpoint back",
          std::to_string(after.progress.done) + " of " + std::to_string(steps));
    check(after.wants() == want::kPause, "a renewal does not erase the pause");
    int entries = 0;
    for (const auto& e : fs::directory_iterator(root / "jobs")) {
        (void)e;
        ++entries;
    }
    check(entries == 2, "the record and its lock are all that is left", std::to_string(entries) + " entries");
    fs::remove_all(root);
}

int main() {
    if (const char* name = std::getenv("JOB_ROLE")) {
        return role(name, fs::path(std::getenv("JOB_PATH")), std::atoi(std::getenv("JOB_N")));
    }
    std::setvbuf(stdout, nullptr, _IONBF, 0);
    std::printf("job store\n");
    a_renewing_owner_does_not_overwrite_progress_or_a_pause();
    std::printf("%s: %d failure(s)\n", failures == 0 ? "OK" : "FAILED", failures);
    return failures == 0 ? 0 : 1;
}
