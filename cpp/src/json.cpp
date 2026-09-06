#include <abstraction/json/value.h>

#include <cerrno>
#include <cstdlib>
#include <limits>
#include <set>

namespace abstraction {
namespace json {

namespace {

// Deep enough for any record anyone has written and shallow enough that the
// recursive descent below cannot be walked off the stack by twenty bytes of
// hostile input. A parser reading a file another machine wrote has no other
// defence against `[[[[[[...`.
constexpr int kMaxDepth = 128;

bool is_digit(char c) { return c >= '0' && c <= '9'; }

void append_utf8(std::string& out, unsigned int cp) {
    if (cp < 0x80) {
        out.push_back(static_cast<char>(cp));
    } else if (cp < 0x800) {
        out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else if (cp < 0x10000) {
        out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else {
        out.push_back(static_cast<char>(0xF0 | (cp >> 18)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    }
}

std::size_t utf8_run(const std::string& s, std::size_t at) {
    const unsigned char c = static_cast<unsigned char>(s[at]);
    std::size_t extra = 0;
    if (c < 0x80) {
        return 1;
    } else if (c >= 0xC2 && c <= 0xDF) {
        extra = 1;
    } else if (c >= 0xE0 && c <= 0xEF) {
        extra = 2;
    } else if (c >= 0xF0 && c <= 0xF4) {
        extra = 3;
    } else {
        return 0;
    }
    if (at + extra >= s.size()) {
        return 0;
    }
    // Narrowing the FIRST continuation byte is what rejects an overlong
    // encoding, a lone surrogate spelled in UTF-8, and anything past U+10FFFF.
    // Accepting 0x80..0xBF here admits all three.
    unsigned char low = 0x80;
    unsigned char high = 0xBF;
    if (c == 0xE0) {
        low = 0xA0;
    } else if (c == 0xED) {
        high = 0x9F;
    } else if (c == 0xF0) {
        low = 0x90;
    } else if (c == 0xF4) {
        high = 0x8F;
    }
    for (std::size_t k = 1; k <= extra; ++k) {
        const unsigned char b = static_cast<unsigned char>(s[at + k]);
        if (b < low || b > high) {
            return 0;
        }
        low = 0x80;
        high = 0xBF;
    }
    return extra + 1;
}

void escape_into(std::string& out, const std::string& s) {
    static const char* kHex = "0123456789abcdef";
    out.push_back('"');
    std::size_t i = 0;
    while (i < s.size()) {
        const unsigned char c = static_cast<unsigned char>(s[i]);
        if (c >= 0x80) {
            // A Value is not only ever built by the parser. A path or an OS
            // message arrives as whatever bytes the machine's code page uses,
            // and written through they would put a record on disk that this
            // parser then refuses to read back.
            const std::size_t run = utf8_run(s, i);
            if (run == 0) {
                append_utf8(out, 0xFFFD);
                ++i;
            } else {
                out.append(s, i, run);
                i += run;
            }
            continue;
        }
        switch (c) {
            case '"': out += "\\\""; break;
            case '\\': out += "\\\\"; break;
            case '\b': out += "\\b"; break;
            case '\f': out += "\\f"; break;
            case '\n': out += "\\n"; break;
            case '\r': out += "\\r"; break;
            case '\t': out += "\\t"; break;
            default:
                if (c < 0x20) {
                    out += "\\u00";
                    out.push_back(kHex[c >> 4]);
                    out.push_back(kHex[c & 0x0F]);
                } else {
                    out.push_back(static_cast<char>(c));
                }
                break;
        }
        ++i;
    }
    out.push_back('"');
}

}  // namespace

struct Reader {
    explicit Reader(const std::string& text) : s(text) {}

    Value read() {
        skip_space();
        Value v = read_value(0);
        skip_space();
        if (at < s.size()) {
            fail("trailing content after the document");
        }
        return v;
    }

    const std::string& s;
    std::size_t at = 0;

    [[noreturn]] void fail(const std::string& why) const {
        throw Value::Error(why + " at byte " + std::to_string(at));
    }

    char peek() const { return at < s.size() ? s[at] : '\0'; }

    void skip_space() {
        while (at < s.size() && (s[at] == ' ' || s[at] == '\t' || s[at] == '\n' || s[at] == '\r')) {
            ++at;
        }
    }

    void expect_word(const char* word) {
        for (const char* p = word; *p != '\0'; ++p, ++at) {
            if (at >= s.size() || s[at] != *p) {
                fail(std::string("expected ") + word);
            }
        }
    }

    Value read_value(int depth) {
        if (depth > kMaxDepth) {
            fail("nested deeper than " + std::to_string(kMaxDepth));
        }
        switch (peek()) {
            case '{': return read_object(depth);
            case '[': return read_array(depth);
            case '"': return Value(read_string());
            case 't': expect_word("true"); return Value(true);
            case 'f': expect_word("false"); return Value(false);
            case 'n': expect_word("null"); return Value();
            default: return read_number();
        }
    }

    Value read_object(int depth) {
        ++at;
        Value out = Value::object();
        skip_space();
        if (peek() == '}') {
            ++at;
            return out;
        }
        // Beside the object rather than asked of it: `contains` walks every key
        // read so far, so this cost the square of a size the input picks.
        // Ordered, not hashed — a hash an attacker can collide is the same trap.
        std::set<std::string> seen;
        for (;;) {
            skip_space();
            std::string key = read_string();
            skip_space();
            if (peek() != ':') {
                fail("expected ':' after a key");
            }
            ++at;
            skip_space();
            Value v = read_value(depth + 1);
            // Refused rather than resolved. Go keeps the last, Python keeps the
            // last, the library that used to sit here kept the first: a record
            // with a repeated key means something different to each reader, and
            // that is the shape of every parser-differential attack.
            if (!seen.insert(key).second) {
                // Escaped: the key is the input's, and this goes to a log.
                std::string named;
                escape_into(named, key);
                fail("duplicate key " + named);
            }
            out.keys_.push_back(std::move(key));
            out.items_.push_back(std::move(v));
            skip_space();
            if (peek() == ',') {
                ++at;
                continue;
            }
            if (peek() == '}') {
                ++at;
                return out;
            }
            fail("expected ',' or '}'");
        }
    }

    Value read_array(int depth) {
        ++at;
        Value out = Value::array();
        skip_space();
        if (peek() == ']') {
            ++at;
            return out;
        }
        for (;;) {
            skip_space();
            out.items_.push_back(read_value(depth + 1));
            skip_space();
            if (peek() == ',') {
                ++at;
                continue;
            }
            if (peek() == ']') {
                ++at;
                return out;
            }
            fail("expected ',' or ']'");
        }
    }

    Value read_number() {
        const std::size_t start = at;
        if (peek() == '-') {
            ++at;
        }
        if (peek() == '0') {
            ++at;
        } else if (peek() >= '1' && peek() <= '9') {
            while (is_digit(peek())) {
                ++at;
            }
        } else {
            fail("expected a value");
        }
        if (peek() == '.') {
            ++at;
            if (!is_digit(peek())) {
                fail("a fraction needs a digit");
            }
            while (is_digit(peek())) {
                ++at;
            }
        }
        if (peek() == 'e' || peek() == 'E') {
            ++at;
            if (peek() == '+' || peek() == '-') {
                ++at;
            }
            if (!is_digit(peek())) {
                fail("an exponent needs a digit");
            }
            while (is_digit(peek())) {
                ++at;
            }
        }
        Value out;
        out.type_ = Value::Type::Number;
        out.text_ = s.substr(start, at - start);
        return out;
    }

    std::string read_string() {
        if (peek() != '"') {
            fail("expected a string");
        }
        ++at;
        std::string out;
        for (;;) {
            if (at >= s.size()) {
                fail("unterminated string");
            }
            const unsigned char c = static_cast<unsigned char>(s[at]);
            if (c == '"') {
                ++at;
                return out;
            }
            if (c < 0x20) {
                fail("a control character in a string must be escaped");
            }
            if (c == '\\') {
                ++at;
                read_escape(out);
                continue;
            }
            copy_utf8(out);
        }
    }

    void copy_utf8(std::string& out) {
        const std::size_t run = utf8_run(s, at);
        if (run == 0) {
            fail("not valid UTF-8");
        }
        out.append(s, at, run);
        at += run;
    }

    unsigned int read_hex4() {
        unsigned int v = 0;
        for (int k = 0; k < 4; ++k, ++at) {
            if (at >= s.size()) {
                fail("a \\u escape needs four hex digits");
            }
            const char c = s[at];
            if (is_digit(c)) {
                v = v * 16 + static_cast<unsigned int>(c - '0');
            } else if (c >= 'a' && c <= 'f') {
                v = v * 16 + static_cast<unsigned int>(c - 'a' + 10);
            } else if (c >= 'A' && c <= 'F') {
                v = v * 16 + static_cast<unsigned int>(c - 'A' + 10);
            } else {
                fail("a \\u escape needs four hex digits");
            }
        }
        return v;
    }

    void read_escape(std::string& out) {
        if (at >= s.size()) {
            fail("unterminated escape");
        }
        const char c = s[at++];
        switch (c) {
            case '"': out.push_back('"'); return;
            case '\\': out.push_back('\\'); return;
            case '/': out.push_back('/'); return;
            case 'b': out.push_back('\b'); return;
            case 'f': out.push_back('\f'); return;
            case 'n': out.push_back('\n'); return;
            case 'r': out.push_back('\r'); return;
            case 't': out.push_back('\t'); return;
            case 'u': break;
            default: fail("unknown escape");
        }
        unsigned int cp = read_hex4();
        if (cp >= 0xD800 && cp <= 0xDBFF) {
            if (at + 1 >= s.size() || s[at] != '\\' || s[at + 1] != 'u') {
                fail("a high surrogate with no low one after it");
            }
            at += 2;
            const unsigned int low = read_hex4();
            if (low < 0xDC00 || low > 0xDFFF) {
                fail("a high surrogate with no low one after it");
            }
            cp = 0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00);
        } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
            fail("a low surrogate with no high one before it");
        }
        append_utf8(out, cp);
    }
};

Value::Value(bool b) : type_(Type::Boolean), boolean_(b) {}

Value::Value(int n) : type_(Type::Number), text_(std::to_string(n)) {}

Value::Value(std::int64_t n) : type_(Type::Number), text_(std::to_string(n)) {}

Value::Value(const char* s) : type_(Type::String), text_(s == nullptr ? "" : s) {}

Value::Value(std::string s) : type_(Type::String), text_(std::move(s)) {}

Value::Value(const std::vector<std::string>& strings) : type_(Type::Array) {
    items_.reserve(strings.size());
    for (const std::string& one : strings) {
        items_.push_back(Value(one));
    }
}

Value::Value(std::initializer_list<Member> members) : type_(Type::Object) {
    for (const Member& m : members) {
        (*this)[m.key] = m.value;
    }
}

Value Value::object() {
    Value v;
    v.type_ = Type::Object;
    return v;
}

Value Value::array() {
    Value v;
    v.type_ = Type::Array;
    return v;
}

Value Value::array(std::initializer_list<Value> items) {
    Value v = array();
    v.items_.assign(items.begin(), items.end());
    return v;
}

Value Value::parse(const std::string& text, bool* ok) {
    if (ok == nullptr) {
        return Reader(text).read();
    }
    try {
        Value v = Reader(text).read();
        *ok = true;
        return v;
    } catch (const Error&) {
        *ok = false;
        return Value();
    }
}

bool Value::is_number_integer() const {
    return type_ == Type::Number &&
           text_.find_first_of(".eE") == std::string::npos;
}

std::size_t Value::size() const {
    switch (type_) {
        case Type::Null: return 0;
        case Type::Array:
        case Type::Object: return items_.size();
        default: return 1;
    }
}

bool Value::contains(const std::string& key) const {
    if (type_ != Type::Object) {
        return false;
    }
    for (const std::string& k : keys_) {
        if (k == key) {
            return true;
        }
    }
    return false;
}

const Value& Value::at(const std::string& key) const {
    if (type_ == Type::Object) {
        for (std::size_t i = 0; i < keys_.size(); ++i) {
            if (keys_[i] == key) {
                return items_[i];
            }
        }
    }
    throw Error("no key \"" + key + "\"");
}

const Value& Value::at(std::size_t index) const {
    if (index >= items_.size()) {
        throw Error("index " + std::to_string(index) + " is past the end");
    }
    return items_[index];
}

Value& Value::operator[](const std::string& key) {
    if (type_ == Type::Null) {
        type_ = Type::Object;
    }
    if (type_ != Type::Object) {
        throw Error("cannot key into a value that is not an object");
    }
    for (std::size_t i = 0; i < keys_.size(); ++i) {
        if (keys_[i] == key) {
            return items_[i];
        }
    }
    keys_.push_back(key);
    items_.push_back(Value());
    return items_.back();
}

void Value::push_back(Value v) {
    if (type_ == Type::Null) {
        type_ = Type::Array;
    }
    if (type_ != Type::Array) {
        throw Error("cannot append to a value that is not an array");
    }
    items_.push_back(std::move(v));
}

void Value::erase(const std::string& key) {
    if (type_ != Type::Object) {
        return;
    }
    for (std::size_t i = 0; i < keys_.size(); ++i) {
        if (keys_[i] == key) {
            keys_.erase(keys_.begin() + static_cast<std::ptrdiff_t>(i));
            items_.erase(items_.begin() + static_cast<std::ptrdiff_t>(i));
            return;
        }
    }
}

const std::string& Value::const_iterator::key() const {
    if (owner_->type_ != Type::Object || at_ >= owner_->keys_.size()) {
        throw Error("only an object's members have keys");
    }
    return owner_->keys_[at_];
}

template <>
std::string Value::get<std::string>() const {
    if (type_ != Type::String) {
        throw Error("not a string");
    }
    return text_;
}

template <>
std::int64_t Value::get<std::int64_t>() const {
    if (!is_number_integer()) {
        throw Error("not an integer");
    }
    errno = 0;
    char* end = nullptr;
    const long long n = std::strtoll(text_.c_str(), &end, 10);
    if (errno == ERANGE || end != text_.c_str() + text_.size()) {
        throw Error("integer out of range: " + text_);
    }
    return static_cast<std::int64_t>(n);
}

template <>
int Value::get<int>() const {
    const std::int64_t n = get<std::int64_t>();
    if (n < std::numeric_limits<int>::min() || n > std::numeric_limits<int>::max()) {
        throw Error("integer out of range: " + text_);
    }
    return static_cast<int>(n);
}

template <>
bool Value::get<bool>() const {
    if (type_ != Type::Boolean) {
        throw Error("not a boolean");
    }
    return boolean_;
}

template <>
std::vector<std::string> Value::get<std::vector<std::string>>() const {
    if (type_ != Type::Array) {
        throw Error("not an array");
    }
    std::vector<std::string> out;
    out.reserve(items_.size());
    for (const Value& one : items_) {
        out.push_back(one.get<std::string>());
    }
    return out;
}

void Value::write(std::string& out, int indent, int level) const {
    switch (type_) {
        case Type::Null: out += "null"; return;
        case Type::Boolean: out += boolean_ ? "true" : "false"; return;
        case Type::Number: out += text_; return;
        case Type::String: escape_into(out, text_); return;
        default: break;
    }
    const bool object = type_ == Type::Object;
    if (items_.empty()) {
        out += object ? "{}" : "[]";
        return;
    }
    const std::string inner = indent < 0 ? "" : "\n" + std::string(static_cast<std::size_t>(indent * (level + 1)), ' ');
    const std::string outer = indent < 0 ? "" : "\n" + std::string(static_cast<std::size_t>(indent * level), ' ');
    out += object ? '{' : '[';
    for (std::size_t i = 0; i < items_.size(); ++i) {
        if (i > 0) {
            out += ',';
        }
        out += inner;
        if (object) {
            escape_into(out, keys_[i]);
            out += indent < 0 ? ":" : ": ";
        }
        items_[i].write(out, indent, level + 1);
    }
    out += outer;
    out += object ? '}' : ']';
}

bool operator==(const Value& a, const Value& b) {
    if (a.type_ != b.type_) {
        return false;
    }
    switch (a.type_) {
        case Value::Type::Null: return true;
        case Value::Type::Boolean: return a.boolean_ == b.boolean_;
        case Value::Type::Number:
        case Value::Type::String: return a.text_ == b.text_;
        case Value::Type::Array: return a.items_ == b.items_;
        default: return a.keys_ == b.keys_ && a.items_ == b.items_;
    }
}

std::string Value::dump(int indent) const {
    std::string out;
    write(out, indent, 0);
    return out;
}

}  // namespace json
}  // namespace abstraction
