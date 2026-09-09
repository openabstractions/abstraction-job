// Which downloader this machine has, discovered rather than configured.
//
// The download layer moves bytes with libcurl, which stops when this process
// stops. On a machine where a system downloader is running, the same transfer
// can outlive Lemonade entirely — and on a machine where one is not, nothing
// should change.
//
// So this is a provider question, not a feature flag. The caller asks "is there
// a better downloader here?" and gets an answer derived from the machine, never
// from a setting a user had to find. Nothing in Lemonade names a NAS, a
// service, or a host: the supervisor decides where the work actually goes, one
// hop further down, and this layer does not know or care.
//
// Discovery has two steps and both are file reads:
//
//   1. The machine's store, from the location the OS designates for
//      configuration. Whatever set a system downloader up wrote it there once,
//      and every process afterwards reads it.
//   2. A heartbeat in that store. A supervisor refreshes it on every sweep, so
//      a timestamp -- not a pid -- is what proves one is alive now.
//
// A pid file would only prove a process existed when the file was written. The
// heartbeat also carries the interval it promises, so a reader can decide what
// stale means without guessing.

#pragma once

#include <cstdint>
#include <string>

namespace abstraction {
namespace job {

// Where the machine keeps job records that tools share. Empty when nothing has
// been configured, which is the ordinary case and not an error.
std::string machine_store();

// The same question a shell tool asks, which has no empty answer.
//
// "This machine has no store" is a real answer to machine_store: no store means
// no supervisor, and the caller downloads for itself. A person who has just
// typed a command needs a directory to open, so the default stands whether or
// not it exists yet.
std::string store_or_default();

// The environment, losslessly, because on Windows the narrow one is not.
//
// Exported so a tool reading its own variable does not reach for getenv and
// reintroduce the mapping this file measured: under Windows-1252,
// `C:\Reinīs-Modeļi` came back as `C:\Reinis-Modeli`, a valid path naming a
// different directory, with no error anywhere.
std::string env_utf8(const char* name);

// What a live supervisor looks like. Absent means this machine has none, and
// the caller downloads for itself exactly as it always did.
//
// Named for the file it reads, not for the process behind it, because
// `Supervisor` in this namespace is already the envelope's — what a caller has
// built, in Go, Python and record.h. Two structs of one name in one namespace
// is not a style question: abstraction_job stopped compiling the day the second
// arrived, and no gate on this machine had a C++ compiler to say so.
struct Heartbeat {
    std::string owner;  // program@host:pid, for a status line
    std::string tier;   // what IT delegates to, purely so a human can see the
                        // whole chain. Lemonade never acts on this.
    bool alive = false;
};

// Read the heartbeat and decide whether it is fresh. A supervisor that was
// killed leaves its heartbeat behind, so trusting the file's existence would
// hand work to a directory nobody is watching -- which looks exactly like a
// download that started and then never progressed.
Heartbeat supervisor_of(const std::string& store_root);

// Ask a supervisor to sweep now instead of at its next tick.
//
// A hint, not a message: it carries no job id and no payload. If it is lost,
// refused, or unix sockets do not work here, the supervisor's own sweep finds
// the work anyway and the only cost is the wait. That is what makes it safe to
// add -- a channel that carried state would be a second source of truth.
void nudge(const std::string& store_root);

}  // namespace job
}  // namespace abstraction
