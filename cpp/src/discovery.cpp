#include <abstraction/job/discovery.h>

#include <chrono>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <string>

#include <abstraction/job/record.h>

#ifdef _WIN32
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <winsock2.h>
#include <afunix.h>
#else
#include <sys/socket.h>
#include <sys/un.h>
#include <unistd.h>
#endif

namespace fs = std::filesystem;

namespace abstraction {
namespace job {

namespace {

// A path that a record spells in UTF-8, opened as the filesystem spells it.
//
// Not decoration on Windows: fs::path built from a narrow string is read in the
// active code page, so a store root naming a directory in any non-ASCII script
// opens the wrong one or none at all — and the root is UTF-8 because it arrives
// from config.json, which three languages have to read.
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

// Every std::string in this file is UTF-8, so the environment has to arrive that
// way too. getenv hands back the Windows environment in the active code page,
// and the CRT best-fit-maps what will not fit: measured under Windows-1252,
// `C:\Reinīs-Modeļi` came back as `C:\Reinis-Modeli`, which is a valid path
// naming a different directory, with no error anywhere. The wide environment is
// the only lossless one.
std::string env(const char* name) {
#ifdef _WIN32
    const std::wstring wide(name, name + std::char_traits<char>::length(name));
    const wchar_t* v = ::_wgetenv(wide.c_str());
    return v ? utf8_of(fs::path(std::wstring(v))) : std::string();
#else
    const char* v = std::getenv(name);
    return v ? std::string(v) : std::string();
#endif
}

// The per-user configuration location each OS already designates. Inventing a
// dotfile somewhere else is the habit this project keeps complaining about in
// other tools.
fs::path user_config_dir() {
#ifdef _WIN32
    const std::string appdata = env("APPDATA");
    if (!appdata.empty()) return path_of(appdata);
#elif defined(__APPLE__)
    const std::string home = env("HOME");
    if (!home.empty()) return path_of(home) / "Library" / "Application Support";
#else
    const std::string xdg = env("XDG_CONFIG_HOME");
    if (!xdg.empty()) return path_of(xdg);
    const std::string home = env("HOME");
    if (!home.empty()) return path_of(home) / ".config";
#endif
    return {};
}

Json read_json(const fs::path& p) {
    std::ifstream in(p, std::ios::binary);
    if (!in) return {};
    std::ostringstream body;
    body << in.rdbuf();
    try {
        return Json::parse(body.str());
    } catch (const std::exception&) {
        return {};  // a malformed config must never take the server down
    }
}

std::string json_string(const Json& j, const char* key) {
    if (!j.is_object() || !j.contains(key) || !j[key].is_string()) return {};
    return j[key].get<std::string>();
}

// "30s", "5m", "1m30s" -> seconds. Zero when it cannot be read, which the
// caller turns into a conservative default rather than treating as instant.
double parse_duration(const std::string& s) {
    double total = 0, value = 0;
    std::string digits;
    for (std::size_t i = 0; i < s.size(); ++i) {
        if (std::isdigit(static_cast<unsigned char>(s[i])) || s[i] == '.') {
            digits.push_back(s[i]);
            continue;
        }
        if (digits.empty()) continue;
        value = std::atof(digits.c_str());
        digits.clear();
        switch (s[i]) {
            case 'h': total += value * 3600; break;
            case 'm': total += (i + 1 < s.size() && s[i + 1] == 's') ? value / 1000 : value * 60; break;
            case 's': total += value; break;
            default: break;
        }
    }
    return total;
}

}  // namespace

std::string env_utf8(const char* name) { return env(name); }

std::string machine_store() {
    // An environment override first, so a test or a container can redirect one
    // run without editing a file other processes are reading.
    const std::string from_env = env("ABSTRACTION_STORE");
    if (!from_env.empty()) return from_env;

    const fs::path dir = user_config_dir();
    if (dir.empty()) return {};

    const Json cfg = read_json(dir / "abstraction" / "config.json");
    const std::string store = json_string(cfg, "store");
    if (!store.empty()) return store;

    // Nothing configured. The default is where a supervisor would put one, so a
    // machine with jobd running and no config file still works.
    const std::string home = env(
#ifdef _WIN32
        "USERPROFILE"
#else
        "HOME"
#endif
    );
    if (home.empty()) return {};
    const fs::path fallback = path_of(home) / ".abstraction";
    return fs::exists(fallback) ? utf8_of(fallback) : std::string();
}

std::string store_or_default() {
    const std::string configured = machine_store();
    if (!configured.empty()) return configured;
    const std::string home = env(
#ifdef _WIN32
        "USERPROFILE"
#else
        "HOME"
#endif
    );
    if (home.empty()) return {};
    return utf8_of(path_of(home) / ".abstraction");
}

Heartbeat supervisor_of(const std::string& store_root) {
    Heartbeat out;
    if (store_root.empty()) return out;

    const Json hb = read_json(path_of(store_root) / "supervisor.json");
    if (!hb.is_object()) return out;

    out.owner = json_string(hb, "owner");
    out.tier = json_string(hb, "tier");

    const std::string seen = json_string(hb, "seen");
    if (seen.empty()) return out;

    double every = parse_duration(json_string(hb, "every"));
    if (every <= 0) every = 30.0;

    // Three intervals: enough to survive a slow sweep or a jittering clock,
    // short enough that a killed supervisor stops attracting work in a minute
    // or two rather than forever.
    const auto age = Clock::now() - parse_rfc3339(seen);
    const double seconds = std::chrono::duration<double>(age).count();
    out.alive = seconds >= 0 && seconds <= 3 * every;
    return out;
}

void nudge(const std::string& store_root) {
    if (store_root.empty()) return;
    const std::string path = utf8_of(path_of(store_root) / "supervisor.sock");
    if (path.size() >= sizeof(sockaddr_un{}.sun_path)) return;

#ifdef _WIN32
    WSADATA wsa;
    if (::WSAStartup(MAKEWORD(2, 2), &wsa) != 0) return;
    SOCKET fd = ::socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd == INVALID_SOCKET) {
        ::WSACleanup();
        return;
    }
#else
    const int fd = ::socket(AF_UNIX, SOCK_STREAM, 0);
    if (fd < 0) return;
#endif

    sockaddr_un addr{};
    addr.sun_family = AF_UNIX;
    std::snprintf(addr.sun_path, sizeof(addr.sun_path), "%s", path.c_str());

    // Every failure here is fine. Nobody listening, a stale socket, a platform
    // that will not do this: the supervisor sweeps on its own timer regardless.
    if (::connect(fd, reinterpret_cast<sockaddr*>(&addr), sizeof(addr)) == 0) {
        const char msg[] = "look\n";
        ::send(fd, msg, static_cast<int>(sizeof(msg) - 1), 0);
    }

#ifdef _WIN32
    ::closesocket(fd);
    ::WSACleanup();
#else
    ::close(fd);
#endif
}

}  // namespace job
}  // namespace abstraction
