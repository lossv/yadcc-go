#!/usr/bin/env bash
# start-scheduler.sh — Start the yadcc-scheduler process.
#
# Usage:
#   ./scripts/start-scheduler.sh [options]
#
# Environment variables (all optional):
#   YADCC_SCHEDULER_GRPC_ADDR   gRPC listen address  (default: 0.0.0.0:8336)
#   YADCC_SCHEDULER_HTTP_ADDR   HTTP debug address   (default: 0.0.0.0:8337)
#   YADCC_BIN_DIR               directory containing yadcc binaries
#                               (default: <repo>/bin)
#   YADCC_LOG_DIR               directory for log files
#                               (default: /tmp/yadcc-logs)
#
# The scheduler is started in the background.  Its PID is written to
# $YADCC_LOG_DIR/yadcc-scheduler.pid.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

GRPC_ADDR="${YADCC_SCHEDULER_GRPC_ADDR:-0.0.0.0:8336}"
HTTP_ADDR="${YADCC_SCHEDULER_HTTP_ADDR:-0.0.0.0:8337}"
BIN_DIR="${YADCC_BIN_DIR:-$REPO_ROOT/bin}"
LOG_DIR="${YADCC_LOG_DIR:-/tmp/yadcc-logs}"

SCHEDULER_BIN="$BIN_DIR/yadcc-scheduler"
PID_FILE="$LOG_DIR/yadcc-scheduler.pid"
LOG_FILE="$LOG_DIR/yadcc-scheduler.log"

# ---- sanity checks ----
if [[ ! -x "$SCHEDULER_BIN" ]]; then
    echo "[error] yadcc-scheduler binary not found: $SCHEDULER_BIN" >&2
    echo "        Run 'make build' or set YADCC_BIN_DIR." >&2
    exit 1
fi

mkdir -p "$LOG_DIR"

if [[ -f "$PID_FILE" ]]; then
    EXISTING_PID="$(cat "$PID_FILE")"
    if kill -0 "$EXISTING_PID" 2>/dev/null; then
        echo "[info] yadcc-scheduler is already running (pid $EXISTING_PID)"
        exit 0
    fi
    rm -f "$PID_FILE"
fi

# ---- start ----
echo "[info] Starting yadcc-scheduler"
echo "[info]   gRPC  : $GRPC_ADDR"
echo "[info]   HTTP  : $HTTP_ADDR"
echo "[info]   log   : $LOG_FILE"

"$SCHEDULER_BIN" \
    --addr="$GRPC_ADDR" \
    --http-addr="$HTTP_ADDR" \
    >>"$LOG_FILE" 2>&1 &

SCHEDULER_PID=$!
echo "$SCHEDULER_PID" > "$PID_FILE"
echo "[info] yadcc-scheduler started (pid $SCHEDULER_PID)"

# ---- wait until it is ready ----
HTTP_HOST="${HTTP_ADDR/0.0.0.0/127.0.0.1}"
DEADLINE=$(( $(date +%s) + 10 ))
while [[ $(date +%s) -lt $DEADLINE ]]; do
    if curl -sf "http://$HTTP_HOST/healthz" >/dev/null 2>&1; then
        echo "[info] yadcc-scheduler is ready"
        exit 0
    fi
    sleep 0.2
done
echo "[warn] yadcc-scheduler did not become healthy within 10 s — check $LOG_FILE"
exit 1
