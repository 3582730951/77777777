#!/usr/bin/env bash
# ============================================================================
#  LLM Pool Gateway — 一键开箱即用部署脚本
#
#  将完整项目目录上传到服务器后，在项目根目录运行：
#    sudo bash install.sh
#
#  完整部署：
#    1. 系统依赖（Go, Python3, Node.js, Xvfb, 浏览器库）
#    2. Go Gateway 编译
#    3. AutoReg React SPA 前端构建 + 嵌入
#    4. AutoReg Python 服务 (venv + 依赖 + systemd)
#    5. Kiro Gateway Python 服务 (venv + 依赖 + systemd)
#    6. Xvfb 虚拟显示 (systemd)
#    7. LLM Pool Gateway (systemd)
#    8. 可选: Nginx 反代 + Let's Encrypt SSL
# ============================================================================

set -euo pipefail

# ── 颜色输出 ──
bold() { printf "\033[1m%s\033[0m\n" "$*"; }
info() { printf "\033[36m[i]\033[0m %s\n" "$*"; }
ok()   { printf "\033[32m[✓]\033[0m %s\n" "$*"; }
warn() { printf "\033[33m[!]\033[0m %s\n" "$*"; }
err()  { printf "\033[31m[✗]\033[0m %s\n" "$*"; exit 1; }
step() { printf "\n\033[1;35m━━ %s ━━\033[0m\n" "$*"; }

ask() {
  local prompt="$1" default="$2" var
  read -r -p "$prompt [$default]: " var || true
  echo "${var:-$default}"
}

generate_random() { head -c "${1:-32}" /dev/urandom | base64 | tr -d '/+=' | head -c "${1:-32}"; }
has_systemd() { command -v systemctl &>/dev/null && [ -d /run/systemd/system ]; }

# ── 路径常量 ──
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_DIR="${INSTALL_DIR:-/opt/llm-pool}"

# 验证项目结构
[ -f "$SRC_DIR/cmd/gateway/main.go" ] || err "请在项目根目录运行此脚本（未找到 cmd/gateway/main.go）"
[ -d "$SRC_DIR/services/autoreg" ]    || err "项目不完整（未找到 services/autoreg）"

# ── 检查 root ──
if [ "$EUID" -ne 0 ]; then
  warn "建议以 root/sudo 身份运行以安装系统依赖并创建 systemd 服务"
  read -r -p "继续？[y/N]: " yn
  [[ "${yn:-N}" =~ ^[Yy]$ ]] || exit 1
fi

# ══════════════════════════════════════════════════════════════════════════════
#  第 0 步：交互式配置
# ══════════════════════════════════════════════════════════════════════════════
bold ""
bold "=========================================="
bold "   LLM Pool Gateway — 一键部署"
bold "=========================================="
echo ""

OS_ID="unknown"
[ -r /etc/os-release ] && . /etc/os-release && OS_ID="$ID"
ARCH=$(uname -m)
info "OS: ${OS_ID}  ARCH: ${ARCH}"

echo ""
step "端口配置"
echo "  网关 API 端口 → 下游 Cursor / Claude Code 连接此端口"
echo "  Admin UI 端口 → 浏览器打开管理后台"
GW_PORT=$(ask "  网关 API 端口" "19787")
UI_PORT=$(ask "  Admin UI 端口" "19788")
AUTOREG_PORT=$(ask "  AutoReg 服务端口" "9900")
KIRO_GW_PORT=$(ask "  Kiro Gateway 端口" "18923")

echo ""
step "反代配置"
echo "  1) 不用 Nginx — 直接 IP:端口 访问（最简单）"
echo "  2) Nginx + SSL — 绑域名 + 自动 HTTPS 证书（生产推荐）"
read -r -p "  请选择 [1]: " NGINX_SEL
NGINX_SEL=${NGINX_SEL:-1}
USE_NGINX=false
DOMAIN=""
if [ "$NGINX_SEL" = "2" ]; then
  USE_NGINX=true
  DOMAIN=$(ask "  域名（A 记录须已指向本机）" "")
  [ -z "$DOMAIN" ] && { warn "未输入域名，切换为直接 IP 访问"; USE_NGINX=false; }
fi

echo ""
step "Provider 模式"
echo "  1) real — 真实订阅账号（需添加 ChatGPT/Claude/Gemini 账号）"
echo "  2) mock — 模拟响应，无需账号（仅测试功能）"
read -r -p "  请选择 [1]: " PROV_SEL
PROV_MODE="real"
[ "${PROV_SEL:-1}" = "2" ] && PROV_MODE="mock"

PUBLIC_IP=$(curl -fsSL --max-time 5 https://api.ipify.org 2>/dev/null \
         || curl -fsSL --max-time 5 https://ifconfig.me 2>/dev/null \
         || hostname -I 2>/dev/null | awk '{print $1}' \
         || echo "127.0.0.1")

MASTER_KEY=$(generate_random 40)

echo ""
step "配置确认"
printf "  %-18s: %s\n" "安装目录"       "$INSTALL_DIR"
printf "  %-18s: %s\n" "网关 API 端口"  "$GW_PORT"
printf "  %-18s: %s\n" "Admin UI 端口"  "$UI_PORT"
printf "  %-18s: %s\n" "AutoReg 端口"   "$AUTOREG_PORT"
printf "  %-18s: %s\n" "Kiro GW 端口"   "$KIRO_GW_PORT"
printf "  %-18s: %s\n" "Nginx + SSL"    "$([ "$USE_NGINX" = true ] && echo "是 ($DOMAIN)" || echo "否")"
printf "  %-18s: %s\n" "Provider 模式"  "$PROV_MODE"
printf "  %-18s: %s\n" "公网 IP"        "$PUBLIC_IP"
echo ""
read -r -p "确认开始部署？[Y/n]: " yn
[[ "${yn:-Y}" =~ ^[Nn]$ ]] && { warn "已取消"; exit 0; }

# ══════════════════════════════════════════════════════════════════════════════
#  第 1 步：系统依赖
# ══════════════════════════════════════════════════════════════════════════════
step "1/8 安装系统依赖"

pkg_install() {
  if command -v apt-get &>/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" 2>&1 | tail -2
  elif command -v dnf &>/dev/null; then
    dnf install -y -q "$@" 2>&1 | tail -2
  elif command -v yum &>/dev/null; then
    yum install -y -q "$@" 2>&1 | tail -2
  else
    warn "无法识别包管理器，请手动安装: $*"
  fi
}

if command -v apt-get &>/dev/null; then
  info "更新包索引..."
  apt-get update -y -qq 2>&1 | tail -1
fi

ensure_python_venv() {
  if ! command -v python3 &>/dev/null; then
    info "安装 Python3..."
    pkg_install python3 python3-venv python3-pip
  fi

  if ! python3 -m venv --help &>/dev/null 2>&1; then
    info "安装 Python venv 支持..."
    pkg_install python3-venv python3-pip
  fi

  if ! python3 -m venv --help &>/dev/null 2>&1; then
    err "python3 venv 不可用。Ubuntu/Debian 请安装 python3-venv python3-pip"
  fi
  ok "Python $(python3 --version 2>&1 | awk '{print $2}') 就绪"
}

venv_usable() {
  local venv_dir="$1/.venv"
  [ -x "$venv_dir/bin/python" ] || return 1
  "$venv_dir/bin/python" -m pip --version >/dev/null 2>&1 || return 1
}

setup_python_venv() {
  local label="$1"
  local service_dir="$2"
  local requirements="$3"
  local entrypoint="${4:-}"
  local venv_dir="$service_dir/.venv"
  local py="$venv_dir/bin/python"

  if [ -d "$venv_dir" ] && ! venv_usable "$service_dir"; then
    warn "$label venv 不可用，删除后重建..."
    rm -rf "$venv_dir"
  fi

  if [ -n "$entrypoint" ] && [ -f "$venv_dir/.deps_installed" ]; then
    if [ ! -x "$venv_dir/bin/$entrypoint" ] || ! "$venv_dir/bin/$entrypoint" --version >/dev/null 2>&1; then
      warn "$label venv 入口脚本不可用，删除后重建..."
      rm -rf "$venv_dir"
    fi
  fi

  if ! venv_usable "$service_dir"; then
    info "创建 $label venv..."
    python3 -m venv "$venv_dir"
  fi

  if ! venv_usable "$service_dir"; then
    "$py" -m ensurepip --upgrade >/dev/null 2>&1 || true
  fi
  if ! venv_usable "$service_dir"; then
    err "$label venv 创建失败，请确认 python3-venv 和 python3-pip 已安装"
  fi

  info "安装 $label Python 依赖..."
  "$py" -m pip install --upgrade pip -q 2>&1 | tail -1
  if [ -f "$requirements" ]; then
    "$py" -m pip install -r "$requirements" -q 2>&1 | tail -3
  else
    warn "$requirements 不存在，跳过依赖安装"
  fi
  touch "$venv_dir/.deps_installed"
  ok "$label venv 就绪"
}

# 基础工具
BASIC_PKGS=(curl wget tar git unzip jq rsync)
MISSING=()
for p in "${BASIC_PKGS[@]}"; do
  command -v "$p" &>/dev/null || MISSING+=("$p")
done
[ ${#MISSING[@]} -gt 0 ] && { info "安装: ${MISSING[*]}"; pkg_install "${MISSING[@]}"; }
ok "基础工具就绪"

ensure_python_venv

# Node.js
if ! command -v node &>/dev/null; then
  info "安装 Node.js..."
  if command -v apt-get &>/dev/null; then
    curl -fsSL https://deb.nodesource.com/setup_22.x 2>/dev/null | bash - 2>&1 | tail -3
    pkg_install nodejs
  else
    pkg_install nodejs npm
  fi
fi
ok "Node $(node --version 2>/dev/null || echo 'N/A') 就绪"

# Go
if command -v go &>/dev/null; then
  GO_VER=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | tr -d 'go')
  GO_MINOR=${GO_VER##*.}
  if [ "${GO_MINOR:-0}" -ge 25 ]; then
    ok "Go $(go version | awk '{print $3}') 就绪"
  else
    NEED_GO=true
  fi
else
  NEED_GO=true
fi

if [ "${NEED_GO:-false}" = true ]; then
  go_arch="amd64"
  [ "$ARCH" = "aarch64" ] && go_arch="arm64"
  go_ver="1.25.3"
  go_file="go${go_ver}.linux-${go_arch}.tar.gz"
  tmpf=$(mktemp /tmp/go-XXXXXX.tar.gz)
  mirrors=(
    "https://golang.google.cn/dl/${go_file}"
    "https://mirrors.aliyun.com/golang/${go_file}"
    "https://go.dev/dl/${go_file}"
  )
  downloaded=false
  for url in "${mirrors[@]}"; do
    info "下载 Go: $url"
    if curl -fsSL --max-time 120 --retry 2 -o "$tmpf" "$url" 2>/dev/null && tar -tzf "$tmpf" &>/dev/null; then
      downloaded=true; ok "下载成功"; break
    fi
  done
  [ "$downloaded" = true ] || { rm -f "$tmpf"; err "无法下载 Go，请手动安装"; }
  rm -rf /usr/local/go && tar -C /usr/local -xzf "$tmpf" && rm -f "$tmpf"
  export PATH="$PATH:/usr/local/go/bin"
  grep -q '/usr/local/go/bin' /etc/profile 2>/dev/null || echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
  ok "Go $(go version | awk '{print $3}') 安装完成"
fi

# Xvfb + 浏览器系统库 (Camoufox headless)
info "安装 Xvfb + 浏览器系统库..."
if command -v apt-get &>/dev/null; then
  for pkg in xvfb libasound2t64 libasound2-dev libatk1.0-0t64 libatk-bridge2.0-0t64 \
    libcups2t64 libdbus-glib-1-2 libgtk-3-0t64 libnspr4 libnss3 \
    libpango-1.0-0 libx11-xcb1 libxcomposite1 libxdamage1 libxrandr2 \
    fonts-noto-cjk; do
    dpkg -s "$pkg" &>/dev/null 2>&1 || apt-get install -y -q "$pkg" 2>/dev/null || true
  done
elif command -v dnf &>/dev/null || command -v yum &>/dev/null; then
  pkg_install xorg-x11-server-Xvfb alsa-lib atk cups-libs dbus-glib \
    gtk3 nspr nss pango libX11-xcb libXcomposite libXdamage libXrandr \
    google-noto-cjk-fonts 2>/dev/null || true
fi
ok "浏览器系统库就绪"

# ══════════════════════════════════════════════════════════════════════════════
#  第 2 步：同步项目文件
# ══════════════════════════════════════════════════════════════════════════════
step "2/8 同步项目文件"
mkdir -p "$INSTALL_DIR"

if [ "$SRC_DIR" != "$INSTALL_DIR" ]; then
  info "同步 $SRC_DIR → $INSTALL_DIR ..."
  if command -v rsync &>/dev/null; then
    rsync -a --delete \
      --exclude='.git/' \
      --exclude='other-*/' \
      --exclude='other_*/' \
      --exclude='*/.venv/' \
      --exclude='*/node_modules/' \
      --exclude='*.log' \
      --exclude='data/pool.db*' \
      --exclude='data/passwd.txt' \
      "$SRC_DIR/" "$INSTALL_DIR/"
  else
    tar -C "$SRC_DIR" \
      --exclude='./.git' \
      --exclude='./other-*' \
      --exclude='./other_*' \
      --exclude='./*/.venv' \
      --exclude='./*/node_modules' \
      --exclude='./*.log' \
      --exclude='./data/pool.db*' \
      --exclude='./data/passwd.txt' \
      -cf - . | tar -C "$INSTALL_DIR" -xf -
  fi
  ok "项目文件已同步"
else
  ok "已在安装目录中运行，跳过同步"
fi
mkdir -p "$INSTALL_DIR/data"

# ══════════════════════════════════════════════════════════════════════════════
#  第 3 步：AutoReg React 前端构建
# ══════════════════════════════════════════════════════════════════════════════
step "3/8 构建 AutoReg 前端 SPA"
FRONTEND_SRC="$INSTALL_DIR/services/autoreg/frontend"
SPA_OUT="$INSTALL_DIR/internal/admin/autoreg_spa"

if [ -d "$FRONTEND_SRC" ]; then
  cd "$FRONTEND_SRC"
  [ ! -d "node_modules" ] && { info "安装 npm 依赖..."; npm install --silent 2>&1 | tail -3; }
  info "构建 SPA..."
  VITE_API_BASE=/api/autoreg npx vite build --base=/autoreg/ --outDir "$SPA_OUT" 2>&1 | tail -5
  ok "前端构建完成 → $SPA_OUT"
  cd "$INSTALL_DIR"
else
  warn "前端源码不存在，跳过"
fi

# ══════════════════════════════════════════════════════════════════════════════
#  第 4 步：Go Gateway 编译
# ══════════════════════════════════════════════════════════════════════════════
step "4/8 编译 Go Gateway"
cd "$INSTALL_DIR"

if ! curl -fsS --max-time 5 https://proxy.golang.org &>/dev/null 2>&1; then
  export GOPROXY="https://goproxy.cn,https://goproxy.io,direct"
  info "已启用 Go 代理: $GOPROXY"
fi

GATEWAY_BIN="$INSTALL_DIR/release/gateway-linux-amd64"
mkdir -p "$INSTALL_DIR/release"
go build -ldflags="-s -w" -o "$GATEWAY_BIN" ./cmd/gateway/ 2>&1
chmod +x "$GATEWAY_BIN"
ok "编译完成 → $GATEWAY_BIN"

# ══════════════════════════════════════════════════════════════════════════════
#  第 5 步：Python 虚拟环境
# ══════════════════════════════════════════════════════════════════════════════
step "5/8 配置 Python 虚拟环境"

AUTOREG_DIR="$INSTALL_DIR/services/autoreg"
KIRO_GW_DIR="$INSTALL_DIR/services/kiro-gateway"

info "配置 AutoReg venv..."
setup_python_venv "AutoReg" "$AUTOREG_DIR" "$AUTOREG_DIR/requirements.txt" "uvicorn"

if [ -d "$KIRO_GW_DIR" ] && [ -f "$KIRO_GW_DIR/requirements.txt" ]; then
  info "配置 Kiro Gateway venv..."
  setup_python_venv "Kiro Gateway" "$KIRO_GW_DIR" "$KIRO_GW_DIR/requirements.txt"
  [ ! -f "$KIRO_GW_DIR/credentials.json" ] && echo '[]' > "$KIRO_GW_DIR/credentials.json"
fi

# ══════════════════════════════════════════════════════════════════════════════
#  第 6 步：生成配置
# ══════════════════════════════════════════════════════════════════════════════
step "6/8 生成配置"
CONFIG_FILE="$INSTALL_DIR/config/config.yaml"
mkdir -p "$INSTALL_DIR/config"

if [ "$USE_NGINX" = true ]; then
  BIND_GW="127.0.0.1:${GW_PORT}"
  BIND_UI="127.0.0.1:${UI_PORT}"
else
  BIND_GW="0.0.0.0:${GW_PORT}"
  BIND_UI="0.0.0.0:${UI_PORT}"
fi

if [ -f "$CONFIG_FILE" ] && grep -q 'master_key' "$CONFIG_FILE" 2>/dev/null; then
  info "已存在 config.yaml，保留并更新端口"
  sed -i "s|gateway_addr:.*|gateway_addr: \"${BIND_GW}\"|" "$CONFIG_FILE"
  sed -i "s|admin_addr:.*|admin_addr: \"${BIND_UI}\"|" "$CONFIG_FILE"
  ok "端口已更新"
else
  cat > "$CONFIG_FILE" <<YAML
server:
  gateway_addr: "${BIND_GW}"
  admin_addr:   "${BIND_UI}"
  read_timeout:  300s
  read_header_timeout: 10s
  write_timeout: 600s
  idle_timeout:  120s
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
  buffer_pool_max: 1000
YAML
  ok "配置已写入 $CONFIG_FILE"
fi

# ══════════════════════════════════════════════════════════════════════════════
#  第 7 步：systemd 服务
# ══════════════════════════════════════════════════════════════════════════════
step "7/8 注册 systemd 服务"

if ! has_systemd; then
  warn "未检测到 systemd，跳过服务注册和自动拉起（容器/精简系统模式）"
  cat > "$INSTALL_DIR/start-direct.sh" <<EOF
#!/usr/bin/env bash
set -euo pipefail
cd "$INSTALL_DIR"
export PROVIDER_MODE="${PROV_MODE}"
exec "$GATEWAY_BIN" -config "$CONFIG_FILE"
EOF
  chmod +x "$INSTALL_DIR/start-direct.sh"

  echo ""
  bold "=========================================="
  bold "   构建与配置完成"
  bold "=========================================="
  echo ""
  printf "  Config          : %s\n" "$CONFIG_FILE"
  printf "  Binary          : %s\n" "$GATEWAY_BIN"
  printf "  Direct start    : %s/start-direct.sh\n" "$INSTALL_DIR"
  echo ""
  warn "当前环境没有 systemd；请用上面的直接启动脚本或容器 ENTRYPOINT 启动 Gateway。"
  exit 0
fi

# 停止旧服务（如果存在）
for svc in llm-pool llm-pool-autoreg llm-pool-kiro-gw llm-pool-xvfb; do
  systemctl stop "$svc" 2>/dev/null || true
done

# --- Xvfb ---
cat > /etc/systemd/system/llm-pool-xvfb.service <<EOF
[Unit]
Description=Xvfb Virtual Display for LLM Pool
After=network.target

[Service]
Type=simple
ExecStart=/usr/bin/Xvfb :99 -screen 0 1920x1080x24 -ac
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
ok "llm-pool-xvfb.service"

# --- AutoReg ---
cat > /etc/systemd/system/llm-pool-autoreg.service <<EOF
[Unit]
Description=LLM Pool AutoReg Service
After=network.target llm-pool-xvfb.service
Wants=llm-pool-xvfb.service

[Service]
Type=simple
User=root
WorkingDirectory=${AUTOREG_DIR}
Environment=DISPLAY=:99
Environment=PYTHONUTF8=1
Environment=KIRO_GATEWAY_DIR=${KIRO_GW_DIR}
Environment=KIRO_GATEWAY_PORT=${KIRO_GW_PORT}
Environment=GATEWAY_ADMIN_URL=http://127.0.0.1:${UI_PORT}
ExecStart=${AUTOREG_DIR}/.venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port ${AUTOREG_PORT}
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
ok "llm-pool-autoreg.service"

# --- Kiro Gateway ---
if [ -d "$KIRO_GW_DIR" ] && [ -f "$KIRO_GW_DIR/main.py" ]; then
cat > /etc/systemd/system/llm-pool-kiro-gw.service <<EOF
[Unit]
Description=LLM Pool Kiro Gateway
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=${KIRO_GW_DIR}
ExecStart=${KIRO_GW_DIR}/.venv/bin/python main.py --port ${KIRO_GW_PORT} --host 127.0.0.1
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
ok "llm-pool-kiro-gw.service"
fi

# --- LLM Pool Gateway ---
cat > /etc/systemd/system/llm-pool.service <<EOF
[Unit]
Description=LLM Pool Gateway
After=network.target llm-pool-autoreg.service
Wants=llm-pool-autoreg.service

[Service]
Type=simple
User=root
WorkingDirectory=${INSTALL_DIR}
ExecStart=${GATEWAY_BIN} -config ${CONFIG_FILE}
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
Environment=PROVIDER_MODE=${PROV_MODE}

[Install]
WantedBy=multi-user.target
EOF
ok "llm-pool.service"

systemctl daemon-reload
systemctl enable llm-pool-xvfb llm-pool-autoreg llm-pool 2>/dev/null
[ -f /etc/systemd/system/llm-pool-kiro-gw.service ] && systemctl enable llm-pool-kiro-gw 2>/dev/null
ok "所有服务已注册并设为开机自启"

# ══════════════════════════════════════════════════════════════════════════════
#  第 8 步：Nginx + SSL
# ══════════════════════════════════════════════════════════════════════════════
if [ "$USE_NGINX" = true ] && [ -n "$DOMAIN" ]; then
  step "8/8 配置 Nginx + SSL"
  command -v nginx &>/dev/null || pkg_install nginx
  command -v certbot &>/dev/null || pkg_install certbot python3-certbot-nginx
  systemctl enable --now nginx 2>/dev/null || true

  mkdir -p /etc/nginx/sites-available /etc/nginx/sites-enabled

  cat > /etc/nginx/sites-available/llm-pool.conf <<NGINX
upstream llm_pool_gw { server 127.0.0.1:${GW_PORT}; keepalive 32; }
upstream llm_pool_ui { server 127.0.0.1:${UI_PORT}; keepalive 16; }

server {
    listen 80;
    listen [::]:80;
    server_name ${DOMAIN};
    location /.well-known/acme-challenge/ { root /var/www/html; }
    location / { return 301 https://\$host\$request_uri; }
}

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name ${DOMAIN};

    proxy_http_version 1.1;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_read_timeout 600s;
    proxy_send_timeout 600s;
    chunked_transfer_encoding on;
    client_max_body_size 50m;
    proxy_set_header Host              \$host;
    proxy_set_header X-Real-IP         \$remote_addr;
    proxy_set_header X-Forwarded-Proto https;

    location /         { proxy_pass http://llm_pool_ui; }
    location /v1       { proxy_pass http://llm_pool_gw; }
    location /v1beta   { proxy_pass http://llm_pool_gw; }
    location /api      { proxy_pass http://llm_pool_gw; }
    location /healthz  { proxy_pass http://llm_pool_gw; }
    location /metrics  { proxy_pass http://llm_pool_gw; allow 127.0.0.1; deny all; }
    location /backend-api/ { proxy_pass http://llm_pool_gw; }
}
NGINX
  ln -sf /etc/nginx/sites-available/llm-pool.conf /etc/nginx/sites-enabled/llm-pool.conf
  rm -f /etc/nginx/sites-enabled/default 2>/dev/null || true
  nginx -t && systemctl reload nginx 2>/dev/null || systemctl restart nginx

  info "申请 SSL 证书..."
  certbot --nginx -d "$DOMAIN" --non-interactive --agree-tos \
    -m "admin@${DOMAIN}" --redirect 2>&1 | tail -5 || \
    warn "certbot 失败，请手动运行: certbot --nginx -d $DOMAIN"
  ok "Nginx + SSL 配置完成"
else
  info "跳过 Nginx 配置"
fi

# ══════════════════════════════════════════════════════════════════════════════
#  启动所有服务
# ══════════════════════════════════════════════════════════════════════════════
step "启动所有服务"

systemctl start llm-pool-xvfb 2>/dev/null || true
sleep 1

systemctl start llm-pool-autoreg
info "等待 AutoReg 启动..."
for i in $(seq 1 30); do
  curl -s "http://127.0.0.1:${AUTOREG_PORT}/api/health" 2>/dev/null | grep -q ok && { ok "AutoReg 已启动 (:$AUTOREG_PORT)"; break; }
  sleep 1
done

if [ -f /etc/systemd/system/llm-pool-kiro-gw.service ]; then
  CRED_COUNT=$(python3 -c "
import json
try:
  d=json.load(open('$KIRO_GW_DIR/credentials.json'))
  print(len([x for x in d if x.get('refresh_token')]))
except: print(0)
" 2>/dev/null || echo 0)
  if [ "$CRED_COUNT" -gt 0 ]; then
    systemctl start llm-pool-kiro-gw
    ok "Kiro Gateway 已启动 (:$KIRO_GW_PORT)"
  else
    info "Kiro Gateway credentials 为空，暂不启动（可在管理后台添加后重启）"
  fi
fi

systemctl start llm-pool
info "等待 Gateway 启动..."
for i in $(seq 1 15); do
  curl -s "http://127.0.0.1:${GW_PORT}/healthz" 2>/dev/null | grep -q ok && { ok "LLM Pool Gateway 已启动 (:$GW_PORT)"; break; }
  sleep 1
done

# ══════════════════════════════════════════════════════════════════════════════
#  完成
# ══════════════════════════════════════════════════════════════════════════════
echo ""
PASSWD_FILE="$INSTALL_DIR/data/passwd.txt"
ADMIN_PASS="（查看 $PASSWD_FILE）"
sleep 2
[ -f "$PASSWD_FILE" ] && ADMIN_PASS=$(head -1 "$PASSWD_FILE" | cut -d: -f2 | xargs)

bold "=========================================="
bold "   部署完成 ✓"
bold "=========================================="
echo ""

if [ "$USE_NGINX" = true ] && [ -n "$DOMAIN" ]; then
  printf "  管理后台       : https://%s\n"                  "$DOMAIN"
  printf "  用户门户       : https://%s/portal/login\n"     "$DOMAIN"
  printf "  API 端点       : https://%s/v1\n"               "$DOMAIN"
else
  printf "  管理后台       : http://%s:%s\n"                "$PUBLIC_IP" "$UI_PORT"
  printf "  用户门户       : http://%s:%s/portal/login\n"   "$PUBLIC_IP" "$UI_PORT"
  printf "  API 端点       : http://%s:%s/v1\n"             "$PUBLIC_IP" "$GW_PORT"
fi
echo ""
printf "  管理员用户名   : admin\n"
printf "  管理员密码     : %s\n" "$ADMIN_PASS"
printf "  密码文件       : %s\n" "$PASSWD_FILE"
echo ""

bold "  服务管理命令："
echo ""
printf "    systemctl status  llm-pool          # Gateway 状态\n"
printf "    systemctl status  llm-pool-autoreg  # AutoReg 状态\n"
printf "    systemctl status  llm-pool-kiro-gw  # Kiro GW 状态\n"
echo ""
printf "    journalctl -u llm-pool -f           # Gateway 日志\n"
printf "    journalctl -u llm-pool-autoreg -f   # AutoReg 日志\n"
printf "    journalctl -u llm-pool-kiro-gw -f   # Kiro GW 日志\n"
echo ""
printf "    systemctl restart llm-pool          # 重启 Gateway\n"
printf "    bash %s/rebuild.sh                  # 完整重编译+重启\n" "$INSTALL_DIR"
echo ""

bold "  AutoReg 功能："
echo "    管理后台左侧「AutoReg」→ 管理自动注册任务"
echo "    管理后台左侧「Kiro Gateway」→ 管理 Kiro Token"
echo ""

bold "  升级方法："
echo "    1. 上传新版项目文件覆盖 $INSTALL_DIR"
echo "    2. 运行 bash $INSTALL_DIR/rebuild.sh"
echo ""
bold "=========================================="
