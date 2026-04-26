#include "mathlib.h"
#include <stdexcept>
#include <string>

// C++ wrapper that throws on divide-by-zero.
int safe_divv(int a, int b) {
    if (b == 0) throw std::invalid_argument("division by zero");
    return divv(a, b);
}

std::string describe(int n) {
    return "fib(" + std::to_string(n) + ") = " + std::to_string(fib(n));
}
