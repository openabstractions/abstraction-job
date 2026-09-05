// A checkpoint of ranges, in C++.
//
// The record every implementation must produce byte for byte lives in
// testdata/ranges-record.json. One file, three languages: Go, Python and C++
// each build the same record from the same ranges and compare. A conformance
// test that compares bytes is the only thing that has ever caught these three
// disagreeing.
//
// ABSTRACTION_JOB_TESTDATA is the directory that file is in, handed in by
// CMake, because a test binary's working directory is wherever it was run from
// and a relative path finds the fixture only by luck.

#include <abstraction/job/ranges.h>
#include <abstraction/job/store.h>

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <string>
#include <vector>

namespace fs = std::filesystem;
using namespace abstraction::job;

static int g_failures = 0;

static void check(const char* name, bool ok) {
    std::printf("[%s] %s\n", ok ? "PASS" : "FAIL", name);
    if (!ok) ++g_failures;
}

template <typename Fn>
static bool throws_job_error(Fn&& fn) {
    try {
        fn();
        return false;
    } catch (const JobError&) {
        return true;
    }
}

static std::string fixture_bytes() {
    const fs::path p = fs::path(ABSTRACTION_JOB_TESTDATA) / "ranges-record.json";
    // Binary, not text: on Windows a text-mode read turns the file's LF into
    // nothing visible but a byte comparison that fails for a reason no diff
    // shows.
    std::ifstream in(p, std::ios::binary);
    if (!in) {
        std::printf("cannot open the shared fixture at %s\n", p.string().c_str());
        return "";
    }
    std::ostringstream buf;
    buf << in.rdbuf();
    return buf.str();
}

// Out of order, and with two adjacent pairs that must merge. A parallel
// fetcher's parts finish in whatever order they finish, and no caller should
// have to sort before recording.
static Ranges fixture_ranges() {
    return {
        {20971520, 23068672},
        {8388608, 10485760},
        {0, 2097152},
        {10485760, 12582912},
        {2097152, 4194304},
    };
}

static Record fixture_record() {
    Record r;
    r.id = "1787202430967-a752f9a9c2c77b123ffd";
    r.kind = "download";
    r.state = state::kRunning;
    r.spec = Json::parse(R"({"artifact":{"bytes":23068672}})");
    set_checkpoint_ranges(r, fixture_ranges());
    r.progress.done = 10485760;
    r.progress.total = 23068672;
    r.progress.updated_at = parse_rfc3339("2026-08-20T05:07:14.951609Z");
    r.lease.owner = "go-worker";
    r.lease.epoch = 2;
    r.lease.expires_at = parse_rfc3339("2026-08-20T05:08:14.635068Z");
    r.created_at = parse_rfc3339("2026-08-20T05:07:10.967343Z");
    r.updated_at = parse_rfc3339("2026-08-20T05:07:15.134811Z");
    return r;
}

// The point of a canonical form, tested the only way it can be tested: against
// the other two implementations' bytes.
static void test_agreed_bytes() {
    const std::string want = fixture_bytes();
    const std::string got = fixture_record().encode();
    check("ranges: encodes to the agreed bytes", got == want);
    if (got != want) {
        std::printf("---- got ----\n%s---- want ----\n%s", got.c_str(), want.c_str());
    }

    // Decoding the agreed bytes and writing them straight back must not move a
    // single byte, or two implementations taking turns on one job churn the
    // file against each other and no diff of its history means anything.
    const Record back = Record::decode(want);
    check("ranges: the agreed bytes round trip unchanged", back.encode() == want);
    const Ranges rs = checkpoint_ranges(back);
    check("ranges: three proven ranges came back", rs.size() == 3);
    check("ranges: the prefix came back", verified_prefix(rs) == 4194304);
    check("ranges: the proven total came back",
          ranges_total(rs) == 4194304 + 4194304 + 2097152);
}

// The half of the change that makes it additive rather than a break.
//
// A reader that has never heard of `verified` decodes the checkpoint it does
// know, finds the prefix, and resumes from it. It re-fetches everything past
// the first gap, which is what it does today; it does not fail, and it does not
// trust a byte nobody proved.
static void test_an_old_reader_still_resumes() {
    const Record r = Record::decode(fixture_bytes());
    check("ranges: a record carrying ranges is readable at all", r.checkpoint.has_value());
    check("ranges: an old reader resumes from the prefix",
          r.checkpoint->at("verified_prefix").get<std::int64_t>() == 4194304);
    check("ranges: never critical, or every existing reader would refuse the job",
          std::find(r.critical.begin(), r.critical.end(), model::kRanges) == r.critical.end());
    check("ranges: a record carrying ranges declares them",
          std::find(r.content.begin(), r.content.end(), model::kRanges) != r.content.end());

    // Advisory means advisory, whoever wrote the record.
    Record marked = fixture_record();
    marked.critical.push_back(model::kRanges);
    const Record after = Record::decode(marked.encode());
    check("ranges: a writer cannot promote a decoration to critical",
          std::find(after.critical.begin(), after.critical.end(), model::kRanges) ==
              after.critical.end());
}

static bool same(const Ranges& a, const Ranges& b) { return a == b; }

static void test_canonical_form() {
    struct Case {
        const char* name;
        Ranges in;
        Ranges want;
    };
    const std::vector<Case> cases = {
        {"already canonical", {{0, 4}, {8, 12}}, {{0, 4}, {8, 12}}},
        {"out of order", {{8, 12}, {0, 4}}, {{0, 4}, {8, 12}}},
        // The case that makes the form canonical rather than merely sorted:
        // [[0,4],[4,8]] and [[0,8]] are the same proven bytes, so only one of
        // them may be legal or two implementations spell one state two ways.
        {"adjacent merge", {{0, 4}, {4, 8}}, {{0, 8}}},
        {"overlapping merge", {{0, 6}, {4, 8}}, {{0, 8}}},
        {"contained", {{0, 8}, {2, 4}}, {{0, 8}}},
        {"identical", {{0, 8}, {0, 8}}, {{0, 8}}},
        {"chain", {{4, 8}, {0, 4}, {8, 9}, {20, 21}}, {{0, 9}, {20, 21}}},
        // Proves nothing, so it is not part of the state and must not change
        // the bytes.
        {"empty range dropped", {{0, 4}, {6, 6}}, {{0, 4}}},
        {"all empty", {{6, 6}}, {}},
        {"nothing at all", {}, {}},
    };
    for (const Case& c : cases) {
        const Ranges got = canonical_ranges(c.in);
        check((std::string("canonical: ") + c.name).c_str(), same(got, c.want));
        // Canonicalising a canonical set changes nothing, or one state would
        // have a spelling that depends on how many times it had been written.
        check((std::string("canonical: ") + c.name + " is idempotent").c_str(),
              same(canonical_ranges(got), got));
    }

    check("canonical: refuses a negative start",
          throws_job_error([] { canonical_ranges({{-1, 4}}); }));
    check("canonical: refuses a negative end",
          throws_job_error([] { canonical_ranges({{0, -4}}); }));
    check("canonical: refuses a range that ends before it starts",
          throws_job_error([] { canonical_ranges({{8, 4}}); }));
}

// The prefix is derived, not remembered, so it cannot drift from the set.
static void test_verified_prefix() {
    check("prefix: one range at zero", verified_prefix({{0, 400}}) == 400);
    check("prefix: a range at zero and others", verified_prefix({{0, 400}, {800, 900}}) == 400);
    // The case a single integer could never express, and the reason it had to
    // stop being the only thing a checkpoint says: real work is proven and the
    // prefix is still zero.
    check("prefix: nothing at zero", verified_prefix({{800, 900}, {1000, 1200}}) == 0);
    check("prefix: empty set", verified_prefix({}) == 0);
    check("prefix: a gap closed by a merge", verified_prefix({{0, 400}, {400, 800}}) == 800);

    // And what is written says the same thing as what is computed.
    const Json cp = checkpoint_with_ranges(std::nullopt, {{800, 900}, {0, 400}});
    check("prefix: written and computed agree",
          cp.at("verified_prefix").get<std::int64_t>() == 400);

    // A checkpoint written before ranges existed IS a range set: one range
    // starting at zero. Without this the degenerate case would be a special
    // case and every caller would have to handle both.
    check("prefix: a prefix-only checkpoint reads as one range",
          same(ranges_from_checkpoint(Json::parse(R"({"verified_prefix":400})")), {{0, 400}}));
    check("prefix: an empty checkpoint proves nothing",
          ranges_from_checkpoint(Json::parse("{}")).empty());
    check("prefix: no checkpoint at all proves nothing",
          ranges_from_checkpoint(std::nullopt).empty());

    // A prefix-only writer that took the job over and advanced the prefix
    // without touching `verified` leaves a record where the two disagree. Both
    // fields are claims that bytes are PROVEN and neither is a claim that other
    // bytes are not, so the union is the only reading that loses nothing.
    check("prefix: a staler verified set does not lose the proven prefix",
          same(ranges_from_checkpoint(Json::parse(
                   R"({"verified_prefix":8388608,"verified":[[0,4194304],[16777216,20971520]]})")),
               {{0, 8388608}, {16777216, 20971520}}));

    for (const char* bad : {R"({"verified":[[0]]})", R"({"verified":[[0,4,8]]})",
                            R"({"verified":[[4,0]]})", R"({"verified":[[-1,4]]})",
                            R"({"verified":"nope"})", R"({"verified_prefix":"400"})",
                            R"({"verified":[[0,4194304.5]]})"}) {
        const std::string body = bad;
        check((std::string("prefix: refuses ") + bad).c_str(),
              throws_job_error([&body] { ranges_from_checkpoint(Json::parse(body)); }));
    }
}

// What a parallel fetcher actually does: parts land in whatever order they land,
// and each is recorded as it is proven.
static void test_parallel_parts() {
    Record r;
    r.kind = "download";
    r.spec = Json::parse(R"({"bytes":48})");
    for (int part : {5, 0, 2, 1, 3, 4}) {
        add_checkpoint_range(r, part * 8, part * 8 + 8);
    }
    // Six touching parts are one proven range, and the prefix is the file.
    check("parallel: touching parts merge into one range",
          same(checkpoint_ranges(r), {{0, 48}}));
    check("parallel: the prefix is the whole file", verified_prefix(checkpoint_ranges(r)) == 48);

    // And the state that had no representation at all when a checkpoint was one
    // integer: parts 0, 2 and 5 done, the rest not.
    Record s;
    s.kind = "download";
    s.spec = Json::parse(R"({"bytes":48})");
    for (int part : {0, 2, 5}) {
        add_checkpoint_range(s, part * 8, part * 8 + 8);
    }
    const Ranges rs = checkpoint_ranges(s);
    check("parallel: scattered parts stay scattered",
          same(rs, {{0, 8}, {16, 24}, {40, 48}}));
    check("parallel: only part zero counts toward the prefix", verified_prefix(rs) == 8);
    // The gaps are what is left to fetch, which is the question a resume asks.
    check("parallel: the gaps are what is left to fetch",
          same(ranges_missing(rs, 0, 48), {{8, 16}, {24, 40}}));

    const Ranges two = {{0, 8}, {16, 24}};
    check("covers: exactly a range", ranges_cover(two, 0, 8));
    check("covers: inside a range", ranges_cover(two, 2, 6));
    check("covers: one byte past", !ranges_cover(two, 0, 9));
    check("covers: a gap", !ranges_cover(two, 8, 16));
    check("covers: an empty interval needs nothing proven", ranges_cover(two, 4, 4));
    check("missing: nothing proven leaves everything to fetch",
          same(ranges_missing({}, 0, 32), {{0, 32}}));
    check("missing: nothing left", ranges_missing(two, 0, 4).empty());
}

// A checkpoint belongs to whoever writes it. These helpers own two keys and must
// leave the rest where they were, sorted so all three implementations put them
// in the same order.
static void test_other_keys() {
    const Json got = checkpoint_with_ranges(
        Json::parse(R"({"zebra":1,"apple":{"nested":[1,2]},"verified_prefix":99})"), {{0, 400}});
    check("keys: the range keys come first and the rest are sorted",
          got.dump() ==
              R"({"verified_prefix":400,"verified":[[0,400]],"apple":{"nested":[1,2]},"zebra":1})");

    // The declaration is carried rather than derived, so it has to be
    // withdrawable: a record that kept declaring a model whose data it no longer
    // holds sends a reader looking for something that is not there.
    Record r = fixture_record();
    clear_checkpoint_ranges(r);
    const std::string text = r.encode();
    check("keys: clearing withdraws the declaration",
          text.find(model::kRanges) == std::string::npos);
    check("keys: clearing removes the ranges", text.find("\"verified\"") == std::string::npos);
    // The prefix survives, because it is not ours to remove: an old reader
    // still resumes from it.
    check("keys: clearing keeps the prefix",
          text.find("\"verified_prefix\": 4194304") != std::string::npos);
}

// The declaration cannot be derived — the checkpoint is opaque here — so it has
// to survive a reader that does a full read, modify and write without ever
// looking inside one.
static void test_declaration_survives_a_read_modify_write() {
    const auto stamp = std::chrono::duration_cast<std::chrono::nanoseconds>(
                           std::chrono::system_clock::now().time_since_epoch())
                           .count();
    const fs::path root =
        fs::temp_directory_path() / ("abstraction-job-ranges-" + std::to_string(stamp));
    FileStore store(root.string());

    Record r = fixture_record();
    r.id.clear();
    r.state = state::kPending;
    r.lease = Lease{};
    const std::string id = store.submit(r);

    const Record claimed = store.claim(id, "somebody-else", std::chrono::seconds(60));
    store.update(id, claimed.lease.epoch, [](Record& rec) {
        rec.progress.done = 99;  // nothing to do with ranges
    });

    const Record got = store.load(id);
    check("carried: a reader that never opened the checkpoint kept its declaration",
          std::find(got.content.begin(), got.content.end(), model::kRanges) != got.content.end());
    check("carried: the ranges survived", checkpoint_ranges(got).size() == 3);

    std::error_code ec;
    fs::remove_all(root, ec);
}

int main() {
    std::setvbuf(stdout, nullptr, _IONBF, 0);

    test_agreed_bytes();
    test_an_old_reader_still_resumes();
    test_canonical_form();
    test_verified_prefix();
    test_parallel_parts();
    test_other_keys();
    test_declaration_survives_a_read_modify_write();

    std::printf("\n%s: %d failure(s)\n", g_failures == 0 ? "OK" : "FAILED", g_failures);
    return g_failures == 0 ? 0 : 1;
}
