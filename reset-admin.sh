#!/usr/bin/env bash
# ============================================================================
#  重置 LLM Pool 管理员密码
#  用法：在 VPS 上运行  bash reset-admin.sh
#  原理：清空 admin_users 表 → 重启 gateway → bootstrapAdmin 自动生成新密码
# ============================================================================
set -euo pipefail

bold() { printf "\033[1m%s\033[0m\n" "$*"; }
info() { printf "\033[36m[i]\033[0m %s\n" "$*"; }
ok()   { printf "\033[32m[✓]\033[0m %s\n" "$*"; }
warn() { printf "\033[33m[!]\033[0m %s\n" "$*"; }
err()  { printf "\033[31m[✗]\033[0m %s\n" "$*"; exit 1; }

INSTALL_DIR=${INSTALL_DIR:-/opt/llm-pool}
CONFIG_FILE="$INSTALL_DIR/config/config.yaml"

# 从 config.yaml 读取 db_path
get_db_path() {
    local raw
    raw=$(grep 'db_path' "$CONFIG_FILE" 2>/dev/null | head -1 | sed 's/.*db_path:\s*//' | tr -d '"' | tr -d "'" | xargs)
    [[ -z "$raw" ]] && raw="data/pool.db"
    # 相对路径转绝对路径
    [[ "$raw" != /* ]] && raw="$INSTALL_DIR/$raw"
    echo "$raw"
}

get_passwd_path() {
    local raw
    raw=$(grep 'passwd_path' "$CONFIG_FILE" 2>/dev/null | head -1 | sed 's/.*passwd_path:\s*//' | tr -d '"' | tr -d "'" | xargs)
    [[ -z "$raw" ]] && raw="data/passwd.txt"
    [[ "$raw" != /* ]] && raw="$INSTALL_DIR/$raw"
    echo "$raw"
}

bold "=========================================="
bold "   LLM Pool — 重置管理员密码"
bold "=========================================="

DB_PATH=$(get_db_path)
PASSWD_PATH=$(get_passwd_path)

info "数据库: $DB_PATH"
info "密码文件: $PASSWD_PATH"

[[ ! -f "$DB_PATH" ]] && err "数据库文件不存在: $DB_PATH"

echo
read -r -p "确认重置管理员密码？旧密码将失效。[y/N]: " yn
[[ "${yn:-N}" =~ ^[Yy]$ ]] || { warn "已取消"; exit 0; }

# 方法 1：用 sqlite3 CLI
if command -v sqlite3 &>/dev/null; then
    info "使用 sqlite3 清空 admin_users 表..."
    sqlite3 "$DB_PATH" "DELETE FROM admin_users;"
    ok "admin_users 已清空"

# 方法 2：用 Go 写一个一次性��具
elif command -v go &>/dev/null || [[ -f /usr/local/go/bin/go ]]; then
    export PATH="$PATH:/usr/local/go/bin"
    info "sqlite3 CLI 不可用，使用 Go 临时工具..."
    local_tmp=$(mktemp -d)
    cat > "$local_tmp/reset.go" <<'GOCODE'
package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: reset <db_path>")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", os.Args[1]+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM admin_users"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("admin_users cleared")
}
GOCODE
    # 复用项目�� go.mod
    cp "$INSTALL_DIR/go.mod" "$local_tmp/" 2>/dev/null || true
    cp "$INSTALL_DIR/go.sum" "$local_tmp/" 2>/dev/null || true
    cd "$local_tmp"
    go run reset.go "$DB_PATH"
    cd - &>/dev/null
    rm -rf "$local_tmp"
    ok "admin_users 已清空"

# 方法 3：都没有，直接删密码文件让 gateway 重建
else
    warn "sqlite3 和 Go 都不可用，尝试直接删除密码文件..."
    warn "注意：这种方式需要同时清空数据库中的 admin 记录才能生效"
    warn "建议先安装 sqlite3:  apt-get install sqlite3"
    err "无法重置，请���装 sqlite3 后重试"
fi

# 删除旧密码文件，让 bootstrapAdmin 重新生成
if [[ -f "$PASSWD_PATH" ]]; then
    mv "$PASSWD_PATH" "${PASSWD_PATH}.old.$(date +%s)"
    info "旧密码文件已备份为 ${PASSWD_PATH}.old.*"
fi

# 重启服务
info "重启 gateway 以生成新密码..."
if systemctl is-active llm-pool &>/dev/null 2>&1; then
    systemctl restart llm-pool
    sleep 2
elif docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^llm-pool$'; then
    cd "$INSTALL_DIR" && docker compose restart
    sleep 3
elif [[ -f "$INSTALL_DIR/bin/gateway" ]]; then
    pkill -f "$INSTALL_DIR/bin/gateway" 2>/dev/null || true
    sleep 1
    nohup "$INSTALL_DIR/bin/gateway" -config "$CONFIG_FILE" >> "$INSTALL_DIR/data/gateway.log" 2>&1 &
    sleep 2
fi

# 读取新密码
sleep 1
if [[ -f "$PASSWD_PATH" ]]; then
    NEW_PASS=$(head -1 "$PASSWD_PATH" | cut -d: -f2)
    echo
    bold "=========================================="
    bold "   重置成功"
    bold "=========================================="
    printf "  用户名: admin\n"
    printf "  新密码: %s\n" "$NEW_PASS"
    printf "  密码文件: %s\n" "$PASSWD_PATH"
    bold "=========================================="
else
    echo
    warn "密码文件尚未生成，服务可能还在启动中"
    info "稍后查看: cat $PASSWD_PATH"
fi
