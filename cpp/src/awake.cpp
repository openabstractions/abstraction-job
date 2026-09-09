#include <abstraction/job/awake.h>

#include <algorithm>
#include <chrono>
#include <vector>

#ifdef _WIN32
#define NOMINMAX
#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#else
#include <fcntl.h>
#include <spawn.h>
#include <sys/wait.h>
#include <unistd.h>
extern char** environ;
#endif

namespace abstraction::job {

namespace {

#ifdef _WIN32

std::wstring wide(const std::string& s) {
    if (s.empty()) return {};
    const int n = MultiByteToWideChar(CP_UTF8, 0, s.data(), static_cast<int>(s.size()), nullptr, 0);
    std::wstring out(static_cast<std::size_t>(n), L'\0');
    MultiByteToWideChar(CP_UTF8, 0, s.data(), static_cast<int>(s.size()), out.data(), n);
    return out;
}

std::function<void()> keep_awake(const std::string& who, const std::string& why) {
    std::wstring text = wide(who + ": " + why);
    REASON_CONTEXT ctx{};
    ctx.Version = POWER_REQUEST_CONTEXT_VERSION;
    ctx.Flags = POWER_REQUEST_CONTEXT_SIMPLE_STRING;
    ctx.Reason.SimpleReasonString = text.data();
    HANDLE h = PowerCreateRequest(&ctx);
    if (h == INVALID_HANDLE_VALUE) return {};
    if (!PowerSetRequest(h, PowerRequestSystemRequired)) {
        CloseHandle(h);
        return {};
    }
    return [h] {
        PowerClearRequest(h, PowerRequestSystemRequired);
        CloseHandle(h);
    };
}

#else

// The inhibitor is a child that lives exactly as long as its stdin: our end of
// the pipe closes when this process exits, however it exits, and cat goes with
// it. That is the lifetime a D-Bus inhibitor fd has, without a D-Bus client.
// The echo arrives once the inhibitor is in place, so the hold is real on return.
std::function<void()> keep_awake(const std::string& who, const std::string& why) {
#ifdef __APPLE__
    std::vector<std::string> args{"caffeinate", "-i", "sh", "-c", "echo; exec cat"};
#else
    std::vector<std::string> args{"systemd-inhibit", "--what=idle:sleep", "--mode=block",
                                  "--who=" + who, "--why=" + why, "sh", "-c", "echo; exec cat"};
#endif
    std::vector<char*> argv;
    for (std::string& a : args) argv.push_back(a.data());
    argv.push_back(nullptr);

    int in[2];
    int out[2];
    if (pipe(in) != 0) return {};
    if (pipe(out) != 0) {
        close(in[0]);
        close(in[1]);
        return {};
    }
    posix_spawn_file_actions_t actions;
    posix_spawn_file_actions_init(&actions);
    posix_spawn_file_actions_adddup2(&actions, in[0], 0);
    posix_spawn_file_actions_adddup2(&actions, out[1], 1);
    posix_spawn_file_actions_addopen(&actions, 2, "/dev/null", O_WRONLY, 0);
    for (int fd : {in[0], in[1], out[0], out[1]}) posix_spawn_file_actions_addclose(&actions, fd);
    pid_t pid = 0;
    const int rc = posix_spawnp(&pid, argv[0], &actions, nullptr, argv.data(), environ);
    posix_spawn_file_actions_destroy(&actions);
    close(in[0]);
    close(out[1]);
    const int ours = in[1];
    const auto stop = [ours, pid] {
        close(ours);
        int status = 0;
        waitpid(pid, &status, 0);
    };
    char ready = 0;
    const bool held = rc == 0 && read(out[0], &ready, 1) == 1;
    close(out[0]);
    if (!held) {
        if (rc == 0) stop();
        else close(ours);
        return {};
    }
    return stop;
}

#endif

}  // namespace

KeepAwake::KeepAwake(Store& store, const Record& claimed)
    : store_(store), id_(claimed.id), epoch_(claimed.lease.epoch), until_(claimed.lease.expires_at) {
    if (!alive(&claimed)) return;
    free_ = keep_awake(claimed.lease.owner, claimed.kind + " " + claimed.id);
    if (!free_) return;
    sub_ = std::make_unique<Subscription>(watch(store, claimed.kind));
    follower_ = std::thread([this] { follow(); });
}

KeepAwake::~KeepAwake() {
    release();
    if (follower_.joinable()) follower_.join();
}

bool KeepAwake::held() const {
    std::lock_guard<std::mutex> lock(mu_);
    return static_cast<bool>(free_);
}

void KeepAwake::release() {
    std::function<void()> free;
    Subscription* sub = nullptr;
    {
        std::lock_guard<std::mutex> lock(mu_);
        free.swap(free_);
        sub = sub_.get();
    }
    if (sub) sub->close();
    if (free) free();
}

void KeepAwake::follow() {
    while (true) {
        const auto left = std::chrono::duration_cast<std::chrono::milliseconds>(until_ - Clock::now());
        auto n = sub_->next(std::max(left, std::chrono::milliseconds{0}));
        if (!n && sub_->closed()) return;
        std::optional<Record> r;
        if (n) {
            for (Record& candidate : n->records) {
                if (candidate.id == id_) r = std::move(candidate);
            }
        } else {
            try {
                r = store_.load(id_);
            } catch (const JobError&) {
            }
        }
        if (!alive(r ? &*r : nullptr)) {
            release();
            return;
        }
        until_ = r->lease.expires_at;
    }
}

bool KeepAwake::alive(const Record* r) const {
    return r != nullptr && r->lease.epoch == epoch_ && r->lease.held(Clock::now()) && !r->terminal();
}

}  // namespace abstraction::job
