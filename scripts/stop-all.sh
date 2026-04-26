#!/usr/bin/env bash
# stop-all.sh — Stop all yadcc processes started by the start-*.sh scripts.
#
# Usage:
#   ./scripts/stop-all.sh
#
# Environment variables:
#   YADCC_LOG_DIR   directory where PID files are stored (default: /tmp/yadcc-logs)
set -euo pipefail

LOG_DIR="${YADCC_LOG_DIR:-/tmp/yadcc-logs}"

stop_pid_file() {
    local name="$1"
    local pid_file="$LOG_DIR/$name.pid"
    if [[ ! -f "$pid_file" ]]; then
        echo "[info] $name: no PID file, skipping"
        return
    fi
    local pid
    pid="$(cat "$pid_file")"
    if kill -0 "$pid" 2>/dev/null; then
        echo "[info] Stopping $name (pid $pid)…"
        kill "$pid"
        # Wait up to 5 s for graceful exit.
        local i
        for i in $(seq 1 25); do
            if ! kill -0 "$pid" 2>/dev/null; then
                echo "[info] $name stopped"
                rm -f "$pid_file"
                return
            fi
            sleep 0.2
        done
        echo "[warn] $name did not stop cleanly; sending SIGKILL"
        kill -9 "$pid" 2>/dev/null || true
        rm -f "$pid_file"
    else
        echo "[info] $name (pid $pid) is not running"
        rm -f "$pid_file"
    fi
}

stop_pid_file "yadcc-daemon"
stop_pid_file "yadcc-scheduler"
stop_pid_file "yadcc-cache"
