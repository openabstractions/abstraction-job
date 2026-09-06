// The parser reads bytes another machine wrote, so most of what is here is
// about what it REFUSES. Everything below either pins a spelling three
// implementations are compared on, or names an input that has taken a parser
// apart somewhere before: depth, an overlong encoding, a lone surrogate, a key
// that appears twice.

#include <abstraction/json/value.h>

#include <chrono>
#include <cstdio>
#include <cstdint>
#include <cstdlib>
#include <string>
#include <vector>

using abstraction::json::Value;

static int g_failures = 0;

static void check(const char* name, bool ok) {
    std::printf("[%s] %s\n", ok ? "PASS" : "FAIL", name);
    if (!ok) ++g_failures;
}

static bool refused(const std::string& text) {
    try {
        Value::parse(text);
        return false;
    } catch (const Value::Error&) {
        return true;
    }
}

static std::string round_trip(const std::string& text) { return Value::parse(text).dump(); }

static void test_the_spellings_that_are_compared() {
    const std::string nested = "{\"a\":{\"b\":[1,2]},\"c\":\"x\"}";
    check("compact dump has no spaces", round_trip(nested) == nested);
    check("indented dump is the record's own form",
          Value::parse(nested).dump(2) ==
              "{\n  \"a\": {\n    \"b\": [\n      1,\n      2\n    ]\n  },\n  \"c\": \"x\"\n}");
    check("empty containers stay on one line",
          Value::parse("{\"a\":{},\"b\":[]}").dump(2) == "{\n  \"a\": {},\n  \"b\": []\n}");
    check("keys keep the order they arrived in",
          round_trip("{\"zebra\":1,\"apple\":2}") == "{\"zebra\":1,\"apple\":2}");

    // The opaque half of a record is somebody else's to spell. Parsing 1.50
    // into a double and printing 1.5 is one value with two spellings, in a file
    // three implementations diff.
    const std::string numbers = "[1.50,1e10,-0,0.0001,123456789012345678]";
    check("numbers are written back exactly as they arrived", round_trip(numbers) == numbers);
    check("a number with a fraction is not an integer",
          !Value::parse("1.0").is_number_integer() && Value::parse("1").is_number_integer());
}

static void test_strings() {
    check("the escapes JSON defines all decode",
          Value::parse("\"a\\\"b\\\\c\\/d\\b\\f\\n\\r\\te\"").get<std::string>() ==
              std::string("a\"b\\c/d\b\f\n\r\te"));
    check("a solidus comes back unescaped", round_trip("\"a\\/b\"") == "\"a/b\"");
    check("a control character is escaped on the way out",
          round_trip("\"\\u0007\"") == "\"\\u0007\"");
    check("a raw control character is refused", refused("\"a\nb\""));
    check("a surrogate pair becomes one character",
          Value::parse("\"\\ud83d\\ude00\"").get<std::string>() == "\xf0\x9f\x98\x80");
    check("a lone high surrogate is refused", refused("\"\\ud83d\""));
    check("a lone low surrogate is refused", refused("\"\\udc00\""));
    check("a short \\u escape is refused", refused("\"\\u12\""));
    check("an unknown escape is refused", refused("\"\\x\""));
    check("an unterminated string is refused", refused("\"abc"));

    check("valid UTF-8 passes through untouched", round_trip("\"\xc3\xa9\"") == "\"\xc3\xa9\"");
    check("an overlong encoding is refused", refused("\"\xc0\x80\""));
    check("a surrogate spelled in UTF-8 is refused", refused("\"\xed\xa0\x80\""));
    check("a code point past U+10FFFF is refused", refused("\"\xf4\x90\x80\x80\""));
    check("a truncated sequence is refused", refused("\"\xe2\x82\""));
}

static void test_what_a_document_may_not_be() {
    check("trailing content is refused", refused("{} {}"));
    check("a truncated object is refused", refused("{\"a\":1"));
    check("a trailing comma is refused", refused("[1,2,]"));
    check("a leading zero is refused", refused("01"));
    check("a bare fraction is refused", refused(".5"));
    check("an exponent with no digits is refused", refused("1e"));
    check("nothing at all is refused", refused(""));

    // Go keeps the last, Python keeps the last, the library that used to sit
    // here kept the first. One record meaning three things to three readers is
    // the shape of every parser-differential attack, so it is refused instead.
    check("a repeated key is refused", refused("{\"a\":1,\"a\":2}"));

    std::string deep;
    for (int i = 0; i < 130; ++i) deep += "[";
    check("input nested past the cap is refused rather than walked off the stack",
          refused(deep));
    std::string fine;
    for (int i = 0; i < 100; ++i) fine += "[";
    fine += "1";
    for (int i = 0; i < 100; ++i) fine += "]";
    check("a hundred levels is still read", !refused(fine));
}

static void test_the_accessors() {
    const Value d = Value::parse("{\"s\":\"x\",\"n\":7,\"b\":true,\"a\":[\"p\",\"q\"]}");
    check("get<std::string>", d.at("s").get<std::string>() == "x");
    check("get<std::int64_t>", d.at("n").get<std::int64_t>() == 7);
    check("get<bool>", d.at("b").get<bool>());
    check("get<std::vector<std::string>>",
          d.at("a").get<std::vector<std::string>>() == std::vector<std::string>({"p", "q"}));
    check("a missing key throws rather than appearing", !d.contains("nope"));

    bool threw = false;
    try {
        Value::parse("99999999999999999999").get<std::int64_t>();
    } catch (const Value::Error&) {
        threw = true;
    }
    check("an integer too large for int64 throws rather than truncating", threw);

    // A caller asking a yes/no question about an endpoint's answer must not get
    // an exception through it.
    bool ok = true;
    const Value bad = Value::parse("{this is not json", &ok);
    check("the non-throwing parse says no and returns null", !ok && bad.is_null());

    Value built = Value::object();
    built["k"] = 1;
    built["k"] = 2;
    check("assigning a key twice replaces rather than repeats", built.dump() == "{\"k\":2}");
    built.erase("k");
    check("erase removes the key and its value", built.dump() == "{}");

    check("equality is structural", Value::parse("{\"a\":[1]}") == Value::parse("{\"a\":[1]}"));
    check("two spellings of one number are not equal",
          Value::parse("1.0") != Value::parse("1"));
}

// These four lines are the whole of what the byte-for-byte record comparison
// rests on, and nothing in its corpus reaches any of them. Go's encoder escapes
// & < > and U+2028; Python's escapes every character above ASCII; this one
// escapes none of them. All three write a record as dump(2) + "\n" and
// scripts/conformance.sh diffs the bytes, so the first record carrying a query
// string or an accented filename settles which of the three is wrong. Pinned
// here so the disagreement is read off a test run rather than off an adopter.
static void test_the_escapes_the_three_writers_do_not_agree_on() {
    check("an ampersand is written raw, where Go writes \\u0026",
          round_trip("{\"u\":\"a&b\"}") == "{\"u\":\"a&b\"}");
    check("an angle bracket is written raw, where Go writes \\u003c",
          round_trip("\"<\"") == "\"<\"");
    check("e-acute is written raw, where Python writes \\u00e9",
          round_trip("\"\xc3\xa9\"") == "\"\xc3\xa9\"");
    check("U+2028 is written raw, where Go and Python both escape it",
          round_trip("\"\xe2\x80\xa8\"") == "\"\xe2\x80\xa8\"");
}

// What a parser of hostile bytes owes its caller beyond a verdict on the
// document: that the work it does is proportional to the input, that what it
// writes it can read, and that nothing the input chose leaves in an error
// message. Each of the three below failed before it was written.
static void test_the_work_an_attacker_chooses() {
    const std::size_t keys = 150000;
    std::string many = "{";
    for (std::size_t i = 0; i < keys; ++i) {
        if (i > 0) many += ',';
        many += "\"k" + std::to_string(i) + "\":0";
    }
    many += '}';
    const auto started = std::chrono::steady_clock::now();
    bool ok = false;
    const Value v = Value::parse(many, &ok);
    const double took =
        std::chrono::duration<double>(std::chrono::steady_clock::now() - started).count();
    check("a hundred and fifty thousand keys cost that, not the square of it",
          ok && v.size() == keys && took < 5.0);

    const std::string repeated = many.substr(0, many.size() - 1) + ",\"k0\":1}";
    check("a key repeated at the far end of a huge object is still refused", refused(repeated));

    const std::string long_key(1u << 20, 'k');
    const std::string wide = "{\"" + long_key + "\":\"" + long_key + "\"}";
    check("a megabyte key and a megabyte value survive a round trip", round_trip(wide) == wide);
}

static void test_the_writer_and_the_parser_agree() {
    // A Value is not only ever built by the parser. A path or an OS error
    // message arrives as bytes, and on Windows those bytes are whatever code
    // page the machine is set to. Writing them through produced a record on
    // disk that this parser then refused to read: the store, not the input,
    // became unopenable.
    Value r = Value::object();
    r["path"] = std::string("C:\\models\\caf\xe9.gguf");
    r[std::string("k\xed\xa0\x80")] = 1;
    bool ok = false;
    const Value back = Value::parse(r.dump(2), &ok);
    check("a record built from bytes that are not UTF-8 still reads back", ok);
    check("those bytes became the replacement character, one for one",
          ok && back.at("path").get<std::string>() == "C:\\models\\caf\xef\xbf\xbd.gguf" &&
              back.contains("k\xef\xbf\xbd\xef\xbf\xbd\xef\xbf\xbd"));

    bool clean = false;
    try {
        Value::parse("{\"a\\nb\":1,\"a\\nb\":2}");
    } catch (const Value::Error& e) {
        clean = std::string(e.what()).find('\n') == std::string::npos;
    }
    check("a refusal does not carry a control character out of the input", clean);
}

static std::string boxes(int n, const char* core) {
    return std::string(static_cast<std::size_t>(n), '[') + core +
           std::string(static_cast<std::size_t>(n), ']');
}

static void test_where_the_depth_cap_actually_falls() {
    // kMaxDepth is 128 and the test is `depth > kMaxDepth` from a top level of
    // 0, so a document may nest 128 containers around a value — and 129 when
    // the innermost is empty, because an empty container is read without
    // descending into it. Pinned because a cap nobody has measured is a cap
    // nobody can change safely.
    check("a hundred and twenty-eight containers around a value are read",
          !refused(boxes(128, "1")));
    check("a hundred and twenty-nine are refused", refused(boxes(129, "1")));
    check("an empty innermost container reaches exactly one level further",
          !refused(boxes(129, "")) && refused(boxes(130, "")));
    const std::string deepest = boxes(128, "1");
    check("the deepest document that is read can be written and read again",
          round_trip(round_trip(deepest)) == round_trip(deepest));
}

// Nobody writes the input that breaks a parser by hand, so the input is
// generated: every document of three bytes or fewer, every four-symbol word over
// the alphabet a JSON parser branches on, every prefix of a small corpus, and
// mutations of it. One invariant holds over all of them — whatever is accepted
// must survive dump and re-read as the same value — and a byte that crashes,
// hangs or launders a value fails the run here rather than on an adopter's
// machine. ABSTRACTION_JSON_FUZZ scales the generated part up for a longer hunt.

static int g_fuzz_problems = 0;
static std::string g_first_problem;

static std::string spelled(const std::string& in) {
    std::string out;
    for (const char raw : in) {
        const unsigned char c = static_cast<unsigned char>(raw);
        if (c >= 0x20 && c < 0x7f) {
            out.push_back(raw);
        } else {
            static const char* kHex = "0123456789abcdef";
            out += "\\x";
            out.push_back(kHex[c >> 4]);
            out.push_back(kHex[c & 0x0F]);
        }
    }
    return out;
}

static void fuzz_problem(const char* what, const std::string& in) {
    if (g_fuzz_problems++ == 0) {
        g_first_problem = std::string(what) + " on <" + spelled(in) + ">";
    }
}

static void one_generated_input(const std::string& in) {
    bool ok = false;
    const Value v = Value::parse(in, &ok);
    if (!ok) {
        if (!v.is_null()) fuzz_problem("a refusal returned a value", in);
        return;
    }
    const std::string compact = v.dump();
    bool reread = false;
    const Value back = Value::parse(compact, &reread);
    if (!reread) {
        fuzz_problem("a compact dump does not read back", in);
    } else if (!(back == v)) {
        fuzz_problem("a round trip changed the value", in);
    } else if (back.dump() != compact) {
        fuzz_problem("a second dump differs from the first", in);
    }
    const std::string spaced = v.dump(2);
    bool wide = false;
    const Value indented = Value::parse(spaced, &wide);
    if (!wide || !(indented == v)) {
        fuzz_problem("an indented dump does not read back", in);
    }
}

static const char kAlphabet[] = {'{',  '}',    '[',    ']',    '"',    ':',    ',',  '\\',
                                 'u',  '0',    '1',    '9',    '-',    '.',    'e',  '+',
                                 't',  'r',    'f',    'a',    'l',    's',    'n',  ' ',
                                 '\n', '\0',   '\x7f', '\x80', '\xc0', '\xc2', '\xe0',
                                 '\xed', '\xf0', '\xf4', '\xff', 'd',  '8',    'A',  '/',
                                 'b'};

static void every_short_document(int longest) {
    std::string buf;
    for (int len = 0; len <= longest; ++len) {
        buf.assign(static_cast<std::size_t>(len), '\0');
        for (;;) {
            one_generated_input(buf);
            int i = len - 1;
            while (i >= 0 && static_cast<unsigned char>(buf[i]) == 0xff) {
                buf[i] = '\0';
                --i;
            }
            if (i < 0) break;
            buf[i] = static_cast<char>(static_cast<unsigned char>(buf[i]) + 1);
        }
    }
}

static void every_word_over_the_alphabet(int longest) {
    const int symbols = static_cast<int>(sizeof(kAlphabet));
    for (int len = 1; len <= longest; ++len) {
        std::vector<int> pick(static_cast<std::size_t>(len), 0);
        std::string buf(static_cast<std::size_t>(len), '\0');
        for (;;) {
            for (int i = 0; i < len; ++i) {
                buf[static_cast<std::size_t>(i)] = kAlphabet[pick[static_cast<std::size_t>(i)]];
            }
            one_generated_input(buf);
            int i = len - 1;
            while (i >= 0 && pick[static_cast<std::size_t>(i)] == symbols - 1) {
                pick[static_cast<std::size_t>(i)] = 0;
                --i;
            }
            if (i < 0) break;
            ++pick[static_cast<std::size_t>(i)];
        }
    }
}

static const std::vector<std::string>& corpus() {
    static const std::vector<std::string> seeds = {
        "{\"a\":1,\"b\":[1,2,{\"c\":null}],\"d\":\"\\u00e9\"}",
        "[true,false,null,-0.5e+3,\"\\ud83d\\ude00\"]",
        "\"\xc3\xa9\xe2\x82\xac\xf0\x9f\x98\x80\"",
        "{\"\":{},\"x\":[]}",
        "[[[[[[[[[[1]]]]]]]]]]",
        "\"\\u0000\\u001f\\\"\\\\\\/\"",
        "1e10",
        "{\"k\":\"v\"}",
    };
    return seeds;
}

static void every_prefix_of_the_corpus() {
    for (const std::string& seed : corpus()) {
        for (std::size_t n = 0; n <= seed.size(); ++n) {
            one_generated_input(seed.substr(0, n));
        }
    }
}

static void mutations_of_the_corpus(long rounds) {
    std::uint64_t state = 0x9E3779B97F4A7C15ull;
    const std::vector<std::string>& seeds = corpus();
    auto roll = [&state]() {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        return state;
    };
    for (long r = 0; r < rounds; ++r) {
        std::string s = seeds[roll() % seeds.size()];
        const int edits = 1 + static_cast<int>(roll() % 6);
        for (int e = 0; e < edits && !s.empty(); ++e) {
            const std::size_t at = roll() % s.size();
            switch (roll() % 4) {
                case 0: s[at] = static_cast<char>(roll() & 0xff); break;
                case 1: s.erase(at, 1); break;
                case 2: s.insert(at, 1, kAlphabet[roll() % sizeof(kAlphabet)]); break;
                default: s.insert(at, seeds[roll() % seeds.size()]); break;
            }
            if (s.size() > 4096) s.resize(4096);
        }
        one_generated_input(s);
    }
}

static void test_generated_input() {
    const char* scale_text = std::getenv("ABSTRACTION_JSON_FUZZ");
    const long scale = scale_text == nullptr ? 1 : std::strtol(scale_text, nullptr, 10);
    every_short_document(scale > 1 ? 3 : 2);
    every_prefix_of_the_corpus();
    every_word_over_the_alphabet(scale > 2 ? 5 : scale > 1 ? 4 : 3);
    mutations_of_the_corpus(scale > 1 ? scale * 50000 : 20000);
    check(g_fuzz_problems == 0 ? "generated input is read, written and read back unchanged"
                               : g_first_problem.c_str(),
          g_fuzz_problems == 0);
}

int main() {
    std::setvbuf(stdout, nullptr, _IONBF, 0);

    test_the_spellings_that_are_compared();
    test_strings();
    test_what_a_document_may_not_be();
    test_the_accessors();
    test_the_escapes_the_three_writers_do_not_agree_on();
    test_the_work_an_attacker_chooses();
    test_the_writer_and_the_parser_agree();
    test_where_the_depth_cap_actually_falls();
    test_generated_input();

    std::printf("\n%s: %d failure(s)\n", g_failures == 0 ? "OK" : "FAILED", g_failures);
    return g_failures == 0 ? 0 : 1;
}
