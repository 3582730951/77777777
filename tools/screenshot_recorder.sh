#!/bin/bash
# 定时截图记录器 - 每3秒截一次屏
DIR="/tmp/browser_screenshots"
mkdir -p "$DIR"
LOG="/tmp/browser_record.log"
echo "[$(date +%H:%M:%S)] 开始记录截图 → $DIR" | tee "$LOG"
i=0
while true; do
    f="$DIR/$(printf '%04d' $i)_$(date +%H%M%S).png"
    DISPLAY=:99 import -window root "$f" 2>/dev/null
    echo "[$(date +%H:%M:%S)] 截图 #$i → $f" >> "$LOG"
    i=$((i+1))
    sleep 3
done
