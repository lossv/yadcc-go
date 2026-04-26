cmake_minimum_required(VERSION 3.14)
project(yadcc_test C CXX)

set(CMAKE_C_STANDARD 11)
set(CMAKE_CXX_STANDARD 17)

# ---- Library: a small math library ----
add_library(mathlib STATIC
    src/mathlib.c
    src/mathlib_cpp.cpp
)
target_include_directories(mathlib PUBLIC src)

# ---- Executable: uses the library ----
add_executable(hello src/main.cpp)
target_link_libraries(hello PRIVATE mathlib)

# ---- Executable: contains __TIME__ / __DATE__ (non-cacheable path) ----
add_executable(timestamp_demo src/timestamp.c)

# ---- CTest ----
enable_testing()

add_test(NAME hello_runs
    COMMAND hello
)
add_test(NAME timestamp_runs
    COMMAND timestamp_demo
)
