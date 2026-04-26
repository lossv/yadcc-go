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
export YADCC_DAEMON_ADDR="http://$DAEMON_LOCAL"
exec "$YADCC_BIN" "$REAL_CC" "\$@"
WEOF
chmod +x "$WRAPPER_DIR/cc"

cat >"$WRAPPER_DIR/c++" <<WEOF
#!/usr/bin/env bash
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

echo ""
echo "============================================"
echo " CMake test PASSED"
echo "   First build  : ${FIRST_MS} ms  (cache cold)"
echo "   Second build : ${SECOND_MS} ms  (cache warm)"
echo "============================================"
