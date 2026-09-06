#pragma once

// A JSON document, and nothing more than these layers need one for: read a
// shape we define, carry a shape we do not, and write both back byte for byte.
//
// WHY THIS EXISTS RATHER THAN A LIBRARY. This header is published. Anything it
// includes, every adopter of the job layer acquires — in their include path, in
// their translation units, and in whatever their build system has to fetch to
// get it. A facade that hands its adopters a dependency they did not choose is
// not a facade. The general-purpose library that used to sit here was 25,000
// lines reached by a git clone at configure time; what it was asked to do is
// below.
//
// Numbers are kept as the literal text they arrived as and written back
// unchanged. `spec`, `checkpoint` and `extensions` are opaque to us, so their
// numbers are somebody else's to spell; parsing 1.50 into a double and printing
// 1.5 is how one value comes to have two spellings in a record three
// implementations compare byte for byte.

#include <cstdint>
#include <initializer_list>
#include <stdexcept>
#include <string>
#include <vector>

namespace abstraction {
namespace json {

struct Reader;

class Value {
public:
    struct Member;

    class Error : public std::runtime_error {
    public:
        explicit Error(const std::string& what) : std::runtime_error(what) {}
    };

    enum class Type { Null, Boolean, Number, String, Array, Object };

    Value() = default;
    Value(bool b);
    Value(int n);
    Value(std::int64_t n);
    Value(const char* s);
    Value(std::string s);
    Value(const std::vector<std::string>& strings);
    Value(std::initializer_list<Member> members);

    static Value object();
    static Value array();
    static Value array(std::initializer_list<Value> items);

    // A null `ok` throws on malformed input. A caller asking a yes/no question
    // about bytes from whatever managed to bind an endpoint passes one instead,
    // and gets a null Value back rather than an exception through its answer.
    static Value parse(const std::string& text, bool* ok = nullptr);

    Type type() const { return type_; }
    bool is_null() const { return type_ == Type::Null; }
    bool is_boolean() const { return type_ == Type::Boolean; }
    bool is_number() const { return type_ == Type::Number; }
    bool is_number_integer() const;
    bool is_string() const { return type_ == Type::String; }
    bool is_array() const { return type_ == Type::Array; }
    bool is_object() const { return type_ == Type::Object; }

    std::size_t size() const;
    bool empty() const { return size() == 0; }
    bool contains(const std::string& key) const;

    const Value& at(const std::string& key) const;
    const Value& at(std::size_t index) const;
    const Value& operator[](const std::string& key) const { return at(key); }
    Value& operator[](const std::string& key);

    void push_back(Value v);
    void erase(const std::string& key);

    template <typename T>
    T get() const;

    // A negative indent is the one-line form. Anything else is that many spaces
    // per level, with no trailing newline — the caller owns that byte.
    std::string dump(int indent = -1) const;

    class const_iterator {
    public:
        const_iterator(const Value* owner, std::size_t at) : owner_(owner), at_(at) {}
        bool operator==(const const_iterator& o) const { return at_ == o.at_; }
        bool operator!=(const const_iterator& o) const { return at_ != o.at_; }
        const_iterator& operator++() {
            ++at_;
            return *this;
        }
        const Value& operator*() const { return owner_->at(at_); }
        const Value* operator->() const { return &owner_->at(at_); }
        const Value& value() const { return owner_->at(at_); }
        const std::string& key() const;

    private:
        const Value* owner_;
        std::size_t at_;
    };

    const_iterator begin() const { return const_iterator(this, 0); }
    const_iterator end() const { return const_iterator(this, size()); }

private:
    friend struct Reader;
    // Two numbers are equal when they are spelled the same, because a spelling
    // is what this carries and what three implementations are compared on.
    friend bool operator==(const Value& a, const Value& b);

    void write(std::string& out, int indent, int level) const;

    Type type_ = Type::Null;
    bool boolean_ = false;
    std::string text_;
    std::vector<std::string> keys_;
    std::vector<Value> items_;
};

struct Value::Member {
    std::string key;
    Value value;
};

bool operator==(const Value& a, const Value& b);
inline bool operator!=(const Value& a, const Value& b) { return !(a == b); }

template <>
std::string Value::get<std::string>() const;
template <>
std::int64_t Value::get<std::int64_t>() const;
template <>
int Value::get<int>() const;
template <>
bool Value::get<bool>() const;
template <>
std::vector<std::string> Value::get<std::vector<std::string>>() const;

}  // namespace json
}  // namespace abstraction
