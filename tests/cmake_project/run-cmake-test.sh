#!/usr/bin/env bash
# run-cmake-test.sh — End-to-end test: build a real CMake project through yadcc.
#
# Usage:
#   cd <repo>/yadcc-go
#   ./tests/cmake_project/run-cmake-test.sh
#
# What it does:
#   1. Builds yadcc binaries (make build).
#   2. Starts yadcc-scheduler and yadcc-daemon (both on localhost).
#   3. Configures & builds the cmake_project using yadcc as the C/C++ compiler.
#   4. Runs ctest to verify the binaries work.
#   5. Rebuilds (after `make clean`) to verify cache hits.
#   6. Stops the yadcc stack.
#
# Verification (the key assertions this script makes):
#   A. Wrapper was actually invoked by cmake — checked via a touch-file sentinel.
#   B. Daemon received at least one compile task — checked via Prometheus metrics.
#   C. Second build produced cache hits — checked via Prometheus metrics.
#
# Prerequisites: cmake, a system C/C++ compiler (gcc or clang), curl.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

BIN_DIR="$REPO_ROOT/bin"
LOG_DIR="/tmp/yadcc-cmake-test-$$"
BUILD_DIR="/tmp/yadcc-cmake-build-$$"

export YADCC_BIN_DIR="$BIN_DIR"
export YADCC_LOG_DIR="$LOG_DIR"

# Use high port numbers to avoid collisions with a production yadcc stack.
SCHED_GRPC="127.0.0.1:19336"
SCHED_HTTP="127.0.0.1:19337"
DAEMON_LOCAL="127.0.0.1:19334"
DAEMON_SERVANT="127.0.0.1:19335"

export YADCC_SCHEDULER_GRPC_ADDR="$SCHED_GRPC"
export YADCC_SCHEDULER_HTTP_ADDR="$SCHED_HTTP"
export YADCC_DAEMON_LOCAL_ADDR="$DAEMON_LOCAL"
export YADCC_DAEMON_SERVANT_ADDR="$DAEMON_SERVANT"
export YADCC_DAEMON_PRIORITY="dedicated"

# Sentinel file written by the wrapper scripts on every invocation.
# After the build we assert it exists to confirm cmake actually called yadcc.
WRAPPER_INVOKED_SENTINEL="$LOG_DIR/wrapper_was_invoked"

# ---------- metric helpers ----------

# read_metric <metric_name> <label_filter>
#   Reads the current value of a Prometheus counter/gauge from the daemon's
#   /metrics endpoint.  Returns 0 if the series does not exist yet.
#   <label_filter> is a literal substring to match inside the {} labels,
#   e.g. 'outcome="remote"'.
read_metric() {
    local name="$1" label="$2"
    curl -sf "http://$DAEMON_LOCAL/metrics" 2>/dev/null \
        | awk -v name="$name" -v label="$label" '
            /^#/ { next }
            $0 ~ name && $0 ~ label { val=$NF }
            END { printf "%d\n", (val+0) }
        '
}

# assert_metric_gt <metric_name> <label_filter> <threshold> <description>
#   Fails the script if the metric value is not strictly greater than threshold.
assert_metric_gt() {
    local name="$1" label="$2" threshold="$3" desc="$4"
    local val
    val="$(read_metric "$name" "$label")"
    if (( val > threshold )); then
        echo "[ok]   $desc : $val (> $threshold)"
    else
        echo "[FAIL] $desc : got $val, expected > $threshold" >&2
        exit 1
    fi
}

# Portable millisecond timestamp (Linux gnu date supports %3N; macOS BSD does not).
now_ms() {
    python3 -c 'import time; print(int(time.time()*1000))' 2>/dev/null \
        || date +%s000
}

cleanup() {
    echo ""
    echo "==> Stopping yadcc stack..."
    bash "$REPO_ROOT/scripts/stop-all.sh" || true
    rm -rf "$LOG_DIR" "$BUILD_DIR"
}
trap cleanup EXIT INT TERM

# ---------- 1. Build yadcc binaries ----------
echo "==> Building yadcc binaries..."
make -C "$REPO_ROOT" build

# ---------- 2. Start yadcc stack ----------
echo "==> Starting yadcc-scheduler..."
bash "$REPO_ROOT/scripts/start-scheduler.sh"

echo "==> Starting yadcc-daemon..."
bash "$REPO_ROOT/scripts/start-daemon.sh"

# ---------- 3. Find the yadcc wrapper and a system compiler ----------
YADCC_BIN="$BIN_DIR/yadcc"

resolve_compiler() {
    local names=("$@")
    for name in "${names[@]}"; do
        local path
        path="$(command -v "$name" 2>/dev/null || true)"
        if [[ -n "$path" && "$(realpath "$path")" != "$(realpath "$YADCC_BIN")" ]]; then
            echo "$path"
            return
        fi
    done
    echo ""
}

REAL_CC="$(resolve_compiler gcc clang cc)"
REAL_CXX="$(resolve_compiler g++ clang++ c++)"

if [[ -z "$REAL_CC" || -z "$REAL_CXX" ]]; then
    echo "[error] No system C/C++ compiler found in PATH." >&2
    exit 1
fi

echo "[info] Real C   compiler : $REAL_CC"
echo "[info] Real C++ compiler : $REAL_CXX"
echo "[info] yadcc wrapper     : $YADCC_BIN"

# Create thin wrapper scripts that invoke yadcc with the real compiler.
# CMake sets CMAKE_*_COMPILER to these scripts.
WRAPPER_DIR="$LOG_DIR/wrappers"
mkdir -p "$WRAPPER_DIR"

cat >"$WRAPPER_DIR/cc" <<WEOF
#!/usr/bin/env bash
touch "$WRAPPER_INVOKED_SENTINEL"
export YADCC_DAEMON_ADDR="http://$DAEMON_LOCAL"
exec "$YADCC_BIN" "$REAL_CC" "\$@"
WEOF
chmod +x "$WRAPPER_DIR/cc"

cat >"$WRAPPER_DIR/c++" <<WEOF
#!/usr/bin/env bash
touch "$WRAPPER_INVOKED_SENTINEL"
export YADCC_DAEMON_ADDR="http://$DAEMON_LOCAL"
exec "$YADCC_BIN" "$REAL_CXX" "\$@"
WEOF
chmod +x "$WRAPPER_DIR/c++"

# ---------- 4. First cmake build (cache cold) ----------
echo ""
echo "==> Configuring CMake project (first build — cache cold)..."
mkdir -p "$BUILD_DIR"
cmake \
    -S "$SCRIPT_DIR" \
    -B "$BUILD_DIR" \
    -DCMAKE_C_COMPILER="$WRAPPER_DIR/cc" \
    -DCMAKE_CXX_COMPILER="$WRAPPER_DIR/c++" \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_VERBOSE_MAKEFILE=ON

echo "==> Building (first pass — cache cold)..."
T0="$(now_ms)"
cmake --build "$BUILD_DIR" -- -j"$(nproc 2>/dev/null || sysctl -n hw.logicalcpu)"
T1="$(now_ms)"
FIRST_MS=$(( T1 - T0 ))
echo "[info] First build took ${FIRST_MS} ms"

# ---------- Assertion A: wrapper was actually invoked ----------
echo ""
echo "==> Verifying wrapper was invoked by cmake..."
if [[ -f "$WRAPPER_INVOKED_SENTINEL" ]]; then
    echo "[ok]   Wrapper sentinel file exists — cmake called yadcc wrapper"
else
    echo "[FAIL] Wrapper sentinel file missing — cmake did NOT call the yadcc wrapper" >&2
    exit 1
fi

# ---------- Assertion B: daemon received compile tasks ----------
echo ""
echo "==> Verifying daemon processed compile tasks..."
# At least one task must have reached the daemon (remote, local fallback, or cache).
# We sum remote + local + cache outcomes by checking each; any non-zero count is enough.
TASKS_REMOTE="$(read_metric "yadcc_daemon_tasks_total" 'outcome="remote"')"
TASKS_LOCAL="$(read_metric  "yadcc_daemon_tasks_total" 'outcome="local"')"
TASKS_L1="$(read_metric     "yadcc_daemon_tasks_total" 'outcome="cache_hit_l1"')"
TASKS_L2="$(read_metric     "yadcc_daemon_tasks_total" 'outcome="cache_hit_l2"')"
TASKS_TOTAL=$(( TASKS_REMOTE + TASKS_LOCAL + TASKS_L1 + TASKS_L2 ))

echo "[info] daemon tasks — remote:${TASKS_REMOTE} local:${TASKS_LOCAL} cache_l1:${TASKS_L1} cache_l2:${TASKS_L2}"
if (( TASKS_TOTAL == 0 )); then
    echo "[FAIL] Daemon received zero compile tasks — wrapper did not reach daemon" >&2
    exit 1
fi
echo "[ok]   Daemon received ${TASKS_TOTAL} task(s) in first build"

# If a scheduler is running the single unified daemon acts as both client and
# worker, so remote tasks should be > 0.  On a single-node setup where the
# daemon has no scheduler, all tasks will be local — both are valid.
if (( TASKS_REMOTE > 0 )); then
    echo "[ok]   Remote path used (tasks dispatched via scheduler)"
else
    echo "[warn] No remote tasks — daemon ran all compilations locally (no scheduler available or tasks not distributable)"
fi

# ---------- 5. Run tests ----------
echo ""
echo "==> Running ctest..."
ctest --test-dir "$BUILD_DIR" --output-on-failure

# ---------- 6. Second build (cache warm) ----------
echo ""
echo "==> Cleaning object files for second build (cache warm)..."
cmake --build "$BUILD_DIR" --target clean

echo "==> Building (second pass — cache warm)..."
T2="$(now_ms)"
cmake --build "$BUILD_DIR" -- -j"$(nproc 2>/dev/null || sysctl -n hw.logicalcpu)"
T3="$(now_ms)"
SECOND_MS=$(( T3 - T2 ))
echo "[info] Second build took ${SECOND_MS} ms"

# ---------- Assertion C: second build used cache ----------
echo ""
echo "==> Verifying second build produced cache hits..."
assert_metric_gt \
    "yadcc_daemon_tasks_total" 'outcome="cache_hit_l1"' 0 \
    "L1 cache hits in second build"

echo ""
echo "============================================"
echo " CMake test PASSED"
echo "   First build  : ${FIRST_MS} ms  (cache cold)"
echo "   Second build : ${SECOND_MS} ms  (cache warm)"
echo "   Wrapper invoked    : yes"
echo "   Daemon tasks total : ${TASKS_TOTAL}"
echo "   L1 cache hits (2nd): $(read_metric "yadcc_daemon_tasks_total" 'outcome="cache_hit_l1"')"
echo "============================================"
