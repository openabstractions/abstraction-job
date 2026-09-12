// Standalone generated vocabulary check; no C++ server or provider.
#include "../abstraction/job/acceptance/rec.h"
#include <iostream>
#include <iterator>

int main() {
    const std::string input{std::istreambuf_iterator<char>(std::cin), {}};
    try {
        const auto value = abstraction::job::acceptance::decode(input);
        std::cout << abstraction::job::acceptance::encode(value);
        return 0;
    } catch (const std::exception& error) {
        std::cerr << error.what() << '\n';
        return 1;
    }
}
