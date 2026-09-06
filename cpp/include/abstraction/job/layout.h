// Which relative paths under a store root belong to the store itself.
//
// A layer above this one — download — lets a caller name where bytes land, and
// resolves a relative destination against the store root. Containment was added
// so such a path could not climb OUT of the root. It could still aim at the
// root's own contents: a final of `jobs/<id>.json` overwrites a job record, and
// a final of `work/<other>` overwrites another job's partial. Both are inside
// the root and both passed every containment check.
//
// The layout is the file binding's, so the answer is the file binding's to
// give. The alternative was download spelling "jobs" and "work" itself, which
// is exactly the coupling the opaque spec exists to prevent.
//
// Header-only and dependency-free: it is a few string rules, and the download
// header — which is also header-only — includes it.

#ifndef ABSTRACTION_JOB_LAYOUT_H
#define ABSTRACTION_JOB_LAYOUT_H

#include <cctype>
#include <string>
#include <vector>

namespace abstraction {
namespace job {

// kRegistryFileName is the discovery registry, which sits at the root beside
// jobs/ and work/ rather than inside either. See discovery_client.cpp.
inline const char* registry_file_name() { return "services.json"; }

// One segment in the spelling a filesystem would give it.
//
// Windows drops a trailing dot or space from a name, so `jobs.` opens `jobs`.
// Dropping it here too keeps the answer the same on both platforms, which is
// the property that matters: a record refused by one and accepted by the other
// would mean the refusal depends on who looked. A name that is NOTHING but dots
// and spaces is left alone — it is a real name on POSIX, and not one Windows
// will open at all.
inline std::string fold_segment(const std::string& seg) {
    std::size_t begin = seg.find_first_not_of(" \t\n\r\f\v");
    if (begin == std::string::npos) {
        return "";
    }
    std::size_t end = seg.find_last_not_of(" \t\n\r\f\v");
    std::string trimmed = seg.substr(begin, end - begin + 1);

    std::size_t cut = trimmed.find_last_not_of(". ");
    if (cut != std::string::npos) {
        trimmed = trimmed.substr(0, cut + 1);
    }
    for (char& c : trimmed) {
        c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
    }
    return trimmed;
}

// A relative path reduced to the segments a store would see, in the one
// spelling two of them can be compared in.
//
// A path that climbs above the root returns nothing: that is containment's
// question, already answered before this one is asked, and answering it again
// here with a different rule would give two verdicts on one record.
inline std::vector<std::string> store_segments(const std::string& rel) {
    std::vector<std::string> out;
    std::string seg;
    bool climbed_out = false;
    const auto take = [&]() {
        if (seg.empty() || seg == ".") {
            // nothing to keep
        } else if (seg == "..") {
            if (out.empty()) {
                climbed_out = true;
            } else {
                out.pop_back();
            }
        } else {
            out.push_back(fold_segment(seg));
        }
        seg.clear();
    };
    for (char c : rel) {
        if (c == '/' || c == '\\') {
            take();
        } else {
            seg.push_back(c);
        }
    }
    take();
    if (climbed_out) {
        return {};
    }
    return out;
}

// Does rel — a path relative to the store root — name part of the file
// binding's own layout rather than free space inside it?
//
// owner is the job asking. Its own scratch, work/<owner> and everything under
// it, is not reserved against that job and is reserved against every other, so
// a download can still put its partial where the store told it to. An empty
// owner asks on behalf of no job, which reserves the whole of work/.
//
// Case is folded on every platform, deliberately unlike the containment
// comparison in the download header, which folds case only where the filesystem
// does. Containment compares two paths on THIS machine; this compares a
// record's path against names the contract fixed, and `Jobs/x.json` does land
// in `jobs/` on NTFS.
inline bool reserved(const std::string& owner, const std::string& rel) {
    const std::vector<std::string> segs = store_segments(rel);
    if (segs.empty()) {
        return false;
    }
    if (segs[0] == "jobs") {
        return true;
    }
    if (segs[0] == "work") {
        return segs.size() < 2 || segs[1] != fold_segment(owner);
    }
    return segs[0] == registry_file_name() && segs.size() == 1;
}

// The name rel takes in the store root, or "" if it names something deeper,
// climbs out of the root, or is empty.
//
// The root is a shared namespace and this binding is not its only occupant: the
// download layer keeps a supervisor heartbeat and a nudge socket beside jobs/
// and work/. A layer that puts a file there has to be able to ask whether a
// path aims at it, and to ask in the same spelling, or the two of them protect
// different sets of names.
inline std::string root_name(const std::string& rel) {
    const std::vector<std::string> segs = store_segments(rel);
    return segs.size() == 1 ? segs[0] : std::string();
}

}  // namespace job
}  // namespace abstraction

#endif  // ABSTRACTION_JOB_LAYOUT_H
