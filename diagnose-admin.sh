#!/usr/bin/env bash
# 诊断管理员登录问题
set -euo pipefail

bold() { printf "\033[1m%s\033[0m\n" "$*"; }
info() { printf "\033[36m[i]\033[0m %s\n" "$*"; }
ok()   { printf "\033[32m[✓]\033[0m %s\n" "$*"; }
warn() { printf "\033[33m[!]\033[0m %s\n" "$*"; }

INSTALL_DIR=${INSTALL_DIR:-/opt/llm-pool}

bold "======= 管理员登录诊断 ======="
echo

# 1. gateway 进程实际使用的 config
bold "── 1. Gateway 进程状态 ──"
if pgrep -af gateway 2>/dev/null; then
    info "进程命令行："
    ps aux | grep '[g]ateway' | head -3
    # 提取 -config 参数
    ACTUAL_CONFIG=$(ps aux | grep '[g]ateway' | grep -oP '(?<=-config )\S+' | head -1)
    [[ -n "$ACTUAL_CONFIG" ]] && info "实际使用的 config: $ACTUAL_CONFIG" || warn "无法提取 -config 参数"
else
    warn "gateway 进程未运行"
    ACTUAL_CONFIG=""
fi

# systemd 中的配置
if [[ -f /etc/systemd/system/llm-pool.service ]]; then
    info "systemd ExecStart:"
    grep 'ExecStart' /etc/systemd/system/llm-pool.service
    info "systemd WorkingDirectory:"
    grep 'WorkingDirectory' /etc/systemd/system/llm-pool.service
fi

echo
bold "── 2. Config 文件内容 ──"
for cfg in "$ACTUAL_CONFIG" "$INSTALL_DIR/config/config.yaml" "/opt/llm-pool/config/config.yaml"; do
    [[ -z "$cfg" ]] && continue
    if [[ -f "$cfg" ]]; then
        info "文件: $cfg"
        echo "  db_path:     $(grep 'db_path' "$cfg" | head -1 | sed 's/.*db_path:\s*//' | tr -d '\"'"'")"
        echo "  passwd_path: $(grep 'passwd_path' "$cfg" | head -1 | sed 's/.*passwd_path:\s*//' | tr -d '\"'"'")"
        echo "  master_key:  $(grep 'master_key' "$cfg" | head -1 | sed 's/.*master_key:\s*//' | tr -d '\"'"'" | head -c 10)..."
        echo "  gateway_addr:$(grep 'gateway_addr' "$cfg" | head -1 | sed 's/.*gateway_addr:\s*//' | tr -d '\"'"'")"
        echo "  admin_addr:  $(grep 'admin_addr' "$cfg" | head -1 | sed 's/.*admin_addr:\s*//' | tr -d '\"'"'")"
    else
        warn "文件不存在: $cfg"
    fi
done

echo
bold "── 3. 数据库文件 ──"
# 找所有可能的 pool.db
info "查找所有 pool.db:"
find / -name "pool.db" -type f 2>/dev/null | while read -r f; do
    sz=$(du -h "$f" | cut -f1)
    mod=$(stat -c '%Y' "$f" 2>/dev/null || echo "?")
    date_str=$(date -d "@$mod" '+%m-%d %H:%M' 2>/dev/null || echo "?")
    printf "  %s  (%s, modified %s)\n" "$f" "$sz" "$date_str"
done

echo
bold "── 4. admin_users 表内容 ──"
# 尝试查询每个找到的 DB
find / -name "pool.db" -type f 2>/dev/null | while read -r db; do
    info "检查: $db"
    if command -v sqlite3 &>/dev/null; then
        count=$(sqlite3 "$db" "SELECT COUNT(*) FROM admin_users;" 2>/dev/null || echo "ERROR")
        echo "  admin_users 行数: $count"
        if [[ "$count" != "0" ]] && [[ "$count" != "ERROR" ]]; then
            sqlite3 "$db" "SELECT username, substr(password_hash,1,20)||'...', datetime(created_at,'unixepoch') FROM admin_users;" 2>/dev/null | while read -r row; do
                echo "  → $row"
            done
        fi
    else
        warn "  sqlite3 未安装，无法查询"
    fi
done

echo
bold "── 5. passwd.txt 文件 ──"
find / -name "passwd.txt" -path "*/llm-pool/*" -o -name "passwd.txt" -path "*/data/*" 2>/dev/null | while read -r f; do
    info "文件: $f"
    echo "  内容第一行: $(head -1 "$f" 2>/dev/null)"
    mod=$(stat -c '%Y' "$f" 2>/dev/null || echo "?")
    date_str=$(date -d "@$mod" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo "?")
    echo "  最后修改: $date_str"
done

echo
bold "── 6. 验证密码是否匹配 ──"
# 从 passwd.txt 提取密码，然后和 DB 里的 hash 比对
PASSWD_FILE=""
DB_FILE=""
CONFIG="${ACTUAL_CONFIG:-$INSTALL_DIR/config/config.yaml}"
if [[ -f "$CONFIG" ]]; then
    raw_db=$(grep 'db_path' "$CONFIG" | head -1 | sed 's/.*db_path:\s*//' | tr -d '\"'"'" | xargs)
    raw_pw=$(grep 'passwd_path' "$CONFIG" | head -1 | sed 's/.*passwd_path:\s*//' | tr -d '\"'"'" | xargs)
    # 解析工作目录
    WORKDIR="$INSTALL_DIR"
    if [[ -f /etc/systemd/system/llm-pool.service ]]; then
        wd=$(grep 'WorkingDirectory' /etc/systemd/system/llm-pool.service | sed 's/.*=//' | xargs)
        [[ -n "$wd" ]] && WORKDIR="$wd"
    fi
    # 转绝对路径
    [[ "$raw_db" != /* ]] && raw_db="$WORKDIR/$raw_db"
    [[ "$raw_pw" != /* ]] && raw_pw="$WORKDIR/$raw_pw"
    DB_FILE="$raw_db"
    PASSWD_FILE="$raw_pw"
fi

if [[ -n "$PASSWD_FILE" ]] && [[ -f "$PASSWD_FILE" ]]; then
    PLAIN_PASS=$(head -1 "$PASSWD_FILE" | cut -d: -f2)
    info "passwd.txt 中的密码: $PLAIN_PASS"
else
    warn "找不到 passwd.txt"
fi

if [[ -n "$DB_FILE" ]] && [[ -f "$DB_FILE" ]] && command -v sqlite3 &>/dev/null; then
    HASH=$(sqlite3 "$DB_FILE" "SELECT password_hash FROM admin_users WHERE username='admin';" 2>/dev/null || echo "")
    if [[ -z "$HASH" ]]; then
        warn "DB 中没有 admin 用户！bootstrap 可能没有运行"
    else
        info "DB 中的 hash: ${HASH:0:30}..."
    fi
fi

echo
bold "── 7. 最近 gateway 日志 ──"
if systemctl is-active llm-pool &>/dev/null 2>&1; then
    journalctl -u llm-pool -n 20 --no-pager 2>/dev/null | grep -iE 'admin|bootstrap|passwd|error|fatal' | tail -10
elif [[ -f "$INSTALL_DIR/data/gateway.log" ]]; then
    tail -20 "$INSTALL_DIR/data/gateway.log" | grep -iE 'admin|bootstrap|passwd|error|fatal' | tail -10
fi

echo
bold "======= 诊断完成 ======="
echo "把以上输出发给我，我帮你定位问题。"
