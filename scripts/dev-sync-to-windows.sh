#!/usr/bin/env bash
# Dev-time real-time sync from WSL native dev path to Windows-visible mirror.
# Run in background during development:
#   nohup bash scripts/dev-sync-to-windows.sh > /tmp/llm-pool-sync.log 2>&1 &

set -euo pipefail

SRC=/home/12/llm-pool
DST=/mnt/d/Code/R3_Code/MI/MI_test_account
DEBOUNCE_MS=400

EXCLUDES=(
  --exclude='.git/'
  --exclude='/data/'
  --exclude='/dist/'
  --exclude='/bin/'
  --exclude='*.log'
  --exclude='*.swp'
  --exclude='*.tmp'
  --exclude='node_modules/'
)

sync_now() {
  if rsync -a --delete "${EXCLUDES[@]}" "$SRC/" "$DST/" 2>&1; then
    echo "[$(date +%H:%M:%S)] synced -> $DST"
  else
    echo "[$(date +%H:%M:%S)] sync FAILED" >&2
  fi
}

echo "[$(date +%H:%M:%S)] dev-sync started: $SRC -> $DST"
sync_now

# inotifywait emits one line per event. We consume them with a small debounce:
# a simple pattern that pulls events with a read timeout — if no event for
# DEBOUNCE_MS, we sync; otherwise keep coalescing.
inotifywait -mrq \
  -e modify,create,delete,move \
  --exclude '(\.git/|/data/|/dist/|/bin/|\.log$|\.swp$|\.tmp$|node_modules/)' \
  --format '%w%f' \
  "$SRC" > /tmp/llm-pool-sync.events &
WATCHER_PID=$!
trap "kill $WATCHER_PID 2>/dev/null || true" EXIT

# Tail the events file with a debounce.
LAST_EVENT=0
while sleep 0.2; do
  if [[ -s /tmp/llm-pool-sync.events ]]; then
    : > /tmp/llm-pool-sync.events
    LAST_EVENT=$(date +%s%N)
  fi
  if (( LAST_EVENT > 0 )); then
    NOW=$(date +%s%N)
    if (( (NOW - LAST_EVENT) / 1000000 >= DEBOUNCE_MS )); then
      sync_now
      LAST_EVENT=0
    fi
  fi
done
