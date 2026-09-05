#pragma once

// A checkpoint of ranges.
//
// WHAT ONE INTEGER COULD NOT SAY. A checkpoint used to hold a single verified
// prefix: "the first N bytes are proven", and nothing else. Every transfer that
// could be described was therefore one stream appending to the end of a file.
// That is not how bytes are fetched — sixteen concurrent ranged parts landing at
// scattered offsets in a sparse file is ordinary — and "parts 0, 2 and 5 done,
// 1, 3 and 4 partway" had no representation at all. An adopter with a parallel
// fetcher could only take this library by deleting its parallelism.
//
// A range set says it. The prefix is the degenerate case: one range at zero.
//
//     "checkpoint": {
//       "verified_prefix": 4194304,
//       "verified": [[0, 4194304], [8388608, 12582912]]
//     }
//
// WHY THE PREFIX STAYS. Not redundancy and not politeness. A reader that has
// never heard of `verified` resumes from `verified_prefix` and re-fetches the
// rest, which is exactly what it does today. That is the whole reason this is an
// addition rather than a break, and it is why model::kRanges is never critical:
// an old reader ignoring the ranges loses some bytes to a second fetch, and
// marking it critical would stop every existing reader dead for no safety gain.
// The prefix is DERIVED from the set on write, so nothing has to remember to
// keep the two agreeing.
//
// WHAT A RANGE MEANS. Proven, not merely written: the bytes are on disk AND
// checked, against a piece digest where the kind's spec carries one and against
// the transport's own framing where it does not. Bytes in flight when a process
// is killed are exactly the ones a successor must not trust; that rule is
// unchanged, and now applies per range instead of to one tail.
//
// WHERE THIS SITS. A checkpoint is opaque to this library. These helpers do not
// change that — they are a canonical FORM offered to whoever writes one, not a
// meaning read out of every record relayed. Record::encode leaves a checkpoint
// exactly as it found it; only a caller that asks for ranges gets them
// rewritten.

#include <abstraction/job/record.h>

#include <cstdint>
#include <string>
#include <vector>

namespace abstraction {
namespace job {

// The two keys these helpers own. Every other key in a checkpoint belongs to
// whoever wrote it and is carried through untouched.
constexpr const char* kVerifiedPrefixKey = "verified_prefix";
constexpr const char* kVerifiedKey = "verified";

// A half-open byte interval: `start` is included, `end` is not. An empty range
// (start == end) proves nothing and is dropped from a canonical set.
struct Range {
    std::int64_t start = 0;
    std::int64_t end = 0;

    std::int64_t size() const { return end - start; }
    bool empty() const { return end <= start; }

    bool operator==(const Range& o) const { return start == o.start && end == o.end; }
    bool operator!=(const Range& o) const { return !(*this == o); }
    bool operator<(const Range& o) const {
        return start != o.start ? start < o.start : end < o.end;
    }
};

// A set of proven byte ranges in canonical form: sorted by start,
// non-overlapping, non-adjacent, and containing no empty range.
using Ranges = std::vector<Range>;

// Sort, merge and validate a set of ranges: the merge-on-write the format
// promises. Callers hand in whatever they have — out of order, overlapping,
// duplicated, adjacent — and get the one spelling of that state that every
// implementation agrees on.
//
// ADJACENT ranges merge as well as overlapping ones, and that is not tidiness.
// [[0,4],[4,8]] and [[0,8]] are the same proven bytes; if both were legal, two
// implementations could write one state as different bytes and a conformance
// test comparing files would call them a disagreement. Merging touching ranges
// is what makes the form canonical rather than merely sorted.
//
// Throws Invalid for a negative offset or a range that ends before it starts.
Ranges canonical_ranges(const Ranges& in);

// The end of the range starting at zero, or 0 when there is none. In a
// canonical set at most one range can start at zero and it is the first, so
// this is the whole rule.
std::int64_t verified_prefix(const Ranges& ranges);

// How many proven bytes the set holds.
std::int64_t ranges_total(const Ranges& ranges);

// Whether every byte of [start, end) is proven. An empty interval is covered by
// anything, including the empty set.
bool ranges_cover(const Ranges& ranges, std::int64_t start, std::int64_t end);

// The gaps in [start, end) that are not proven yet — what a fetcher still has
// to ask for, which is the question a resume asks.
Ranges ranges_missing(const Ranges& ranges, std::int64_t start, std::int64_t end);

// The proven ranges a checkpoint carries.
//
// Three inputs, one answer:
//
//   * `verified` present: those ranges, canonicalised.
//   * only `verified_prefix`: {{0, prefix}}, because a prefix IS a range. This
//     is what lets a record written before ranges existed be read as one, and
//     what makes "the prefix is the degenerate case" true in code rather than
//     only in prose.
//   * neither, or no checkpoint at all: the empty set.
//
// When both are present the prefix is UNIONED IN rather than checked against
// the ranges. A prefix-only writer that took the job over and advanced the
// prefix without touching `verified` left a record where the two disagree, and
// the union is the only reading that loses nothing: both fields are claims that
// bytes are proven, and neither is a claim that other bytes are not.
Ranges ranges_from_checkpoint(const std::optional<Json>& checkpoint);

// A checkpoint carrying `ranges` canonically, keeping every key it does not own.
//
// The form is pinned, because three implementations have to produce the same
// bytes for the same state:
//
//     {"verified_prefix": P, "verified": [[s, e], ...], <everything else, by key>}
//
// The two range keys come first and in that order — a reader skimming a record
// should see the number that matters first — and the caller's other keys follow
// sorted by name. Sorted rather than left as found because Go reaches a
// checkpoint through a map, which has no order to preserve, so "as found" is
// not something all three languages can agree to do.
Json checkpoint_with_ranges(const std::optional<Json>& checkpoint, const Ranges& ranges);

// The proven ranges this record's checkpoint carries. A record that has never
// checkpointed, and one whose checkpoint predates ranges entirely, both answer
// without throwing: the first with the empty set, the second with the prefix as
// one range.
Ranges checkpoint_ranges(const Record& r);

// Record what is proven, merged into canonical form, and declare the ranges
// model.
//
// Both halves matter. Without the canonical form two writers spell one state
// two ways; without the declaration a reader cannot tell whether an absent
// `verified` means "nothing proven beyond the prefix" or "this writer had never
// heard of ranges".
void set_checkpoint_ranges(Record& r, const Ranges& ranges);

// Fold one newly proven range into the checkpoint. What a parallel fetcher
// calls as each part lands.
void add_checkpoint_range(Record& r, std::int64_t start, std::int64_t end);

// Remove the ranges and the declaration, leaving every other key alone.
//
// The declaration is carried rather than derived, so it has to be withdrawn
// explicitly; a record that kept declaring a model whose data it no longer
// holds sends a reader looking for something that is not there.
void clear_checkpoint_ranges(Record& r);

}  // namespace job
}  // namespace abstraction
