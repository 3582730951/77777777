#!/usr/bin/env bash
# ============================================================================
#  LLM Pool Gateway — 全量重编译升级脚本
#
#  用法：在源码根目录运行  bash upgrade.sh
#
#  核心原则：
#    ✓ 清除所有编译缓存，100% 从源码重编译
#    ✓ 清除 Go module 缓存并重新下载依赖
#    ✓ 清除 INSTALL_DIR 下旧源码残留（rsync --delete）
#    ✗ 绝不删除：config.yaml / pool.db / passwd.txt / 账号数据
#
#  保护清单（绝不触碰）：
#    config/config.yaml        用户配置（端口、密钥）
#    config/config.local.yaml  本地覆盖配置
#    data/pool.db              所有租户/账号/API Key 数据
#    data/pool.db-shm          SQLite 共享内存
#    data/pool.db-wal          SQLite WAL 日志
#    data/passwd.txt           管理员密码
#    docker-compose.yml        Docker 端口映射
# ============================================================================

set -euo pipefail

# ── 路径 ────────────────────────────────────────────────────────────────────
INSTALL_DIR=${INSTALL_DIR:-/opt/llm-pool}
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BINARY="$INSTALL_DIR/bin/gateway"
CONFIG_FILE="$INSTALL_DIR/config/config.yaml"
DB_FILE="$INSTALL_DIR/data/pool.db"
BACKUP_DIR="$INSTALL_DIR/backups"
LOG_FILE="$INSTALL_DIR/data/upgrade.log"

# ── 输出 ────────────────────────────────────────────────────────────────────
bold() { printf "\033[1m%s\033[0m\n" "$*"; }
info() { printf "\033[36m[i]\033[0m %s\n" "$*"; }
ok()   { printf "\033[32m[✓]\033[0m %s\n" "$*"; }
warn() { printf "\033[33m[!]\033[0m %s\n" "$*"; }
err()  { printf "\033[31m[✗]\033[0m %s\n" "$*"; }
die()  { err "$*"; exit 1; }
log()  { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >> "$LOG_FILE" 2>/dev/null || true; }
ask()  { local v; read -r -p "$1 [$2]: " v || true; echo "${v:-$2}"; }
ts()   { date '+%Y%m%d_%H%M%S'; }
generate_random() { head -c "${1:-32}" /dev/urandom | base64 | tr -d '/+=' | head -c "${1:-32}"; }
has_systemd() { command -v systemctl &>/dev/null && [[ -d /run/systemd/system ]]; }

# ── YAML 读取 ───────────────────────────────────────────────────────────────
read_yaml_val() {
    local key="$1" file="${2:-$CONFIG_FILE}"
    [[ -f "$file" ]] || return 1
    grep -E "^\s*${key}:" "$file" | head -1 | sed "s/.*${key}:\s*//" | tr -d '"' | tr -d "'" | xargs
}
parse_port() { echo "${1##*:}"; }
get_gateway_port() { local a; a=$(read_yaml_val "gateway_addr" 2>/dev/null || echo ""); [[ -n "$a" ]] && parse_port "$a" || echo "8787"; }
get_admin_port()   { local a; a=$(read_yaml_val "admin_addr"   2>/dev/null || echo ""); [[ -n "$a" ]] && parse_port "$a" || echo "8788"; }

# ── 最近一次备份目录 ────────────────────────────────────────────────────────
latest_backup() { ls -dt "$BACKUP_DIR"/*/ 2>/dev/null | head -1; }

# ── 部署模式检测 ────────────────────────────────────────────────────────────
detect_mode() {
    if has_systemd && (systemctl is-active llm-pool &>/dev/null 2>&1 || systemctl is-enabled llm-pool &>/dev/null 2>&1); then
        echo "systemd"
    elif docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q '^llm-pool$'; then
        echo "docker"
    elif [[ -f "$BINARY" ]]; then
        echo "binary"
    else
        echo "fresh"
    fi
}

# ── Go 环境 ─────────────────────────────────────────────────────────────────
ensure_go() {
    export PATH="$PATH:/usr/local/go/bin:/usr/lib/go/bin"
    if command -v go &>/dev/null; then
        local ver major minor
        ver=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | tr -d 'go')
        major=${ver%%.*}; minor=${ver##*.}
        if [[ "${major:-0}" -ge 1 ]] && [[ "${minor:-0}" -ge 25 ]]; then
            ok "Go $(go version | awk '{print $3}')"
            return 0
        fi
    fi

    info "安装 Go >= 1.25 ..."
    local arch; arch=$(uname -m)
    case "$arch" in x86_64) arch="amd64";; aarch64) arch="arm64";; *) die "不支持的架构: $arch";; esac

    local go_ver="1.25.3" go_file="go${go_ver}.linux-${arch}.tar.gz"
    local tmpf; tmpf=$(mktemp /tmp/go-XXXXXX.tar.gz)
    local ok_dl=false
    for url in "https://golang.google.cn/dl/${go_file}" "https://mirrors.aliyun.com/golang/${go_file}" "https://go.dev/dl/${go_file}"; do
        info "  下载: $url"
        if curl -fsSL --max-time 120 --retry 2 -o "$tmpf" "$url" 2>/dev/null && tar -tzf "$tmpf" &>/dev/null; then
            ok_dl=true; break
        fi
    done
    [[ "$ok_dl" != true ]] && { rm -f "$tmpf"; die "Go 下载失败，请手动安装"; }
    rm -rf /usr/local/go && tar -C /usr/local -xzf "$tmpf" && rm -f "$tmpf"
    export PATH="$PATH:/usr/local/go/bin"
    grep -q '/usr/local/go/bin' /etc/profile 2>/dev/null || echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
    ok "Go $(go version | awk '{print $3}') 已安装"
}

# ══════════════════════════════════════════════════════════════════════════════
#  Step 1: 备份（只备份关键数据，不备份源码）
# ══════════════════════════════════════════════════════════════════════════════
do_backup() {
    local bak="$BACKUP_DIR/$(ts)"
    mkdir -p "$bak"
    [[ -f "$BINARY" ]]                        && cp "$BINARY" "$bak/gateway.bak"          && ok "备份 binary"
    [[ -f "$DB_FILE" ]]                        && cp "$DB_FILE" "$bak/pool.db.bak"         && ok "备份 数据库"
    [[ -f "${DB_FILE}-wal" ]]                  && cp "${DB_FILE}-wal" "$bak/pool.db-wal.bak"
    [[ -f "$CONFIG_FILE" ]]                    && cp "$CONFIG_FILE" "$bak/config.yaml.bak" && ok "备份 配置"
    [[ -f "$INSTALL_DIR/data/passwd.txt" ]]    && cp "$INSTALL_DIR/data/passwd.txt" "$bak/passwd.txt.bak" && ok "备份 密码"
    # 保留最近 5 份
    ls -dt "$BACKUP_DIR"/*/ 2>/dev/null | tail -n +6 | xargs rm -rf 2>/dev/null || true
    echo "$bak"
}

# ══════════════════════════════════════════════════════════════════════════════
#  Step 2: 停止服务
# ══════════════════════════════════════════════════════════════════════════════
stop_service() {
    case "$1" in
        systemd) has_systemd && systemctl stop llm-pool 2>/dev/null || true ;;
        docker)  (cd "$INSTALL_DIR" && docker compose stop 2>/dev/null) || docker stop llm-pool 2>/dev/null || true ;;
        binary)  pkill -f "$BINARY" 2>/dev/null || true; sleep 1 ;;
    esac
    ok "服务已停止"
}

# ══════════════════════════════════════════════════════════════════════════════
#  Step 3: 同步源码（保护数据文件，清除旧残留）
# ══════════════════════════════════════════════════════════════════════════════
sync_source() {
    info "同步源码 → $INSTALL_DIR ..."
    mkdir -p "$INSTALL_DIR"/{bin,data,config,backups}

    # 记录受保护文件的 hash
    local cfg_hash="" db_hash="" pw_hash=""
    [[ -f "$CONFIG_FILE" ]]                     && cfg_hash=$(md5sum "$CONFIG_FILE" | cut -d' ' -f1)
    [[ -f "$DB_FILE" ]]                         && db_hash=$(md5sum "$DB_FILE" | cut -d' ' -f1)
    [[ -f "$INSTALL_DIR/data/passwd.txt" ]]     && pw_hash=$(md5sum "$INSTALL_DIR/data/passwd.txt" | cut -d' ' -f1)

    if command -v rsync &>/dev/null; then
        # rsync --delete 会删除 INSTALL_DIR 中源码不再存在的文件（清除残留），
        # 但 exclude 保护了数据/配置/备份不被删除。
        rsync -a --delete \
            --exclude='.git/' \
            --exclude='data/' \
            --exclude='backups/' \
            --exclude='config/config.yaml' \
            --exclude='config/config.local.yaml' \
            --exclude='bin/' \
            --exclude='docker-compose.yml' \
            --exclude='passwd.txt' \
            --exclude='*.db' --exclude='*.db-shm' --exclude='*.db-wal' \
            --exclude='*.log' \
            --exclude='*/node_modules/' \
            --exclude='*/.venv/' \
            --exclude='other-*/' \
            --exclude='other_*/' \
            "$SRC_DIR/" "$INSTALL_DIR/"
    else
        # 无 rsync 时：先清除旧源码目录再复制（确保无残留）
        for d in cmd internal tools scripts deploy docs release frontend services plan web testdata; do
            rm -rf "$INSTALL_DIR/$d" 2>/dev/null || true
            [[ -d "$SRC_DIR/$d" ]] && cp -r "$SRC_DIR/$d" "$INSTALL_DIR/"
        done
        for f in go.mod go.sum README.md .gitignore install.sh upgrade.sh Dockerfile; do
            [[ -f "$SRC_DIR/$f" ]] && cp "$SRC_DIR/$f" "$INSTALL_DIR/"
        done
        find "$INSTALL_DIR" -path '*/node_modules' -prune -exec rm -rf {} + 2>/dev/null || true
        find "$INSTALL_DIR" -path '*/.venv' -prune -exec rm -rf {} + 2>/dev/null || true
    fi

    [[ -f "$SRC_DIR/config/config.example.yaml" ]] && cp "$SRC_DIR/config/config.example.yaml" "$INSTALL_DIR/config/" 2>/dev/null || true

    # 验证受保护文件未被篡改（使用 latest_backup 而非通配符）
    local safe=true
    local latest_bak; latest_bak=$(latest_backup)
    if [[ -n "$cfg_hash" ]]; then
        local h; h=$(md5sum "$CONFIG_FILE" 2>/dev/null | cut -d' ' -f1)
        if [[ "$h" != "$cfg_hash" ]]; then
            err "config.yaml 被修改！正在恢复..."
            [[ -n "$latest_bak" && -f "$latest_bak/config.yaml.bak" ]] && cp "$latest_bak/config.yaml.bak" "$CONFIG_FILE"
            safe=false
        fi
    fi
    if [[ -n "$db_hash" ]] && [[ -f "$DB_FILE" ]]; then
        local h; h=$(md5sum "$DB_FILE" 2>/dev/null | cut -d' ' -f1)
        if [[ "$h" != "$db_hash" ]]; then
            err "pool.db 被修改！正在恢复..."
            [[ -n "$latest_bak" && -f "$latest_bak/pool.db.bak" ]] && cp "$latest_bak/pool.db.bak" "$DB_FILE"
            safe=false
        fi
    fi
    if [[ -n "$pw_hash" ]] && [[ -f "$INSTALL_DIR/data/passwd.txt" ]]; then
        local h; h=$(md5sum "$INSTALL_DIR/data/passwd.txt" 2>/dev/null | cut -d' ' -f1)
        if [[ "$h" != "$pw_hash" ]]; then
            err "passwd.txt 被修改！正在恢复..."
            [[ -n "$latest_bak" && -f "$latest_bak/passwd.txt.bak" ]] && cp "$latest_bak/passwd.txt.bak" "$INSTALL_DIR/data/passwd.txt"
            safe=false
        fi
    fi
    [[ "$safe" == true ]] && ok "源码已同步（受保护文件完整）" || warn "部分受保护文件已从备份恢复"
}

# ══════════════════════════════════════════════════════════════════════════════
#  Step 4: 全量清缓存 + 重编译
# ══════════════════════════════════════════════════════════════════════════════
full_rebuild() {
    ensure_go
    cd "$INSTALL_DIR"

    bold "── 清除所有缓存 ──"

    info "清除 Go 编译缓存 ..."
    go clean -cache -testcache 2>/dev/null || true
    ok "Go build cache 已清除"

    info "清除 Go module 缓存 ..."
    go clean -modcache 2>/dev/null || true
    ok "Go module cache 已清除"

    # 清除 GOPATH/pkg 残留（非默认 GOPATH 场景）
    local gopath; gopath=$(go env GOPATH 2>/dev/null || echo "$HOME/go")
    if [[ -d "$gopath/pkg/mod" ]]; then
        chmod -R u+w "$gopath/pkg/mod" 2>/dev/null || true
    fi

    # 删除旧 binary 和所有 bin/ 下的工具
    rm -rf "$INSTALL_DIR/bin/"* 2>/dev/null || true
    mkdir -p "$INSTALL_DIR/bin"
    ok "旧 binary 已清除"

    bold "── 重新下载依赖 ──"

    # 自动检测 Go proxy
    if ! curl -fsS --max-time 5 https://proxy.golang.org &>/dev/null 2>&1; then
        export GOPROXY="https://goproxy.cn,https://goproxy.io,direct"
        info "Go proxy: $GOPROXY"
    fi

    go mod download
    ok "依赖下载完成"

    go mod tidy 2>/dev/null || true

    bold "── 全量编译 ──"

    build_autoreg_spa

    local tmp_bin="$INSTALL_DIR/bin/gateway.new"

    info "CGO_ENABLED=1 go build -a -ldflags='-s -w' ..."
    if CGO_ENABLED=1 go build -a -ldflags="-s -w" -o "$tmp_bin" ./cmd/gateway/; then
        mv "$tmp_bin" "$BINARY"
        chmod +x "$BINARY"
        local size; size=$(du -h "$BINARY" | cut -f1)
        ok "编译成功 → $BINARY ($size)"
    else
        rm -f "$tmp_bin"
        return 1
    fi
    cd - &>/dev/null
}

build_autoreg_spa() {
    local frontend_src="$INSTALL_DIR/services/autoreg/frontend"
    local spa_out="$INSTALL_DIR/internal/admin/autoreg_spa"
    if [[ ! -d "$frontend_src" ]]; then
        warn "AutoReg 前端源码不存在，跳过 SPA 重建"
        return 0
    fi
    if ! command -v npm &>/dev/null; then
        warn "未找到 npm，跳过 AutoReg SPA 重建"
        return 0
    fi
    info "重建 AutoReg SPA ..."
    cd "$frontend_src"
    if [[ -f package-lock.json ]]; then
        npm ci --silent 2>&1 | tail -3
    else
        npm install --silent 2>&1 | tail -3
    fi
    VITE_API_BASE=/api/autoreg npx vite build --base=/autoreg/ --outDir "$spa_out" 2>&1 | tail -5
    cd "$INSTALL_DIR"
    ok "AutoReg SPA 已重建 → $spa_out"
}

# ══════════════════════════════════════════════════════════════════════════════
#  Step 5: 启动服务
# ══════════════════════════════════════════════════════════════════════════════
write_systemd_unit() {
    local prov="${1:-real}"
    local master_key_env=""
    # 从 config.yaml 读取 master_key 注入环境变量（如果存在）
    local mk; mk=$(read_yaml_val "master_key" 2>/dev/null || echo "")
    [[ -n "$mk" ]] && master_key_env="Environment=POOL_MASTER_KEY=${mk}"

    cat > /etc/systemd/system/llm-pool.service <<EOF
[Unit]
Description=LLM Pool Gateway
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$BINARY -config $CONFIG_FILE
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
Environment=PROVIDER_MODE=${prov}
${master_key_env}

[Install]
WantedBy=multi-user.target
EOF
}

start_service() {
    case "$1" in
        systemd)
            if has_systemd; then
                write_systemd_unit "real"
                systemctl daemon-reload
                systemctl start llm-pool
                ok "systemd 服务已启动"
            else
                nohup "$BINARY" -config "$CONFIG_FILE" >> "$INSTALL_DIR/data/gateway.log" 2>&1 &
                ok "Gateway 已启动 (PID $!)"
            fi
            ;;
        docker)
            cd "$INSTALL_DIR"
            # 查找 Dockerfile（根目录或 deploy/ 下）
            local dockerfile=""
            [[ -f "$INSTALL_DIR/Dockerfile" ]] && dockerfile="$INSTALL_DIR/Dockerfile"
            [[ -f "$INSTALL_DIR/deploy/Dockerfile" ]] && dockerfile="$INSTALL_DIR/deploy/Dockerfile"
            if [[ -n "$dockerfile" ]]; then
                info "重建 Docker 镜像 ..."
                docker build -f "$dockerfile" -t llm-pool:local "$INSTALL_DIR"
                ok "Docker 镜像已重建"
            else
                warn "未找到 Dockerfile，使用现有镜像"
            fi
            docker compose up -d
            ok "Docker 容器已启动"
            ;;
        binary)
            nohup "$BINARY" -config "$CONFIG_FILE" >> "$INSTALL_DIR/data/gateway.log" 2>&1 &
            ok "Gateway 已启动 (PID $!)"
            ;;
        fresh)
            if has_systemd; then
                write_systemd_unit "real"
                systemctl daemon-reload
                systemctl enable llm-pool
                systemctl start llm-pool
                ok "systemd 服务已创建并启动"
            else
                nohup "$BINARY" -config "$CONFIG_FILE" >> "$INSTALL_DIR/data/gateway.log" 2>&1 &
                ok "Gateway 已启动 (PID $!)"
            fi
            ;;
    esac
}

# ══════════════════════════════════════════════════════════════════════════════
#  Step 6: 健康检查（双端口）
# ══════════════════════════════════════════════════════════════════════════════
health_check() {
    local gw_port="$1" admin_port="$2" max_wait="${3:-30}"
    local gw_ok=false admin_ok=false

    info "健康检查 → gateway:${gw_port} + admin:${admin_port} (最多等 ${max_wait}s)"
    for _ in $(seq 1 "$max_wait"); do
        if [[ "$gw_ok" != true ]] && curl -fsS "http://127.0.0.1:${gw_port}/healthz" &>/dev/null; then
            gw_ok=true
        fi
        if [[ "$admin_ok" != true ]] && curl -fsS "http://127.0.0.1:${admin_port}/login" &>/dev/null; then
            admin_ok=true
        fi
        if [[ "$gw_ok" == true && "$admin_ok" == true ]]; then
            ok "健康检查通过（gateway + admin）"
            return 0
        fi
        sleep 1
    done

    [[ "$gw_ok" == true ]]    && ok "  gateway :${gw_port} — 正常"    || warn "  gateway :${gw_port} — 超时"
    [[ "$admin_ok" == true ]] && ok "  admin   :${admin_port} — 正常"  || warn "  admin   :${admin_port} — 超时"
    return 1
}

# ══════════════════════════════════════════════════════════════════════════════
#  首次部署配置
# ══════════════════════════════════════════════════════════════════════════════
fresh_configure() {
    bold "━━ 首次部署配置 ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    local GW_PORT UI_PORT bind_ip PROVIDER_MODE MASTER_KEY

    GW_PORT=$(ask "  网关 API 端口" "8787")
    UI_PORT=$(ask "  管理后台端口" "8788")
    echo "  1) 所有接口 (0.0.0.0)    2) 仅本地 (127.0.0.1)"
    local sel; read -r -p "  绑定方式 [1]: " sel; sel=${sel:-1}
    bind_ip="0.0.0.0"; [[ "$sel" == "2" ]] && bind_ip="127.0.0.1"
    echo "  1) real — 真实账号    2) mock — 模拟测试"
    read -r -p "  Provider [1]: " sel; sel=${sel:-1}
    PROVIDER_MODE="real"; [[ "$sel" == "2" ]] && PROVIDER_MODE="mock"
    MASTER_KEY=$(generate_random 40)

    mkdir -p "$INSTALL_DIR/config"
    cat > "$CONFIG_FILE" <<YAML
server:
  gateway_addr: "${bind_ip}:${GW_PORT}"
  admin_addr: "${bind_ip}:${UI_PORT}"
  read_timeout: 300s
  read_header_timeout: 10s
  write_timeout: 600s
  idle_timeout: 120s
  max_header_bytes: 1048576
  max_request_bytes: 536870912
  request_body_memory_budget: 536870912
  network_ingress_bytes_per_sec: 8388608
  network_egress_bytes_per_sec: 4194304
  network_burst_bytes: 1048576
  rate_limit_rpm: 300
  rate_limit_burst: 60

storage:
  db_path: "data/pool.db"
  master_key: "${MASTER_KEY}"
  passwd_path: "data/passwd.txt"

logging:
  level: info
  format: text

scheduler:
  ewma_alpha: 0.3
  sticky:
    enabled: true
    sticky_ttl: 30m

resource:
  profile: lowmem
  go_mem_limit: 768MiB
  go_gc: 50
  sqlite_mmap: 30MB
  conversation_cache_max: 5000
  responses_state_max: 5000
  responses_state_bytes: 268435456
  responses_state_ttl: 12h
YAML
    ok "配置已写入 → $CONFIG_FILE"
    write_systemd_unit "$PROVIDER_MODE"
}

# ══════════════════════════════════════════════════════════════════════════════
#  回滚
# ══════════════════════════════════════════════════════════════════════════════
rollback() {
    local bak_dir="$1" mode="$2"
    if [[ -f "$bak_dir/gateway.bak" ]]; then
        warn "编译失败 — 回滚到上一版本 ..."
        cp "$bak_dir/gateway.bak" "$BINARY"
        chmod +x "$BINARY"
        start_service "$mode"
        ok "已回滚"
    else
        err "无可用备份"
    fi
}

# ══════════════════════════════════════════════════════════════════════════════
#  MAIN
# ══════════════════════════════════════════════════════════════════════════════
main() {
    bold "╔══════════════════════════════════════════╗"
    bold "║   LLM Pool Gateway — 全量重编译升级     ║"
    bold "╚══════════════════════════════════════════╝"
    echo

    [[ ! -f "$SRC_DIR/go.mod" ]] && die "请在源码根目录运行此脚本"

    mkdir -p "$INSTALL_DIR"/{bin,data,config,backups}
    mkdir -p "$(dirname "$LOG_FILE")"
    log "=== 全量升级开始 ==="

    local mode; mode=$(detect_mode)
    info "部署模式: $mode"

    # 显示当前配置
    if [[ -f "$CONFIG_FILE" ]]; then
        info "当前配置:"
        printf "  gateway : %s\n" "$(read_yaml_val gateway_addr 2>/dev/null || echo '未设置')"
        printf "  admin   : %s\n" "$(read_yaml_val admin_addr 2>/dev/null || echo '未设置')"
    fi

    # 显示受保护数据
    echo
    bold "━━ 受保护数据（不会被删除）━━━━━━━━━━━━━━━━━━━━━━━"
    [[ -f "$CONFIG_FILE" ]]                  && printf "  ✓ config.yaml   (%s)\n" "$(du -h "$CONFIG_FILE" | cut -f1)"   || printf "  · config.yaml   (不存在)\n"
    [[ -f "$DB_FILE" ]]                      && printf "  ✓ pool.db       (%s)\n" "$(du -h "$DB_FILE" | cut -f1)"       || printf "  · pool.db       (不存在)\n"
    [[ -f "$INSTALL_DIR/data/passwd.txt" ]]  && printf "  ✓ passwd.txt    (存在)\n"                                     || printf "  · passwd.txt    (不存在)\n"
    echo

    # ── Step 1 ─────────────────────────────────────────────────────────────
    bold "━━ Step 1/6: 备份关键数据"
    local bak_dir; bak_dir=$(do_backup)
    log "备份: $bak_dir"

    # ── Step 2 ─────────────────────────────────────────────────────────────
    echo
    bold "━━ Step 2/6: 停止服务"
    if [[ "$mode" != "fresh" ]]; then
        stop_service "$mode"
    else
        info "首次安装 — 无需停止"
    fi

    # ── Step 3 ─────────────────────────────────────────────────────────────
    echo
    bold "━━ Step 3/6: 同步源码（清除旧残留）"
    sync_source

    # 首次部署配置
    if [[ ! -f "$CONFIG_FILE" ]]; then
        echo
        fresh_configure
        mode="fresh"
    fi

    # ── Step 4 ─────────────────────────────────────────────────────────────
    echo
    bold "━━ Step 4/6: 全量清缓存 + 重编译"
    if [[ "$mode" == "docker" ]]; then
        # Docker 模式也需要重建镜像，不能跳过
        ensure_go
        info "Docker 模式 — 镜像将在启动时重建"
    else
        if ! full_rebuild; then
            err "编译失败！"
            rollback "$bak_dir" "$mode"
            die "升级中止，已回滚到上一版本"
        fi
    fi
    log "编译完成"

    # ── Step 5 ─────────────────────────────────────────────────────────────
    echo
    bold "━━ Step 5/6: 启动服务"
    if [[ "$mode" == "fresh" ]]; then
        start_service "fresh"
    else
        start_service "$mode"
    fi
    log "服务已启动"

    # ── Step 6 ─────────────────────────────────────────────────────────────
    echo
    bold "━━ Step 6/6: 健康检查"
    local gw_port admin_port
    gw_port=$(get_gateway_port)
    admin_port=$(get_admin_port)
    if ! health_check "$gw_port" "$admin_port" 30; then
        if [[ "$mode" == "systemd" || "$mode" == "fresh" ]]; then
            warn "查看日志: journalctl -u llm-pool -n 50"
        elif [[ "$mode" == "docker" ]]; then
            warn "查看日志: cd $INSTALL_DIR && docker compose logs --tail 50"
        fi
        log "健康检查失败 gw=$gw_port admin=$admin_port"
    else
        log "健康检查通过 gw=$gw_port admin=$admin_port"
    fi

    # ── 完成 ───────────────────────────────────────────────────────────────
    local pub_ip
    pub_ip=$(curl -fsSL --max-time 4 https://api.ipify.org 2>/dev/null \
          || curl -fsSL --max-time 4 https://ifconfig.me 2>/dev/null \
          || hostname -I 2>/dev/null | awk '{print $1}' \
          || echo "YOUR_IP")

    local admin_pass="(见 $INSTALL_DIR/data/passwd.txt)"
    local pw_path="$INSTALL_DIR/data/passwd.txt"
    [[ -f "$pw_path" ]] && admin_pass=$(head -1 "$pw_path" 2>/dev/null | cut -d: -f2 || echo "(见文件)")

    echo
    bold "╔══════════════════════════════════════════╗"
    bold "║           升级完成                       ║"
    bold "╚══════════════════════════════════════════╝"
    echo
    if [[ -f "$BINARY" ]]; then
        printf "  Binary    : %s (%s)\n" "$BINARY" "$(du -h "$BINARY" | cut -f1)"
        printf "  编译时间  : %s\n" "$(date -d @"$(stat -c '%Y' "$BINARY" 2>/dev/null || echo 0)" '+%Y-%m-%d %H:%M:%S' 2>/dev/null || echo 'unknown')"
    fi
    printf "  Config    : %s\n" "$CONFIG_FILE"
    echo
    printf "  管理后台  : http://%s:%s\n"             "$pub_ip" "$admin_port"
    printf "  用户门户  : http://%s:%s/portal/login\n" "$pub_ip" "$admin_port"
    printf "  API 端点  : http://%s:%s/v1\n"           "$pub_ip" "$gw_port"
    printf "  管理密码  : %s\n"                        "$admin_pass"
    echo
    bold "  清除内容:"
    printf "    ✓ Go build cache    (go clean -cache -testcache)\n"
    printf "    ✓ Go module cache   (go clean -modcache)\n"
    printf "    ✓ 旧 binary + bin/  (全部清除后重编译)\n"
    printf "    ✓ 旧源码残留        (rsync --delete 清除)\n"
    echo
    bold "  保留内容:"
    printf "    ✓ config.yaml       租户/端口/密钥配置\n"
    printf "    ✓ pool.db           所有账号/API Key 数据\n"
    printf "    ✓ passwd.txt        管理员密码\n"
    printf "    ✓ docker-compose.yml\n"
    echo

    if [[ "$mode" == "systemd" || "$mode" == "fresh" ]]; then
        printf "  systemctl status llm-pool    # 状态\n"
        printf "  journalctl -u llm-pool -f    # 日志\n"
        printf "  systemctl restart llm-pool   # 重启\n"
    elif [[ "$mode" == "docker" ]]; then
        printf "  cd %s && docker compose logs -f   # 日志\n" "$INSTALL_DIR"
        printf "  cd %s && docker compose restart    # 重启\n" "$INSTALL_DIR"
    fi

    bold "══════════════════════════════════════════"
    log "=== 全量升级完成 ==="
}

main "$@"
