// The record and the lease are a cross-language contract, so most of what
// matters is only provable by running three implementations against one
// directory (scripts/conformance.sh). What is worth pinning here is everything
// a single process can prove on its own: the exact bytes written, the refusals,
// and the epoch fencing.

#include <abstraction/job/store.h>

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <filesystem>
#include <fstream>
#include <string>

namespace fs = std::filesystem;
using namespace abstraction::job;

static int g_failures = 0;

static void check(const char* name, bool ok) {
    std::printf("[%s] %s\n", ok ? "PASS" : "FAIL", name);
    if (!ok) ++g_failures;
}

static std::string temp_root() {
    const auto stamp = std::chrono::duration_cast<std::chrono::nanoseconds>(
                           std::chrono::system_clock::now().time_since_epoch())
                           .count();
    const fs::path root = fs::temp_directory_path() / ("abstraction-job-test-" + std::to_string(stamp));
    return root.string();
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

// The two answers a timestamp near the edge of this build's clock may give.
// Anything else -- a different instant, a year of other than four digits -- is
// the defect these pin, and it is invisible without the comparison.
static bool refused_as_bad_timestamp(const JobError& e) {
    return std::string(e.what()).find("bad_timestamp") != std::string::npos;
}

static bool held_or_refused(const std::string& canonical) {
    try {
        return format_rfc3339(parse_rfc3339(canonical)) == canonical;
    } catch (const JobError& e) {
        return refused_as_bad_timestamp(e);
    }
}

static bool written_or_refused(TimePoint t) {
    std::string written;
    try {
        written = format_rfc3339(t);
    } catch (const JobError& e) {
        return refused_as_bad_timestamp(e);
    }
    const auto micros = [](TimePoint p) {
        return std::chrono::duration_cast<std::chrono::microseconds>(p.time_since_epoch());
    };
    return written.size() == 27 && micros(parse_rfc3339(written)) == micros(t);
}

static void test_timestamp_format() {
    for (const char* invalid : {
        "2026-00-01T00:00:00Z", "2026-13-01T00:00:00Z",
        "2026-01-00T00:00:00Z", "2026-01-32T00:00:00Z",
        "2026-04-31T00:00:00Z", "2026-02-29T00:00:00Z",
        "1900-02-29T00:00:00Z", "2100-02-29T00:00:00Z",
        "2026-01-01T24:00:00Z", "2026-01-01T00:60:00Z",
        "2026-01-01T00:00:60Z", "2026-01-01T00:00:00+24:00",
        "2026-01-01T00:00:00-00:60", "2026-01-01T00:00:00+02:00junk",
        "2026-01-01T00:00:00Zjunk", "2026-01-01T00:00:00",
        "2026-01-01 00:00:00Z", "2026-01-01T00:00:00.Z",
        "2026-01-01T00:00:00.1234567890Z", "2026-01-01T00:00:00x",
        "2026-01-01T00:00:00Z ", " 2026-01-01T00:00:00Z"
    }) {
        bool refused = false;
        try { parse_rfc3339(invalid); }
        catch (const Invalid& e) { refused = refused_as_bad_timestamp(e); }
        check((std::string("timestamp: rejects ") + invalid).c_str(), refused);
    }
    check("timestamp: empty/default handling retained", parse_rfc3339("") == TimePoint{} &&
          parse_rfc3339(" \t\r\n") == TimePoint{});
    check("timestamp: leap-day offset and lowercase separators",
          format_rfc3339(parse_rfc3339("2000-03-01t00:15:00.1+00:30")) ==
          "2000-02-29T23:45:00.100000Z");
    check("timestamp: negative offset moves into next year",
          format_rfc3339(parse_rfc3339("2025-12-31T23:59:59.123456789-00:01")) ==
          "2026-01-01T00:00:59.123456Z");
    // Six fractional digits, always, trailing zeros included: Go trimmed them
    // for a while and disagreed with Python about the same instant.
    const TimePoint epoch{};
    check("timestamp: epoch", format_rfc3339(epoch) == "1970-01-01T00:00:00.000000Z");

    const TimePoint t = parse_rfc3339("2026-08-20T09:35:07.829869Z");
    check("timestamp: round trip", format_rfc3339(t) == "2026-08-20T09:35:07.829869Z");

    check("timestamp: trailing zeros kept",
          format_rfc3339(parse_rfc3339("2026-08-20T09:35:07.100000Z")) ==
              "2026-08-20T09:35:07.100000Z");

    // The wire's year is four digits and this build's clock holds a different
    // span: nanoseconds in an int64 reach 1677 to 2262, 100 ns ticks reach
    // ±29,227 years. So the answer for an instant near either edge is the
    // instant itself or a refusal, and never a third thing -- year one arrived
    // as 1754 on a nanosecond clock until this was pinned.
    for (const char* wide : {"0000-01-01T00:00:00.000000Z", "0001-01-01T00:00:00.000000Z",
                             "1677-09-22T00:00:00.000000Z", "2262-04-11T00:00:00.000000Z",
                             "9999-12-31T23:59:59.999999Z"}) {
        check((std::string("timestamp: held or refused, never wrapped: ") + wide).c_str(),
              held_or_refused(wide));
    }

    check("timestamp: the earliest instant this clock holds is written or refused",
          written_or_refused(TimePoint::min()));
    check("timestamp: the latest instant this clock holds is written or refused",
          written_or_refused(TimePoint::max()));

    // Nanoseconds from a Go writer truncate rather than round: a resume point
    // must never move forward because of a rounding rule.
    check("timestamp: nanoseconds truncate",
          format_rfc3339(parse_rfc3339("2026-08-20T09:35:07.829869999Z")) ==
              "2026-08-20T09:35:07.829869Z");

    check("timestamp: offset normalised to UTC",
          format_rfc3339(parse_rfc3339("2026-08-20T11:35:07.829869+02:00")) ==
              "2026-08-20T09:35:07.829869Z");
}

static void test_encoding() {
    Record r;
    r.id = "1787202430967-a752f9a9c2c77b123ffd";
    r.kind = "download";
    r.state = state::kRunning;
    r.spec = Json::parse(R"({"zebra":1,"apple":{"nested":[1,2,3]}})");
    r.checkpoint = Json::parse(R"({"verified_prefix":400})");
    r.progress.done = 460;
    r.progress.total = 1000;
    r.progress.updated_at = parse_rfc3339("2026-08-20T05:07:14.951609Z");
    r.lease.owner = "cpp-worker";
    r.lease.epoch = 2;
    r.lease.expires_at = parse_rfc3339("2026-08-20T05:08:14.635068Z");
    r.created_at = parse_rfc3339("2026-08-20T05:07:10.967343Z");
    r.updated_at = parse_rfc3339("2026-08-20T05:07:15.134811Z");

    const std::string encoded = r.encode();
    const std::string expected =
        "{\n"
        "  \"content\": [\n"
        "    \"abstraction.job/base@1\"\n"
        "  ],\n"
        "  \"critical\": [\n"
        "    \"abstraction.job/base@1\"\n"
        "  ],\n"
        "  \"id\": \"1787202430967-a752f9a9c2c77b123ffd\",\n"
        "  \"kind\": \"download\",\n"
        "  \"state\": \"running\",\n"
        "  \"spec\": {\n"
        "    \"zebra\": 1,\n"
        "    \"apple\": {\n"
        "      \"nested\": [\n"
        "        1,\n"
        "        2,\n"
        "        3\n"
        "      ]\n"
        "    }\n"
        "  },\n"
        "  \"checkpoint\": {\n"
        "    \"verified_prefix\": 400\n"
        "  },\n"
        "  \"progress\": {\n"
        "    \"done\": 460,\n"
        "    \"total\": 1000,\n"
        "    \"updated_at\": \"2026-08-20T05:07:14.951609Z\"\n"
        "  },\n"
        "  \"lease\": {\n"
        "    \"owner\": \"cpp-worker\",\n"
        "    \"epoch\": 2,\n"
        "    \"expires_at\": \"2026-08-20T05:08:14.635068Z\"\n"
        "  },\n"
        "  \"created_at\": \"2026-08-20T05:07:10.967343Z\",\n"
        "  \"updated_at\": \"2026-08-20T05:07:15.134811Z\"\n"
        "}\n";
    check("encode: byte-identical to the agreed form", encoded == expected);
    if (encoded != expected) {
        std::printf("---- got ----\n%s---- want ----\n%s", encoded.c_str(), expected.c_str());
    }

    // An alphabetising JSON container would reorder the opaque spec, which is
    // the caller's data and not ours to touch.
    check("encode: spec key order preserved",
          encoded.find("\"zebra\"") < encoded.find("\"apple\""));

    Json invalid_time = Json::parse(encoded);
    invalid_time["updated_at"] = "2026-04-31T00:00:00Z";
    check("decode: impossible timestamp refused rather than normalized",
          throws_job_error([&] { Record::decode(invalid_time.dump()); }));

    const Record back = Record::decode(encoded);
    check("decode: round trips", back.encode() == encoded);
    check("decode: spec is returned untouched", back.spec == r.spec);
    check("decode: checkpoint is returned untouched", back.checkpoint == r.checkpoint);

    Record thin;
    thin.id = "x";
    thin.kind = "download";
    const std::string thin_encoded = thin.encode();
    check("encode: zero total omitted", thin_encoded.find("\"total\"") == std::string::npos);
    check("encode: absent checkpoint omitted",
          thin_encoded.find("\"checkpoint\"") == std::string::npos);
    check("encode: empty requires omitted",
          thin_encoded.find("\"requires\"") == std::string::npos);
    check("encode: empty error omitted", thin_encoded.find("\"error\"") == std::string::npos);
    check("encode: trailing newline", !thin_encoded.empty() && thin_encoded.back() == '\n');
}

static void test_refusals() {
    Record r;
    r.id = "x";
    check("validate: kind is required", throws_job_error([&] { r.encode(); }));

    // What a Go peer writes for a timestamp nobody set: Go's zero time is year
    // one, where this reader's is 1970. A nanosecond clock cannot hold year one
    // at all, so the whole record is refused rather than read as 1754.
    const std::string go_zero_time =
        "{\n"
        "  \"content\": [\n"
        "    \"abstraction.job/base@1\"\n"
        "  ],\n"
        "  \"critical\": [\n"
        "    \"abstraction.job/base@1\"\n"
        "  ],\n"
        "  \"id\": \"x\",\n"
        "  \"kind\": \"download\",\n"
        "  \"state\": \"pending\",\n"
        "  \"spec\": {},\n"
        "  \"progress\": {\n"
        "    \"done\": 0,\n"
        "    \"updated_at\": \"0001-01-01T00:00:00.000000Z\"\n"
        "  },\n"
        "  \"lease\": {\n"
        "    \"owner\": \"\",\n"
        "    \"epoch\": 0,\n"
        "    \"expires_at\": \"0001-01-01T00:00:00.000000Z\"\n"
        "  },\n"
        "  \"created_at\": \"0001-01-01T00:00:00.000000Z\",\n"
        "  \"updated_at\": \"0001-01-01T00:00:00.000000Z\"\n"
        "}\n";
    bool zero_time_is_honest = false;
    try {
        zero_time_is_honest = Record::decode(go_zero_time).encode() == go_zero_time;
    } catch (const JobError& e) {
        zero_time_is_honest = std::string(e.what()).find("bad_timestamp") != std::string::npos;
    }
    check("decode: a peer's zero timestamp is held or refused, never wrapped", zero_time_is_honest);

    const std::string good = [] {
        Record ok;
        ok.id = "x";
        ok.kind = "download";
        return ok.encode();
    }();

    // A model this implementation genuinely cannot read, declared CRITICAL by
    // whoever wrote it. That is the whole point of the content set: the writer
    // says "you cannot act on this correctly without understanding me", and the
    // only honest answer is to refuse.
    //
    // Built rather than patched. This was find/replace surgery on a literal
    // "schema": 4, and it broke TWICE for the same reason: the format moved, so
    // find() returned npos, replace() threw std::out_of_range — not a JobError,
    // so it escaped main uncaught and the run died with no verdict at all. A
    // test that spells out a format is a test that silently stops testing.
    const std::string critical_unknown =
        "{\n"
        "  \"content\": [\n"
        "    \"abstraction.job/base@1\",\n"
        "    \"abstraction.job/hovercraft@7\"\n"
        "  ],\n"
        "  \"critical\": [\n"
        "    \"abstraction.job/base@1\",\n"
        "    \"abstraction.job/hovercraft@7\"\n"
        "  ],\n"
        "  \"id\": \"x\",\n"
        "  \"kind\": \"download\",\n"
        "  \"state\": \"pending\",\n"
        "  \"spec\": {},\n"
        "  \"progress\": {\n"
        "    \"done\": 0,\n"
        "    \"updated_at\": \"2026-01-01T00:00:00.000000Z\"\n"
        "  },\n"
        "  \"lease\": {\n"
        "    \"owner\": \"\",\n"
        "    \"epoch\": 0,\n"
        "    \"expires_at\": \"2026-01-01T00:00:00.000000Z\"\n"
        "  },\n"
        "  \"created_at\": \"2026-01-01T00:00:00.000000Z\",\n"
        "  \"updated_at\": \"2026-01-01T00:00:00.000000Z\"\n"
        "}\n";
    check("decode: an unknown CRITICAL model is refused",
          throws_job_error([&] { Record::decode(critical_unknown); }));

    // The same record with that model advisory instead. A reader that ignores
    // it is still correct about everything that matters, so it must read.
    std::string advisory = critical_unknown;
    const std::string crit_block =
        "  \"critical\": [\n"
        "    \"abstraction.job/base@1\",\n"
        "    \"abstraction.job/hovercraft@7\"\n"
        "  ],\n";
    const std::size_t at = advisory.find(crit_block);
    check("test setup: the critical block was found", at != std::string::npos);
    if (at != std::string::npos) {
        advisory.replace(at, crit_block.size(),
                         "  \"critical\": [\n    \"abstraction.job/base@1\"\n  ],\n");
        check("decode: an unknown ADVISORY model is read",
              !throws_job_error([&] { Record::decode(advisory); }));
    }

    // Legacy records stay readable, and that is a guarantee rather than an
    // accident: stores full of version 3 and 4 records exist on disks and on a
    // NAS, and the mapping onto models is exact rather than a guess.
    const std::string legacy_v3 =
        "{\n"
        "  \"schema\": 3,\n"
        "  \"id\": \"x\",\n"
        "  \"kind\": \"download\",\n"
        "  \"state\": \"pending\",\n"
        "  \"spec\": {},\n"
        "  \"progress\": {\n"
        "    \"done\": 0,\n"
        "    \"updated_at\": \"2026-01-01T00:00:00.000000Z\"\n"
        "  },\n"
        "  \"lease\": {\n"
        "    \"owner\": \"\",\n"
        "    \"epoch\": 0,\n"
        "    \"expires_at\": \"2026-01-01T00:00:00.000000Z\"\n"
        "  },\n"
        "  \"created_at\": \"2026-01-01T00:00:00.000000Z\",\n"
        "  \"updated_at\": \"2026-01-01T00:00:00.000000Z\"\n"
        "}\n";
    check("decode: a legacy version 3 record is still readable",
          !throws_job_error([&] { Record::decode(legacy_v3); }));

    std::string extra = good;
    extra.replace(extra.find("\"id\""), 4, "\"invented_key\": 1,\n  \"id\"");
    check("decode: unknown field refused", throws_job_error([&] { Record::decode(extra); }));

    std::string no_kind = good;
    no_kind.replace(no_kind.find("\"download\""), 10, "\"\"");
    check("decode: missing kind refused", throws_job_error([&] { Record::decode(no_kind); }));

    check("decode: garbage refused", throws_job_error([] { Record::decode("not json"); }));

    // Every refusal leaves decode as a JobError, or a caller that catches this
    // layer's own refusals does not catch it and the process ends on bytes
    // somebody else wrote. An integer past 64 bits came out as a Json::Error.
    std::string too_big = good;
    too_big.replace(too_big.find("\"done\": 0"), 9, "\"done\": 9223372036854775808");
    check("decode: a number past what a field holds is refused, not thrown past",
          throws_job_error([&] { Record::decode(too_big); }));
}

// A record built from its two declaration lists and nothing else, so a test
// about `critical` says what it is about. Patching a literal is how the two
// tests above stopped testing anything, twice.
static std::string declaring(const std::string& content, const std::string& critical,
                             const std::string& spec = "{}", const std::string& extra = "") {
    return "{\n"
           "  \"content\": [" + content + "],\n"
           "  \"critical\": [" + critical + "],\n"
           "  \"id\": \"x\",\n"
           "  \"kind\": \"download\",\n"
           "  \"state\": \"pending\",\n"
           "  \"spec\": " + spec + ",\n" + extra +
           "  \"progress\": {\n"
           "    \"done\": 0,\n"
           "    \"updated_at\": \"2026-01-01T00:00:00.000000Z\"\n"
           "  },\n"
           "  \"lease\": {\n"
           "    \"owner\": \"\",\n"
           "    \"epoch\": 0,\n"
           "    \"expires_at\": \"2026-01-01T00:00:00.000000Z\"\n"
           "  },\n"
           "  \"created_at\": \"2026-01-01T00:00:00.000000Z\",\n"
           "  \"updated_at\": \"2026-01-01T00:00:00.000000Z\"\n"
           "}\n";
}

static bool marked(const Record& r, const std::string& name) {
    return std::find(r.critical.begin(), r.critical.end(), name) != r.critical.end();
}

// The rules that read `critical`, in the order [JOB-D10] fixes them in. These
// mirror abstraction-job/python's CriticalTest and abstraction-job/go's refusals_test.go: three readers
// that disagree about which records are readable are not three implementations
// of one contract.
static void test_critical_rules() {
    const std::string base = "\"abstraction.job/base@1\"";
    const std::string intent = "\"abstraction.job/intent@1\"";
    const std::string delegation = "\"abstraction.job/delegation@1\"";
    const std::string step = "\"abstraction.job/step@1\"";
    const std::string ranges = "\"abstraction.download/ranges@1\"";

    // [JOB-C2]. A reader that knows nothing about `verified` resumes from the
    // prefix and re-fetches the rest, so a writer that marked the model critical
    // must not be able to stop one.
    const Record stripped = Record::decode(declaring(base, base + "," + ranges));
    check("decode: a critical ranges marking is stripped, not refused",
          !marked(stripped, feature::kRanges));
    check("decode: and it is gone from what this reader writes back",
          stripped.encode().find(feature::kRanges) == std::string::npos);

    const Record no_step = Record::decode(declaring(base + "," + step, base + "," + step));
    check("decode: a critical step marking is stripped too", !marked(no_step, feature::kStep));

    // Stripping first is what makes [JOB-C2] true against [JOB-D8]. Order the
    // two the other way and this record refuses.
    const Record kept = Record::decode(declaring(base, base + "," + ranges + "," + step));
    check("decode: a stripped name is never checked for the subset rule",
          kept.critical.size() == 1 && marked(kept, feature::kBase));

    // [JOB-D8], and RFC 9052 3.1 behind it: a label listed as critical whose
    // parameter is not carried is a fatal error, not a shrug.
    check("decode: a critical name outside content is not a subset",
          throws_job_error([&] { Record::decode(declaring(base, base + "," + intent)); }));

    // Stripping one advisory name is not permission to ignore criticality.
    check("decode: an unknown critical name still refuses",
          throws_job_error(
              [&] { Record::decode(declaring(base + ",\"x.example/y@1\"",
                                             base + ",\"x.example/y@1\"")); }));

    // [JOB-I7] with [JOB-I9]: a reader too old to know the intent model must
    // refuse rather than keep working on a job somebody asked to stop, so
    // marking intent is correct rather than an error.
    check("decode: intent is critical whenever present",
          !throws_job_error([&] {
              Record::decode(declaring(base + "," + intent, base + "," + intent, "{}",
                                       "  \"intent\": {\"want\": \"cancel\"},\n"));
          }));
    check("decode: delegation is critical whenever present",
          !throws_job_error([&] {
              Record::decode(declaring(
                  base + "," + delegation, base + "," + delegation, "{}",
                  "  \"delegation\": {\"system\": \"s\", \"external_id\": \"e\"},\n"));
          }));

    // [JOB-F1]. The scope a definition profile granted and dropped: a newer
    // writer's addition here was accepted and destroyed on the next write.
    check("decode: delegation refuses a field it does not know",
          throws_job_error([&] {
              Record::decode(declaring(base + "," + delegation, base + "," + delegation, "{}",
                                       "  \"delegation\": {\"system\": \"s\", "
                                       "\"external_id\": \"e\", \"settled_at\": 1},\n"));
          }));

    // A bare string here was skipped as the wrong type and the record accepted
    // with a declaration nobody had read.
    std::string as_string = Record::decode(declaring(base, base)).encode();
    const std::string block = "  \"content\": [\n    " + base + "\n  ],\n";
    const std::size_t where = as_string.find(block);
    check("test setup: the content block was found", where != std::string::npos);
    if (where != std::string::npos) {
        as_string.replace(where, block.size(), "  \"content\": " + base + ",\n");
        check("decode: a declaration list must hold strings",
              throws_job_error([&] { Record::decode(as_string); }));
    }
}

// Intent: what somebody WANTS, written by a party holding no lease.
//
// The case this exists for is a person clicking cancel in this server's UI while
// a supervisor on another machine moves the bytes. That person has no lease and
// cannot get one without stealing the job, which is the single thing the lease
// exists to prevent.
static void test_intent() {
    const std::string root = temp_root();
    FileStore store(root);

    Record r;
    r.kind = "download";
    r.spec = Json::parse(R"({"sink":{"final":"model.gguf"}})");
    const std::string id = store.submit(r);

    check("intent: absent means run", store.load(id).wants() == want::kRun);
    check("intent: work with no intent is available", store.orphans().size() == 1);

    // No epoch is presented. That absence is the feature.
    const Record paused = store.set_intent(id, want::kPause, "a-person");
    check("intent: settable without a lease", paused.wants() == want::kPause);
    check("intent: who asked is recorded", paused.intent && paused.intent->by == "a-person");
    check("intent: paused is not terminal", !paused.terminal() && paused.paused());

    // A paused job looks abandoned and is not. A sweep that adopted it would
    // restart work seconds after a person stopped it.
    check("intent: a paused job is not an orphan", store.orphans().empty());

    store.set_intent(id, want::kRun, "a-person");
    check("intent: resuming makes it available again", store.orphans().size() == 1);

    // Survives the round trip another language will read it through.
    const Record back = Record::decode(store.load(id).encode());
    check("intent: round trips", back.wants() == want::kRun);

    check("intent: an unknown want is refused",
          throws_job_error([&] { store.set_intent(id, "nonsense", "a-person"); }));

    // Nothing reopens finished work.
    const Record claimed = store.claim(id, "worker", std::chrono::seconds(30));
    store.update(id, claimed.lease.epoch, [](Record& rec) { rec.state = state::kComplete; });
    check("intent: refused once terminal",
          throws_job_error([&] { store.set_intent(id, want::kCancel, "a-person"); }));

    std::error_code ec;
    fs::remove_all(root, ec);
}

static void test_lease_rules() {
    const std::string root = temp_root();
    FileStore store(root);

    Record r;
    r.kind = "download";
    r.spec = Json::parse(R"({"sink":{"final":"model.gguf"}})");
    r.progress.total = 1000;
    const std::string id = store.submit(std::move(r));

    check("submit: id is returned", !id.empty());
    check("submit: pending", store.load(id).state == state::kPending);
    check("submit: a fresh job is an orphan", store.orphans().size() == 1);

    const Record claimed = store.claim(id, "cpp-worker", std::chrono::seconds(30));
    check("claim: epoch starts at one", claimed.lease.epoch == 1);
    check("claim: state becomes running", claimed.state == state::kRunning);
    check("claim: the record's lock is beside it",
          fs::exists(fs::path(root) / "jobs" / (id + ".json.lock")));
    check("claim: a held job is not an orphan", store.orphans().empty());
    check("claim: another owner is refused",
          throws_job_error([&] { store.claim(id, "other", std::chrono::seconds(30)); }));

    store.update(id, 1, [](Record& rec) {
        rec.progress.done = 460;
        rec.checkpoint = Json::parse(R"({"verified_prefix":400})");
    });
    check("update: stale epoch refused",
          throws_job_error([&] { store.update(id, 0, [](Record&) {}); }));
    check("update: future epoch refused",
          throws_job_error([&] { store.update(id, 7, [](Record&) {}); }));

    // The sleep case: a process suspended past its own expiry wakes up still
    // believing it owns the job. Renew must refuse and force a re-claim, which
    // bumps the epoch and invalidates anything it had in flight.
    store.set_clock([] { return Clock::now() + std::chrono::hours(1); });
    check("renew: an expired lease may not be renewed",
          throws_job_error([&] { store.renew(id, 1, std::chrono::seconds(30)); }));
    check("update: an expired lease may not write",
          throws_job_error([&] { store.update(id, 1, [](Record&) {}); }));
    check("orphans: an expired lease is claimable", store.orphans().size() == 1);

    const Record adopted = store.claim(id, "successor", std::chrono::seconds(30));
    check("claim: epoch rises on adoption", adopted.lease.epoch == 2);
    check("claim: successor inherits the proven checkpoint",
          adopted.checkpoint && (*adopted.checkpoint)["verified_prefix"] == 400);
    check("claim: the predecessor's epoch is now stale",
          throws_job_error([&] { store.update(id, 1, [](Record&) {}); }));

    // The write that MAKES a record terminal is an ordinary update onto a record
    // that is not yet terminal, and it must land; every write after it is
    // refused, including the caller's own release. Update was the one operation
    // no implementation refused, so a lease holder could write progress onto a
    // finished record and release could walk it back to pending, where the next
    // sweep offered it as an orphan.
    store.update(id, 2, [](Record& rec) { rec.state = state::kComplete; });
    check("claim: a terminal job is refused",
          throws_job_error([&] { store.claim(id, "late", std::chrono::seconds(30)); }));
    check("update: a terminal job is refused",
          throws_job_error([&] { store.update(id, 2, [](Record& rec) { rec.progress.done = 999; }); }));
    check("release: a terminal job is refused",
          throws_job_error([&] { store.release(id, 2); }));
    const Record finished = store.load(id);
    check("update: a finished record was not written over",
          finished.state == state::kComplete && finished.progress.done == 460);
    check("orphans: a finished job is not stranded work", store.orphans().empty());

    check("load: an unknown id is not found",
          throws_job_error([&] { store.load("no-such-job"); }));

    std::error_code ec;
    fs::remove_all(root, ec);
}

static void test_atomic_replacement() {
    const std::string root = temp_root();
    FileStore store(root);

    Record r;
    r.kind = "download";
    const std::string id = store.submit(std::move(r));
    store.claim(id, "cpp-worker", std::chrono::seconds(30));
    store.update(id, 1, [](Record& rec) { rec.progress.done = 1; });

    int leftovers = 0;
    for (const auto& entry : fs::directory_iterator(fs::path(root) / "jobs")) {
        if (entry.path().filename().string().find(".tmp-") != std::string::npos) {
            ++leftovers;
        }
    }
    check("write: no temp files left behind", leftovers == 0);

    std::error_code ec;
    fs::remove_all(root, ec);
}

// A finished job is waiting for the requester, not for a supervisor to redo it.
static void test_transferred_is_not_an_orphan() {
    const std::string root = temp_root();
    FileStore store(root);

    Record r;
    r.kind = "download";
    const std::string id = store.submit(std::move(r));
    Record held = store.claim(id, "worker", std::chrono::seconds(30));
    store.update(id, held.lease.epoch,
                 [](Record& rec) { rec.state = state::kTransferred; });
    check("staging: the job really is transferred",
          store.load(id).state == state::kTransferred);

    // Let the lease lapse, as it would after a crash. The clock moves rather
    // than the test sleeping: a 1 ms TTL makes the staging update itself race
    // the expiry, which is how the Go version of this test once staged nothing
    // and passed anyway.
    store.set_clock([] { return Clock::now() + std::chrono::hours(1); });

    bool offered = false;
    for (const auto& o : store.orphans()) {
        if (o.id == id) offered = true;
    }
    check("orphans: a transferred job is not stranded work", !offered);

    // But taking delivery must still be possible. Only the rescue sweep leaves
    // it alone.
    bool can_take_delivery = true;
    try {
        store.claim(id, "consumer", std::chrono::seconds(30));
    } catch (const std::exception&) {
        can_take_delivery = false;
    }
    check("claim: a transferred job may still be taken delivery of",
          can_take_delivery);

    std::error_code ec;
    fs::remove_all(root, ec);
}

// A claim computed from a record that has since moved must not commit.
//
// Found by reading rather than by two implementations disagreeing: before the
// record was changed under a lock, the write after a claim was unguarded
// -- so a claimant descheduled between reading the record and writing it could
// rename its own version over a newer owner's and return success to a caller
// that then went and did the work.
//
// The epoch even went backwards. A successful claim removes the previous
// epoch's token, so the number a straggler was going to take is free again by
// the time it wakes up, and it writes epoch 1 over epoch 2 with every step
// reporting success. A zombie holding the earlier epoch 1 then passes the
// staleness check in update(), which is precisely the two-owners damage the
// whole lease exists to prevent.
static void test_a_stale_claim_cannot_commit_over_a_newer_owner() {
    const std::string root = temp_root();
    FileStore store(root);

    Record r;
    r.kind = "download";
    const std::string id = store.submit(std::move(r));

    // What a straggler read before it was descheduled: the job, unclaimed.
    const Record seen = store.load(id);

    // Two claims go through while it is not looking. The second one's cleanup
    // removes the token for epoch 1, so the number the straggler is about to
    // take is free again.
    const Record zombie = store.claim(id, "first", std::chrono::seconds(30));
    store.set_clock([] { return Clock::now() + std::chrono::seconds(31); });
    const Record current = store.claim(id, "second", std::chrono::seconds(30));
    check("staging: the record is at epoch 2", current.lease.epoch == 2);

    // The straggler wakes up and finishes the claim it started.
    check("claim: a claim computed from a record that has moved is refused",
          throws_job_error([&] { store.claim_from(seen, "straggler", std::chrono::seconds(30)); }));

    const Record after = store.load(id);
    check("claim: the epoch did not go backwards",
          after.lease.epoch == 2 && after.lease.owner == "second");

    // The damage that would have followed: the first owner's lease expired and
    // its writes must stay refused. They only stay refused while the epoch on
    // disk is above its own.
    check("claim: a zombie owner's write is still refused",
          throws_job_error([&] { store.update(id, zombie.lease.epoch, [](Record&) {}); }));

    std::error_code ec;
    fs::remove_all(root, ec);
}

// Delegation releases the lease immediately, so a delegated job spends most of
// its life unleased. Demoting it to pending would invite a second tier to start
// the same work again.
static void test_releasing_delegated_job_keeps_it_running() {
    const std::string root = temp_root();
    FileStore store(root);

    Record r;
    r.kind = "download";
    const std::string id = store.submit(std::move(r));
    Record held = store.claim(id, "delegator", std::chrono::seconds(30));
    store.update(id, held.lease.epoch, [](Record& rec) {
        Delegation d;
        d.system = "nas";
        d.external_id = "remote-1";
        rec.delegation = d;
        rec.state = state::kRunning;
    });
    store.release(id, held.lease.epoch);

    Record got = store.load(id);
    check("release: a delegated job stays running", got.state == state::kRunning);
    check("release: the delegation handle survives", got.delegated());

    // An undelegated job still goes back to pending, which is what release is
    // for in the ordinary case.
    Record p;
    p.kind = "download";
    const std::string plain = store.submit(std::move(p));
    Record ph = store.claim(plain, "worker", std::chrono::seconds(30));
    store.release(plain, ph.lease.epoch);
    check("release: an ordinary job returns to pending",
          store.load(plain).state == state::kPending);

    std::error_code ec;
    fs::remove_all(root, ec);
}

static void test_recall() {
    const std::string root = temp_root();
    FileStore store(root);
    // Microseconds, because that is what the record can hold and the test
    // compares against what comes back off disk.
    const TimePoint start = std::chrono::time_point_cast<std::chrono::microseconds>(Clock::now());
    TimePoint clock = start;
    store.set_clock([&] { return clock; });

    Record r;
    r.kind = "test-kind";
    r.spec = Json::parse(R"({"what":"a thing"})");
    const std::string id = store.submit(std::move(r));

    check("recall: nobody holding is lease-expired",
          throws_job_error([&] { store.recall(id, 0, "yield", "", std::chrono::seconds(60)); }));
    const Record held = store.claim(id, "holder", std::chrono::hours(1));
    check("recall: an epoch nobody observed is stale",
          throws_job_error([&] { store.recall(id, 2, "yield", "", std::chrono::seconds(60)); }));
    check("recall: a reason is required",
          throws_job_error([&] { store.recall(id, 1, " ", "", std::chrono::seconds(60)); }));

    const Record recalled = store.recall(id, 1, "yield", "issuer", std::chrono::seconds(10));
    check("recall: recorded with its reason",
          recalled.lease.recalled() && recalled.lease.recall->reason == "yield");
    check("recall: intent untouched", recalled.wants() == want::kRun);
    check("recall: the lease now ends at the deadline",
          recalled.lease.expires_at == start + std::chrono::seconds(10));
    check("recall: declared critical",
          std::find(recalled.critical.begin(), recalled.critical.end(), feature::kRecall) !=
              recalled.critical.end());
    check("recall: round trip is byte-stable",
          Record::decode(recalled.encode()).encode() == recalled.encode());

    store.renew(id, 1, std::chrono::hours(1));
    check("renew: never extends past the deadline",
          store.load(id).lease.expires_at == start + std::chrono::seconds(10));
    store.update(id, 1, [](Record& rec) { rec.progress.done = 8; });
    check("claim: the holder cannot shed a recall by re-claiming",
          throws_job_error([&] { store.claim(id, "holder", std::chrono::hours(1)); }));

    clock = start + std::chrono::seconds(11);
    check("update: evicted at the deadline",
          throws_job_error([&] { store.update(id, 1, [](Record&) {}); }));
    check("orphans: an evicted job is offered", store.orphans().size() == 1);
    const Record evicted = store.load(id);
    check("evicted: still says who was asked and what they proved",
          evicted.lease.recalled() && evicted.lease.owner == "holder" && evicted.progress.done == 8);

    const Record next = store.claim(id, "successor", std::chrono::hours(1));
    check("claim: a new holding carries no recall",
          !next.lease.recalled() &&
              std::find(next.content.begin(), next.content.end(), feature::kRecall) ==
                  next.content.end());
    store.recall(id, 2, "yield", "", std::chrono::seconds(60));
    store.release(id, 2);
    const Record complied = store.load(id);
    check("release: compliance keeps the recall and drops the owner",
          complied.lease.recalled() && complied.lease.owner.empty() &&
              complied.state == state::kPending);
    (void)held;

    std::error_code ec;
    fs::remove_all(root, ec);
}

// --- the base envelope ------------------------------------------------------
//
// The corpus under abstraction-job/testdata/envelope is the point of these. The rules the
// envelope adds — the schema grammar, and that every action name resolves to a
// schema the record declares — are reader obligations, which the definition
// cannot state and the generated codec therefore cannot enforce. That is the
// exact class of rule where three hand-written readers drift silently, so the
// expected verdict lives in one directory and every language reads it.

static const char* kVendor = "nas.example/transfer@2";

// Located from the source path rather than the working directory, because the
// working directory of a test binary is whatever the runner felt like.
static fs::path corpus_dir() {
    return fs::path(__FILE__).parent_path().parent_path().parent_path() / "testdata" / "envelope";
}

static std::string read_file(const fs::path& p) {
    std::ifstream in(p, std::ios::binary);
    return std::string((std::istreambuf_iterator<char>(in)), std::istreambuf_iterator<char>());
}

static std::string verdict_for(const std::string& raw, Record* out) {
    try {
        Record r = Record::decode(raw);
        if (out != nullptr) *out = r;
        return "accept";
    } catch (const UnknownSchema&) {
        return "unknown_schema";
    } catch (const Invalid&) {
        return "invalid";
    } catch (const JobError&) {
        return "other";
    }
}

static std::string ask_word(const Record& r, const std::string& a, const Supervisor& s) {
    try {
        r.ask(a, s);
        return "ok";
    } catch (const UnknownSchema&) {
        return "unknown_schema";
    } catch (const NotSupported&) {
        return "not_supported";
    } catch (const Invalid&) {
        return "invalid";
    }
}

static std::vector<std::string> split_on(const std::string& s, char sep) {
    std::vector<std::string> out;
    std::size_t start = 0;
    for (;;) {
        const std::size_t at = s.find(sep, start);
        out.push_back(at == std::string::npos ? s.substr(start) : s.substr(start, at - start));
        if (at == std::string::npos) return out;
        start = at + 1;
    }
}

static std::vector<std::string> words(const std::string& s) {
    if (s == "-") return {};
    std::vector<std::string> out;
    for (const std::string& w : split_on(s, ' ')) {
        if (!w.empty()) out.push_back(w);
    }
    return out;
}

static void test_envelope_corpus() {
    const fs::path dir = corpus_dir();
    if (!fs::is_directory(dir)) {
        check(("the envelope corpus is at " + dir.string()).c_str(), false);
        return;
    }
    std::vector<fs::path> files;
    for (const auto& e : fs::directory_iterator(dir)) {
        if (e.path().extension() == ".json") files.push_back(e.path());
    }
    std::sort(files.begin(), files.end());
    check("the envelope corpus is not empty", !files.empty());
    for (const fs::path& p : files) {
        const std::string name = p.filename().string();
        const std::string want = split_on(name, '.').front();
        const std::string raw = read_file(p);
        Record r;
        const std::string got = verdict_for(raw, &r);
        check((name + ": " + want).c_str(), got == want);
        if (got != "accept" || want != "accept") continue;
        // An accepted case is also the byte proof: the file IS the canonical
        // form, so a reader that re-spells anything on the way out fails here
        // rather than in a diff between two languages a week later.
        check((name + ": decode then encode is the same bytes").c_str(), r.encode() == raw);
    }

    std::ifstream asks(dir / "asks.tsv");
    check("asks.tsv opens", asks.good());
    std::string line;
    int seen = 0;
    while (std::getline(asks, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (line.empty() || line[0] == '#') continue;
        const std::vector<std::string> col = split_on(line, '\t');
        if (col.size() != 5) {
            check("asks.tsv has five columns", false);
            continue;
        }
        const Record r = Record::decode(read_file(dir / col[0]));
        Supervisor s;
        s.schemas = words(col[2]);
        s.actions = words(col[3]);
        const std::string got = ask_word(r, col[1], s);
        check((col[0] + " asked \"" + col[1] + "\": " + col[4]).c_str(), got == col[4]);
        ++seen;
    }
    check("asks.tsv holds cases", seen > 0);
}

static void test_a_schema_identifier_cannot_be_an_address() {
    bool all_refused = true;
    for (const char* s : {"https://example.invalid/steal@1", "http://127.0.0.1:8080/x@1",
                          "file:///etc/passwd@1", "//example.invalid/x@1", "\\\\host\\share\\x@1",
                          "../../../etc/passwd@1", "C:/windows/system32@1",
                          "example.invalid/%2e%2e/x@1", "example.invalid/x@1#fragment",
                          "example.invalid/a/b@1", "Example.Invalid/X@1", "example.invalid/x",
                          "example.invalid/x@0", "example.invalid/x@01", "example..invalid/x@1",
                          "example invalid/x@1", ""}) {
        if (valid_schema(s)) {
            std::printf("      accepted as a schema identifier: %s\n", s);
            all_refused = false;
        }
    }
    check("no address is a legal schema identifier", all_refused);
    bool all_accepted = true;
    for (const char* s : {"abstraction.job/base@1", "nas.example/transfer@2", "a/b@1",
                          "a-b.c-d.e/f-g@123456789"}) {
        if (!valid_schema(s)) all_accepted = false;
    }
    check("a schema identifier is accepted", all_accepted);
}

static void test_a_namespaced_action_does_not_collide_with_a_bare_one() {
    Record r;
    r.id = "1787202430967-a752f9a9c2c77b123ffd";
    r.kind = "test";
    r.spec = Json::object();
    r.spec["anything"] = 1;
    Envelope e;
    e.schema = kVendor;
    e.actions = {std::string(kVendor) + "#cancel"};
    r.envelope = e;
    check("a bare cancel does not match a vendor's cancel", !r.supports(action::kCancel));
    check("the vendor's own cancel matches itself", r.supports(std::string(kVendor) + "#cancel"));

    r.envelope->actions = {action::kCancel, std::string(kVendor) + "#cancel"};
    check("a record may declare both, and they are two actions",
          !throws_job_error([&] { r.encode(); }));

    const std::vector<std::vector<std::string>> refused = {
        {"remirror"},
        {std::string(kBaseSchema) + "#pause"},
        {"someone.else/other@1#verify"},
        {action::kPause, action::kPause},
        {"Verify"},
        {"https://evil.invalid/x@1#go"},
        {std::string(kVendor) + "#a#b"}};
    for (const std::vector<std::string>& bad : refused) {
        r.envelope->actions = bad;
        check(("refused: " + bad.front()).c_str(), throws_job_error([&] { r.encode(); }));
    }
}

static void test_a_lease_holder_cannot_move_the_envelope() {
    const std::string root = temp_root();
    FileStore store(root);
    Record r;
    r.kind = "test";
    r.spec = Json::object();
    r.spec["anything"] = 1;
    Envelope e;
    e.schema = kVendor;
    e.actions = {action::kPause};
    r.envelope = e;
    const std::string id = store.submit(r);
    const Record held = store.claim(id, "a-holder", std::chrono::seconds(60));

    check("adding an action is refused", throws_job_error([&] {
              store.update(id, held.lease.epoch, [](Record& rr) {
                  rr.envelope->actions.push_back(std::string(kVendor) + "#verify");
              });
          }));
    check("dropping the envelope is refused", throws_job_error([&] {
              store.update(id, held.lease.epoch, [](Record& rr) { rr.envelope.reset(); });
          }));
    check("renaming the schema is refused", throws_job_error([&] {
              store.update(id, held.lease.epoch,
                           [](Record& rr) { rr.envelope->schema = "someone.else/other@1"; });
          }));

    const Record after = store.load(id);
    check("the envelope did not move", after.schema() == kVendor && after.actions().size() == 1);
    check("an ordinary write still lands", !throws_job_error([&] {
              store.update(id, held.lease.epoch, [](Record& rr) { rr.progress.done = 7; });
          }));

    std::error_code ec;
    fs::remove_all(root, ec);
}

int main() {
    // Unbuffered, so a crash still tells you which check it died after. When
    // stdout is a pipe it is fully buffered, so a test that dies mid-run prints
    // absolutely nothing and the failure looks like it happened before the
    // first line — which costs far more time than the buffering ever saved.
    std::setvbuf(stdout, nullptr, _IONBF, 0);

    test_timestamp_format();
    test_encoding();
    test_refusals();
    test_critical_rules();
    test_intent();
    test_lease_rules();
    test_atomic_replacement();
    test_transferred_is_not_an_orphan();
    test_a_stale_claim_cannot_commit_over_a_newer_owner();
    test_releasing_delegated_job_keeps_it_running();
    test_recall();
    test_envelope_corpus();
    test_a_schema_identifier_cannot_be_an_address();
    test_a_namespaced_action_does_not_collide_with_a_bare_one();
    test_a_lease_holder_cannot_move_the_envelope();

    std::printf("\n%s: %d failure(s)\n", g_failures == 0 ? "OK" : "FAILED", g_failures);
    return g_failures == 0 ? 0 : 1;
}
