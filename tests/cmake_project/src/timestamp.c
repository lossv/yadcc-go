/* timestamp.c — deliberately uses __TIME__ and __DATE__ to exercise the
 * non-cacheable compilation path in yadcc.  The build result of this file
 * should never be served from cache. */
#include <stdio.h>

int main(void) {
    printf("Built on %s at %s\n", __DATE__, __TIME__);
    return 0;
}
