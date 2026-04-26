/* mathlib.h — simple integer math helpers used by the yadcc CMake test */
#pragma once

#ifdef __cplusplus
extern "C" {
#endif

int add(int a, int b);
int sub(int a, int b);
int mul(int a, int b);
/* Returns -1 on division by zero */
int divv(int a, int b);

long long fib(int n);

#ifdef __cplusplus
}
#endif
