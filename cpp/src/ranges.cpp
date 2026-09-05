#include <abstraction/job/ranges.h>

#include <algorithm>
#include <string>
#include <vector>

namespace abstraction {
namespace job {

namespace {

// A JSON number is only a byte offset if it is a whole number that fits. A
// double that happens to look like one does not qualify: nlohmann will hand
// back 4194304.0 for a value some other writer emitted as a float, and
// accepting it would let a resume point arrive with a fractional part.
std::int64_t as_offset(const Json& v, const char* what) {
    if (!v.is_number_integer()) {
        throw Invalid(std::string(what) + " is a whole number of bytes, got " + v.dump());
    }
    return v.get<std::int64_t>();
}

}  // namespace

Ranges canonical_ranges(const Ranges& in) {
    Ranges kept;
    kept.reserve(in.size());
    for (const Range& r : in) {
        if (r.start < 0 || r.end < 0) {
            throw Invalid("a byte offset cannot be negative: [" + std::to_string(r.start) + "," +
                          std::to_string(r.end) + ")");
        }
        if (r.end < r.start) {
            throw Invalid("a range ends before it starts: [" + std::to_string(r.start) + "," +
                          std::to_string(r.end) + ")");
        }
        // An empty range is not an error — a fetcher that recorded a
        // zero-length part is not lying, it has just proven nothing — but it
        // carries no information, and two sets differing only by one are the
        // same set.
        if (r.end == r.start) {
            continue;
        }
        kept.push_back(r);
    }
    std::sort(kept.begin(), kept.end());

    Ranges out;
    out.reserve(kept.size());
    for (const Range& r : kept) {
        if (!out.empty() && r.start <= out.back().end) {
            if (r.end > out.back().end) {
                out.back().end = r.end;
            }
            continue;
        }
        out.push_back(r);
    }
    return out;
}

std::int64_t verified_prefix(const Ranges& ranges) {
    const Ranges canon = canonical_ranges(ranges);
    if (!canon.empty() && canon.front().start == 0) {
        return canon.front().end;
    }
    return 0;
}

std::int64_t ranges_total(const Ranges& ranges) {
    std::int64_t n = 0;
    for (const Range& r : canonical_ranges(ranges)) {
        n += r.size();
    }
    return n;
}

bool ranges_cover(const Ranges& ranges, std::int64_t start, std::int64_t end) {
    if (end <= start) {
        return true;
    }
    for (const Range& r : canonical_ranges(ranges)) {
        if (r.start <= start && end <= r.end) {
            return true;
        }
    }
    return false;
}

Ranges ranges_missing(const Ranges& ranges, std::int64_t start, std::int64_t end) {
    Ranges out;
    std::int64_t at = start;
    for (const Range& r : canonical_ranges(ranges)) {
        if (r.end <= at) {
            continue;
        }
        if (r.start >= end) {
            break;
        }
        if (r.start > at) {
            out.push_back(Range{at, r.start});
        }
        at = std::max(at, r.end);
        if (at >= end) {
            break;
        }
    }
    if (at < end) {
        out.push_back(Range{at, end});
    }
    return out;
}

Ranges ranges_from_checkpoint(const std::optional<Json>& checkpoint) {
    if (!checkpoint.has_value() || checkpoint->is_null()) {
        return {};
    }
    if (!checkpoint->is_object()) {
        throw Invalid("a checkpoint carrying ranges must be a JSON object");
    }
    Ranges found;
    if (checkpoint->contains(kVerifiedKey) && !checkpoint->at(kVerifiedKey).is_null()) {
        const Json& raw = checkpoint->at(kVerifiedKey);
        if (!raw.is_array()) {
            throw Invalid(std::string(kVerifiedKey) + " is a list of [start, end) pairs");
        }
        for (const Json& pair : raw) {
            if (!pair.is_array() || pair.size() != 2) {
                throw Invalid("a verified range is a pair [start, end), got " + pair.dump());
            }
            found.push_back(Range{as_offset(pair.at(0), "a byte offset"),
                                  as_offset(pair.at(1), "a byte offset")});
        }
    }
    if (checkpoint->contains(kVerifiedPrefixKey) &&
        !checkpoint->at(kVerifiedPrefixKey).is_null()) {
        const std::int64_t prefix =
            as_offset(checkpoint->at(kVerifiedPrefixKey), kVerifiedPrefixKey);
        if (prefix > 0) {
            found.push_back(Range{0, prefix});
        }
    }
    return canonical_ranges(found);
}

Json checkpoint_with_ranges(const std::optional<Json>& checkpoint, const Ranges& ranges) {
    if (checkpoint.has_value() && !checkpoint->is_null() && !checkpoint->is_object()) {
        throw Invalid("a checkpoint carrying ranges must be a JSON object");
    }
    const Ranges canon = canonical_ranges(ranges);

    Json out = Json::object();
    out[kVerifiedPrefixKey] = verified_prefix(canon);
    Json verified = Json::array();
    for (const Range& r : canon) {
        Json pair = Json::array();
        pair.push_back(r.start);
        pair.push_back(r.end);
        verified.push_back(std::move(pair));
    }
    out[kVerifiedKey] = std::move(verified);

    if (checkpoint.has_value() && checkpoint->is_object()) {
        std::vector<std::string> names;
        for (auto it = checkpoint->begin(); it != checkpoint->end(); ++it) {
            if (it.key() == kVerifiedPrefixKey || it.key() == kVerifiedKey) {
                continue;
            }
            names.push_back(it.key());
        }
        std::sort(names.begin(), names.end());
        for (const std::string& name : names) {
            out[name] = checkpoint->at(name);
        }
    }
    return out;
}

Ranges checkpoint_ranges(const Record& r) { return ranges_from_checkpoint(r.checkpoint); }

void set_checkpoint_ranges(Record& r, const Ranges& ranges) {
    r.checkpoint = checkpoint_with_ranges(r.checkpoint, ranges);
    if (std::find(r.content.begin(), r.content.end(), model::kRanges) == r.content.end()) {
        r.content.push_back(model::kRanges);
    }
}

void add_checkpoint_range(Record& r, std::int64_t start, std::int64_t end) {
    Ranges next = checkpoint_ranges(r);
    next.push_back(Range{start, end});
    set_checkpoint_ranges(r, next);
}

void clear_checkpoint_ranges(Record& r) {
    if (r.checkpoint.has_value() && r.checkpoint->is_object()) {
        std::vector<std::string> names;
        for (auto it = r.checkpoint->begin(); it != r.checkpoint->end(); ++it) {
            if (it.key() != kVerifiedKey) {
                names.push_back(it.key());
            }
        }
        std::sort(names.begin(), names.end());
        if (names.empty()) {
            r.checkpoint.reset();
        } else {
            Json rest = Json::object();
            for (const std::string& name : names) {
                rest[name] = r.checkpoint->at(name);
            }
            r.checkpoint = std::move(rest);
        }
    }
    r.content.erase(std::remove(r.content.begin(), r.content.end(), std::string(model::kRanges)),
                    r.content.end());
}

}  // namespace job
}  // namespace abstraction
