#include <iostream>
#include <string>
#include "mathlib.h"

// Forward declaration of the C++ helper defined in mathlib_cpp.cpp
std::string describe(int n);
int safe_divv(int a, int b);

int main() {
    std::cout << "yadcc CMake test\n";
    std::cout << "add(3,4)  = " << add(3, 4) << "\n";
    std::cout << "sub(10,3) = " << sub(10, 3) << "\n";
    std::cout << "mul(6,7)  = " << mul(6, 7) << "\n";
    std::cout << "divv(9,3) = " << divv(9, 3) << "\n";
    for (int i = 0; i <= 10; i++) {
        std::cout << describe(i) << "\n";
    }
    try {
        safe_divv(1, 0);
    } catch (const std::invalid_argument& e) {
        std::cout << "caught: " << e.what() << "\n";
    }
    return 0;
}
