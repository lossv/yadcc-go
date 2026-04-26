#!/usr/bin/env bash
# start-daemon.sh — Start one yadcc-daemon process (local + servant roles).
#
# Usage:
#   ./scripts/start-daemon.sh [options]
#
# Environment variables (all optional):
#   YADCC_SCHEDULER_GRPC_ADDR   Scheduler gRPC address, e.g. 10.0.0.1:8336
#                               (default: 127.0.0.1:8336)
#   YADCC_DAEMON_LOCAL_ADDR     HTTP listen address for the wrapper API
#                               (default: 127.0.0.1:8334)
#   YADCC_DAEMON_SERVANT_ADDR   gRPC listen address for remote compile tasks
#                               (default: 0.0.0.0:8335)
#   YADCC_DAEMON_PRIORITY       servant_priority: "user" or "dedicated"
#                               (default: user)
#   YADCC_CACHE_GRPC_ADDR       External yadcc-cache gRPC address (optional)
#   YADCC_TOKEN                 Authentication token (default: yadcc)
#   YADCC_BIN_DIR               Directory containing yadcc binaries
#                               (default: <repo>/bin)
#   YADCC_LOG_DIR               Directory for log files
#                               (default: /tmp/yadcc-logs)
#
# The daemon is started in the background.  Its PID is written to
# $YADCC_LOG_DIR/yadcc-daemon.pid.
#
# On a build-farm node, set YADCC_DAEMON_PRIORITY=dedicated so the machine
# donates most of its CPUs to remote compilation tasks.
# On a developer workstation, leave it as "user" (default).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SCHEDULER_ADDR="${YADCC_SCHEDULER_GRPC_ADDR:-127.0.0.1:8336}"
LOCAL_ADDR="${YADCC_DAEMON_LOCAL_ADDR:-127.0.0.1:8334}"
SERVANT_ADDR="${YADCC_DAEMON_SERVANT_ADDR:-0.0.0.0:8335}"
PRIORITY="${YADCC_DAEMON_PRIORITY:-user}"
CACHE_ADDR="${YADCC_CACHE_GRPC_ADDR:-}"
TOKEN="${YADCC_TOKEN:-yadcc}"
BIN_DIR="${YADCC_BIN_DIR:-$REPO_ROOT/bin}"
LOG_DIR="${YADCC_LOG_DIR:-/tmp/yadcc-logs}"

DAEMON_BIN="$BIN_DIR/yadcc-daemon"
PID_FILE="$LOG_DIR/yadcc-daemon.pid"
LOG_FILE="$LOG_DIR/yadcc-daemon.log"

# ---- sanity checks ----
if [[ ! -x "$DAEMON_BIN" ]]; then
    echo "[error] yadcc-daemon binary not found: $DAEMON_BIN" >&2
    echo "        Run 'make build' or set YADCC_BIN_DIR." >&2
    exit 1
fi

mkdir -p "$LOG_DIR"

if [[ -f "$PID_FILE" ]]; then
    EXISTING_PID="$(cat "$PID_FILE")"
    if kill -0 "$EXISTING_PID" 2>/dev/null; then
        echo "[info] yadcc-daemon is already running (pid $EXISTING_PID)"
        exit 0
    fi
    rm -f "$PID_FILE"
fi

# ---- build argument list ----
ARGS=(
    "--local_addr=$LOCAL_ADDR"
    "--servant_addr=$SERVANT_ADDR"
    "--scheduler_uri=$SCHEDULER_ADDR"
    "--servant_priority=$PRIORITY"
    "--token=$TOKEN"
)
if [[ -n "$CACHE_ADDR" ]]; then
    ARGS+=("--cache_addr=$CACHE_ADDR")
fi

# ---- start ----
echo "[info] Starting yadcc-daemon"
echo "[info]   local (HTTP) : $LOCAL_ADDR"
echo "[info]   servant (gRPC): $SERVANT_ADDR"
echo "[info]   scheduler    : $SCHEDULER_ADDR"
echo "[info]   priority     : $PRIORITY"
[[ -n "$CACHE_ADDR" ]] && echo "[info]   cache        : $CACHE_ADDR"
echo "[info]   log          : $LOG_FILE"

"$DAEMON_BIN" "${ARGS[@]}" >>"$LOG_FILE" 2>&1 &

DAEMON_PID=$!
echo "$DAEMON_PID" > "$PID_FILE"
echo "[info] yadcc-daemon started (pid $DAEMON_PID)"

# ---- wait until the local HTTP endpoint is ready ----
LOCAL_HOST="${LOCAL_ADDR/0.0.0.0/127.0.0.1}"
DEADLINE=$(( $(date +%s) + 15 ))
while [[ $(date +%s) -lt $DEADLINE ]]; do
    if curl -sf "http://$LOCAL_HOST/healthz" >/dev/null 2>&1; then
        echo "[info] yadcc-daemon is ready"
        exit 0
    fi
    sleep 0.2
done
echo "[warn] yadcc-daemon did not become healthy within 15 s — check $LOG_FILE"
exit 1
