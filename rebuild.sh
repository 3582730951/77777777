#!/usr/bin/env bash
# LLM Pool Gateway — 完整重编译 + 重启脚本
# 用法: bash rebuild.sh [选项]
#
# 选项:
#   --skip-frontend    跳过 AutoReg React 前端构建
#   --clean            清除 Go 编译缓存后重新编译
#   --force-deps       强制重装所有 Python 依赖
#
# 涵盖:
#   0. 停止所有服务（systemd 或直接进程）
#   1. Xvfb 虚拟显示
#   2. AutoReg React SPA 前端构建 + 嵌入
#   3. Go Gateway 编译
#   4. AutoReg Python 虚拟环境 + 依赖
#   5. Kiro Gateway Python 虚拟环境 + 依赖
#   6. systemd 场景同步 binary、配置、服务代码到实际运行目录
#   7. 启动 AutoReg 服务
#   8. 启动 Kiro Gateway（如有 credentials）
#   9. 启动 LLM Pool Gateway（systemd 或直接进程）
#  10. 状态汇总

set -euo pipefail

ROOT="$(cd "$(dirname "$0")" && pwd)"
GATEWAY_BIN="$ROOT/release/gateway-linux-amd64"
CONFIG="${GATEWAY_CONFIG:-$ROOT/config/config.yaml}"
AUTOREG_DIR="$ROOT/services/autoreg"
AUTOREG_VENV="$AUTOREG_DIR/.venv/bin"
KIRO_GW_DIR="$ROOT/services/kiro-gateway"
FRONTEND_SRC="$ROOT/services/autoreg/frontend"
SPA_OUT="$ROOT/internal/admin/autoreg_spa"

# 从 config 提取端口
_gw_port=$(grep 'gateway_addr' "$CONFIG" 2>/dev/null | head -1 | grep -oP ':\K\d+' || echo "8787")
_ad_port=$(grep 'admin_addr' "$CONFIG" 2>/dev/null | head -1 | grep -oP ':\K\d+' || echo "8788")
GW_ADDR="${GATEWAY_ADDR:-:${_gw_port}}"
ADMIN_ADDR="${ADMIN_ADDR:-:${_ad_port}}"
AUTOREG_PORT="${AUTOREG_PORT:-9900}"
KIRO_GW_PORT="${KIRO_GW_PORT:-18923}"

SKIP_FRONTEND=false
CLEAN_BUILD=false
FORCE_DEPS=false
for arg in "$@"; do
  case "$arg" in
    --skip-frontend) SKIP_FRONTEND=true ;;
    --clean)         CLEAN_BUILD=true ;;
    --force-deps)    FORCE_DEPS=true ;;
  esac
done

# ── 颜色输出 ──
info()  { printf "\033[36m[i]\033[0m %s\n" "$*"; }
ok()    { printf "\033[32m[✓]\033[0m %s\n" "$*"; }
warn()  { printf "\033[33m[!]\033[0m %s\n" "$*"; }
err()   { printf "\033[31m[✗]\033[0m %s\n" "$*"; }
step()  { printf "\n\033[1m── %s ──\033[0m\n" "$*"; }

timestamp() { date +%Y%m%d-%H%M%S; }

sync_file_with_backup() {
  local src="$1" dst="$2" label="$3"
  if [ ! -f "$src" ]; then
    warn "$label 源文件不存在，跳过: $src"
    return 0
  fi
  mkdir -p "$(dirname "$dst")"
  if [ -f "$dst" ]; then
    if cmp -s "$src" "$dst"; then
      ok "$label 已是最新 → $dst"
      return 0
    fi
    local bak="${dst}.bak.$(timestamp)"
    cp -p "$dst" "$bak"
    info "$label 旧文件已备份 → $bak"
  fi
  cp -p "$src" "$dst"
  ok "$label 已同步 → $dst"
}

sync_code_tree() {
  local src="$1" dst="$2" label="$3"
  if [ ! -d "$src" ]; then
    return 0
  fi
  mkdir -p "$dst"
  local src_real dst_real
  src_real="$(cd "$src" && pwd -P)"
  dst_real="$(cd "$dst" && pwd -P)"
  if [ "$src_real" = "$dst_real" ]; then
    ok "$label 源目录即运行目录，跳过同步 → $dst"
    return 0
  fi
  if command -v rsync >/dev/null 2>&1; then
    rsync -a --delete \
      --exclude='.git/' \
      --exclude='.venv/' \
      --exclude='node_modules/' \
      --exclude='__pycache__/' \
      --exclude='.pytest_cache/' \
      --exclude='*.pyc' \
      --exclude='data/' \
      --exclude='credentials.json' \
      "$src"/ "$dst"/
  else
    warn "rsync 不存在，使用 tar 同步 $label（不会删除目标端多余旧文件）"
    (cd "$src" && tar \
      --exclude='.git' \
      --exclude='.venv' \
      --exclude='node_modules' \
      --exclude='__pycache__' \
      --exclude='.pytest_cache' \
      --exclude='*.pyc' \
      --exclude='data' \
      --exclude='credentials.json' \
      -cf - .) | (cd "$dst" && tar -xf -)
  fi
  ok "$label 已同步 → $dst"
}

systemd_gateway_exec_line() {
  systemctl cat llm-pool 2>/dev/null | sed -n 's/^ExecStart=//p' | tail -1
}

systemd_gateway_binary_path() {
  local line candidate
  line="$(systemd_gateway_exec_line)"
  [ -n "$line" ] || return 0
  # shellcheck disable=SC2086
  set -- $line
  while [ "$#" -gt 0 ]; do
    case "$1" in
      env|/usr/bin/env)
        shift
        while [ "$#" -gt 0 ]; do
          case "$1" in
            *=*) shift ;;
            *) break ;;
          esac
        done
        ;;
      *=*)
        shift
        ;;
      *)
        candidate="$1"
        case "$(basename "$candidate")" in
          *gateway*)
            printf "%s" "$candidate"
            return 0
            ;;
        esac
        return 0
        ;;
    esac
  done
}

systemd_gateway_config_path() {
  local line prev arg
  line="$(systemd_gateway_exec_line)"
  [ -n "$line" ] || return 0
  prev=""
  # shellcheck disable=SC2086
  for arg in $line; do
    if [ "$prev" = "-config" ]; then
      printf "%s" "$arg"
      return 0
    fi
    case "$arg" in
      -config=*)
        printf "%s" "${arg#-config=}"
        return 0
        ;;
    esac
    prev="$arg"
  done
}

systemd_gateway_workdir() {
  local wd
  wd="$(systemctl show llm-pool -p WorkingDirectory --value 2>/dev/null || true)"
  [ "$wd" = "-" ] && wd=""
  printf "%s" "$wd"
}

ensure_go() {
  export PATH="$PATH:/usr/local/go/bin:/usr/lib/go/bin"
  if command -v go >/dev/null 2>&1; then
    local ver major minor
    ver=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | tr -d 'go')
    major=${ver%%.*}
    minor=${ver##*.}
    if [ "${major:-0}" -ge 1 ] && [ "${minor:-0}" -ge 25 ]; then
      ok "Go $(go version | awk '{print $3}') 就绪"
      return 0
    fi
  fi

  info "安装 Go >= 1.25 ..."
  local arch go_arch go_ver go_file tmpf downloaded
  arch=$(uname -m)
  case "$arch" in
    x86_64) go_arch="amd64" ;;
    aarch64|arm64) go_arch="arm64" ;;
    *) err "不支持的架构: $arch"; exit 1 ;;
  esac
  go_ver="1.25.3"
  go_file="go${go_ver}.linux-${go_arch}.tar.gz"
  tmpf=$(mktemp /tmp/go-XXXXXX.tar.gz)
  downloaded=false
  for url in "https://golang.google.cn/dl/${go_file}" "https://mirrors.aliyun.com/golang/${go_file}" "https://go.dev/dl/${go_file}"; do
    info "下载 Go: $url"
    if curl -fsSL --max-time 120 --retry 2 -o "$tmpf" "$url" 2>/dev/null && tar -tzf "$tmpf" >/dev/null 2>&1; then
      downloaded=true
      break
    fi
  done
  if [ "$downloaded" != true ]; then
    rm -f "$tmpf"
    err "无法下载 Go，请手动安装 Go 1.25+"
    exit 1
  fi
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmpf"
  rm -f "$tmpf"
  export PATH="$PATH:/usr/local/go/bin"
  grep -q '/usr/local/go/bin' /etc/profile 2>/dev/null || echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile
  ok "Go $(go version | awk '{print $3}') 安装完成"
}

node_version_ok() {
  command -v node >/dev/null 2>&1 || return 1
  local ver major minor rest
  ver=$(node -v 2>/dev/null | sed 's/^v//')
  major=${ver%%.*}
  rest=${ver#*.}
  minor=${rest%%.*}
  case "$major" in
    ''|*[!0-9]*) return 1 ;;
  esac
  case "$minor" in
    ''|*[!0-9]*) minor=0 ;;
  esac
  # Vite 8 / Tailwind 4.2 require Node ^20.19 or >=22.12.
  if [ "$major" -gt 22 ]; then
    return 0
  fi
  if [ "$major" -eq 22 ] && [ "$minor" -ge 12 ]; then
    return 0
  fi
  if [ "$major" -eq 20 ] && [ "$minor" -ge 19 ]; then
    return 0
  fi
  return 1
}

ensure_node() {
  export PATH="/usr/local/node/bin:$PATH"
  if node_version_ok && command -v npm >/dev/null 2>&1; then
    ok "Node $(node -v) / npm $(npm -v) 就绪"
    return 0
  fi

  if [ "$(id -u)" -ne 0 ]; then
    err "前端构建需要 Node >=20.19。请用 root 执行 rebuild.sh，或手动安装 Node 22 LTS"
    exit 1
  fi

  info "安装 Node 22 LTS（Vite/Tailwind 需要 Node >=20.19）..."
  local arch node_arch ver base tmpd node_file downloaded
  arch=$(uname -m)
  case "$arch" in
    x86_64) node_arch="x64" ;;
    aarch64|arm64) node_arch="arm64" ;;
    *) err "不支持的 Node 架构: $arch"; exit 1 ;;
  esac

  ver=""
  base=""
  for candidate_base in "https://nodejs.org/dist" "https://npmmirror.com/mirrors/node"; do
    ver=$(curl -fsSL --max-time 30 --retry 2 "$candidate_base/index.tab" 2>/dev/null | awk 'NR>1 && $1 ~ /^v22\./ {print $1; exit}' || true)
    if [ -n "$ver" ]; then
      base="$candidate_base"
      break
    fi
  done
  if [ -z "$ver" ]; then
    ver="v22.22.2"
    base="https://nodejs.org/dist"
  fi

  node_file="node-${ver}-linux-${node_arch}.tar.xz"
  tmpd=$(mktemp -d /tmp/node-XXXXXX)
  downloaded=false
  for url in \
    "$base/$ver/$node_file" \
    "https://nodejs.org/dist/$ver/$node_file" \
    "https://npmmirror.com/mirrors/node/$ver/$node_file"; do
    info "下载 Node: $url"
    if curl -fsSL --max-time 180 --retry 2 -o "$tmpd/$node_file" "$url" 2>/dev/null && tar -tf "$tmpd/$node_file" >/dev/null 2>&1; then
      downloaded=true
      break
    fi
  done
  if [ "$downloaded" != true ]; then
    rm -rf "$tmpd"
    err "无法下载 Node 22，请手动安装 Node >=20.19 后重试，或使用 ./rebuild.sh --skip-frontend"
    exit 1
  fi

  rm -rf "/usr/local/node-${ver}-linux-${node_arch}"
  tar -C /usr/local -xf "$tmpd/$node_file"
  rm -rf "$tmpd"
  ln -sfn "/usr/local/node-${ver}-linux-${node_arch}" /usr/local/node
  export PATH="/usr/local/node/bin:$PATH"
  grep -q '/usr/local/node/bin' /etc/profile 2>/dev/null || echo 'export PATH=/usr/local/node/bin:$PATH' >> /etc/profile
  if ! node_version_ok || ! command -v npm >/dev/null 2>&1; then
    err "Node 安装后仍不可用，请检查 /usr/local/node/bin"
    exit 1
  fi
  ok "Node $(node -v) / npm $(npm -v) 安装完成"
}

pkg_install() {
  if command -v apt-get >/dev/null 2>&1; then
    if [ "$(id -u)" -ne 0 ]; then
      err "缺少系统依赖，请用 root 执行或手动安装: $*"
      exit 1
    fi
    apt-get update -y -qq 2>&1 | tail -1 || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q "$@" 2>&1 | tail -3
  elif command -v dnf >/dev/null 2>&1; then
    [ "$(id -u)" -eq 0 ] || { err "缺少系统依赖，请用 root 执行或手动安装: $*"; exit 1; }
    dnf install -y -q "$@" 2>&1 | tail -3
  elif command -v yum >/dev/null 2>&1; then
    [ "$(id -u)" -eq 0 ] || { err "缺少系统依赖，请用 root 执行或手动安装: $*"; exit 1; }
    yum install -y -q "$@" 2>&1 | tail -3
  elif command -v apk >/dev/null 2>&1; then
    [ "$(id -u)" -eq 0 ] || { err "缺少系统依赖，请用 root 执行或手动安装: $*"; exit 1; }
    apk add --no-cache "$@" 2>&1 | tail -3
  else
    err "无法识别包管理器，请手动安装: $*"
    exit 1
  fi
}

install_xvfb_optional() {
  [ "$(id -u)" -eq 0 ] || return 1
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -y -qq >/dev/null 2>&1 || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y -q xvfb >/dev/null 2>&1
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y -q xorg-x11-server-Xvfb >/dev/null 2>&1
  elif command -v yum >/dev/null 2>&1; then
    yum install -y -q xorg-x11-server-Xvfb >/dev/null 2>&1
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache xvfb >/dev/null 2>&1
  else
    return 1
  fi
}

ensure_python_venv() {
  if ! command -v python3 >/dev/null 2>&1; then
    info "安装 Python3..."
    if command -v apk >/dev/null 2>&1; then
      pkg_install python3 py3-pip
    else
      pkg_install python3 python3-venv python3-pip
    fi
  fi

  if ! python3 -m venv --help >/dev/null 2>&1; then
    info "安装 Python venv 支持..."
    if command -v apk >/dev/null 2>&1; then
      pkg_install python3 py3-pip
    else
      pkg_install python3-venv python3-pip
    fi
  fi

  if ! python3 -m venv --help >/dev/null 2>&1; then
    err "python3 venv 不可用。Ubuntu/Debian 请安装 python3-venv python3-pip"
    exit 1
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

  if [ "$FORCE_DEPS" = true ] && [ -d "$venv_dir" ]; then
    info "强制重建 $label venv..."
    rm -rf "$venv_dir"
  fi

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
    exit 1
  fi

  if [ "$FORCE_DEPS" = true ] || [ ! -f "$venv_dir/.deps_installed" ]; then
    info "安装 $label Python 依赖..."
    "$py" -m pip install --upgrade pip -q 2>&1 | tail -1
    if [ -f "$requirements" ]; then
      "$py" -m pip install -r "$requirements" -q 2>&1 | tail -3
    else
      warn "$requirements 不存在，跳过依赖安装"
    fi
    touch "$venv_dir/.deps_installed"
  fi
  ok "$label venv 就绪"
}

# 检测是否用 systemd 管理
HAS_SYSTEMD=false
if systemctl is-enabled llm-pool &>/dev/null 2>&1; then
  HAS_SYSTEMD=true
fi

HAS_AUTOREG_SYSTEMD=false
if systemctl is-enabled llm-pool-autoreg &>/dev/null 2>&1; then
  HAS_AUTOREG_SYSTEMD=true
fi

HAS_KIRO_SYSTEMD=false
if systemctl is-enabled llm-pool-kiro-gw &>/dev/null 2>&1; then
  HAS_KIRO_SYSTEMD=true
fi

# ── 0. 停止所有服务 ──
step "停止所有服务"
if [ "$HAS_SYSTEMD" = true ]; then
  systemctl stop llm-pool 2>/dev/null || true
else
  pkill -f 'gateway-linux-amd64\|llm-pool-gateway' 2>/dev/null || true
fi
if [ "$HAS_AUTOREG_SYSTEMD" = true ]; then
  systemctl stop llm-pool-autoreg 2>/dev/null || true
else
  pkill -f 'uvicorn main:app.*9900' 2>/dev/null || true
fi
if [ "$HAS_KIRO_SYSTEMD" = true ]; then
  systemctl stop llm-pool-kiro-gw 2>/dev/null || true
else
  pkill -f "kiro.*gateway.*${KIRO_GW_PORT}" 2>/dev/null || true
fi
pkill -f 'start\.py.*solver' 2>/dev/null || true
sleep 2
ok "所有服务已停止"

# ── 1. Xvfb 虚拟显示 ──
step "确保 Xvfb 运行"
if ! pgrep -x Xvfb >/dev/null 2>&1; then
  if ! command -v Xvfb >/dev/null 2>&1; then
    info "尝试安装 Xvfb..."
    install_xvfb_optional || true
  fi
  if command -v Xvfb >/dev/null 2>&1; then
    Xvfb :99 -screen 0 1920x1080x24 -ac &>/dev/null &
    sleep 1
    ok "Xvfb 已启动 (:99)"
  else
    warn "Xvfb 未安装，浏览器模式可能受限"
  fi
else
  ok "Xvfb 已在运行"
fi
export DISPLAY=:99

# ── 2. AutoReg React SPA 前端构建 ──
if [ "$SKIP_FRONTEND" = false ] && [ -d "$FRONTEND_SRC" ]; then
  step "构建 AutoReg React 前端"
  ensure_node
  cd "$FRONTEND_SRC"
  if [ "$FORCE_DEPS" = true ] || [ ! -d "node_modules" ] || [ ! -x "node_modules/.bin/vite" ]; then
    if [ -d "node_modules" ] && [ ! -x "node_modules/.bin/vite" ]; then
      warn "npm 依赖不完整，删除 node_modules 后重装..."
      rm -rf node_modules
    elif [ "$FORCE_DEPS" = true ] && [ -d "node_modules" ]; then
      info "强制重装 npm 依赖..."
      rm -rf node_modules
    fi
    info "安装 npm 依赖..."
    npm_log="/tmp/autoreg-npm-install.log"
    if [ -f package-lock.json ]; then
      npm ci --silent >"$npm_log" 2>&1 || { err "npm ci 失败，完整日志: $npm_log"; tail -80 "$npm_log"; exit 1; }
    else
      npm install --silent >"$npm_log" 2>&1 || { err "npm install 失败，完整日志: $npm_log"; tail -80 "$npm_log"; exit 1; }
    fi
  fi
  info "构建 SPA (base=/autoreg/, API=/api/autoreg)..."
  build_log="/tmp/autoreg-frontend-build.log"
  if ! VITE_API_BASE=/api/autoreg npx vite build --base=/autoreg/ --outDir "$SPA_OUT" >"$build_log" 2>&1; then
    err "前端构建失败，完整日志: $build_log"
    tail -120 "$build_log"
    exit 1
  fi
  tail -10 "$build_log"
  ok "前端构建完成 → $SPA_OUT"
  cd "$ROOT"
else
  info "跳过前端构建 (--skip-frontend 或目录不存在)"
fi

# ── 3. Go Gateway 编译 ──
step "编译 LLM Pool Gateway"
cd "$ROOT"
ensure_go
if [ "$CLEAN_BUILD" = true ]; then
  info "清除 Go 编译缓存..."
  go clean -cache 2>/dev/null || true
fi
go build -o "$GATEWAY_BIN" ./cmd/gateway/ 2>&1
ok "编译完成 → $GATEWAY_BIN"

# ── 4. AutoReg Python 虚拟环境 + 依赖 ──
if [ -d "$AUTOREG_DIR" ]; then
  step "配置 AutoReg Python 环境"
  ensure_python_venv
  setup_python_venv "AutoReg" "$AUTOREG_DIR" "$AUTOREG_DIR/requirements.txt" "uvicorn"
fi

# ── 5. Kiro Gateway Python 环境 + 依赖 ──
if [ -d "$KIRO_GW_DIR" ]; then
  step "配置 Kiro Gateway Python 环境"
  ensure_python_venv
  setup_python_venv "Kiro Gateway" "$KIRO_GW_DIR" "$KIRO_GW_DIR/requirements.txt"
  # 确保 credentials.json 存在
  [ ! -f "$KIRO_GW_DIR/credentials.json" ] && echo '[]' > "$KIRO_GW_DIR/credentials.json"
fi

# ── 6. 如果 systemd 管理，同步代码和配置到实际运行目录 ──
INSTALL_DIR="/opt/llm-pool"
if [ "$HAS_SYSTEMD" = true ]; then
  _systemd_wd="$(systemd_gateway_workdir)"
  [ -n "$_systemd_wd" ] && INSTALL_DIR="$_systemd_wd"
fi

AUTOREG_RUN_DIR="$AUTOREG_DIR"
AUTOREG_RUN_VENV="$AUTOREG_VENV"
KIRO_GW_RUN_DIR="$KIRO_GW_DIR"

if [ "$HAS_SYSTEMD" = true ] && [ -d "$INSTALL_DIR" ]; then
  step "同步代码和配置到安装目录 ($INSTALL_DIR)"

  systemd_bin="$(systemd_gateway_binary_path)"
  systemd_cfg="$(systemd_gateway_config_path)"
  [ -z "$systemd_bin" ] && systemd_bin="$INSTALL_DIR/bin/gateway"
  [ -z "$systemd_cfg" ] && systemd_cfg="$INSTALL_DIR/config/config.yaml"

  sync_file_with_backup "$GATEWAY_BIN" "$systemd_bin" "Gateway binary"
  chmod +x "$systemd_bin"
  if [ "$systemd_bin" != "$INSTALL_DIR/bin/gateway" ]; then
    sync_file_with_backup "$GATEWAY_BIN" "$INSTALL_DIR/bin/gateway" "Gateway binary 兼容路径"
    chmod +x "$INSTALL_DIR/bin/gateway"
  fi
  if [ "$systemd_bin" != "$INSTALL_DIR/release/gateway-linux-amd64" ]; then
    sync_file_with_backup "$GATEWAY_BIN" "$INSTALL_DIR/release/gateway-linux-amd64" "Gateway release binary"
    chmod +x "$INSTALL_DIR/release/gateway-linux-amd64"
  fi

  sync_file_with_backup "$CONFIG" "$systemd_cfg" "Gateway 配置"
  if [ "$systemd_cfg" != "$INSTALL_DIR/config/config.yaml" ]; then
    sync_file_with_backup "$CONFIG" "$INSTALL_DIR/config/config.yaml" "Gateway 配置兼容路径"
  fi
  sync_file_with_backup "$ROOT/config/config.example.yaml" "$INSTALL_DIR/config/config.example.yaml" "配置示例"

  if [ -d "$AUTOREG_DIR" ]; then
    sync_code_tree "$AUTOREG_DIR" "$INSTALL_DIR/services/autoreg" "AutoReg 代码"
    AUTOREG_RUN_DIR="$INSTALL_DIR/services/autoreg"
    AUTOREG_RUN_VENV="$AUTOREG_RUN_DIR/.venv/bin"
    ensure_python_venv
    setup_python_venv "AutoReg (install)" "$AUTOREG_RUN_DIR" "$AUTOREG_RUN_DIR/requirements.txt" "uvicorn"
  fi

  if [ -d "$KIRO_GW_DIR" ]; then
    sync_code_tree "$KIRO_GW_DIR" "$INSTALL_DIR/services/kiro-gateway" "Kiro Gateway 代码"
    KIRO_GW_RUN_DIR="$INSTALL_DIR/services/kiro-gateway"
    ensure_python_venv
    setup_python_venv "Kiro Gateway (install)" "$KIRO_GW_RUN_DIR" "$KIRO_GW_RUN_DIR/requirements.txt"
    [ ! -f "$KIRO_GW_RUN_DIR/credentials.json" ] && echo '[]' > "$KIRO_GW_RUN_DIR/credentials.json"
  fi
elif [ "$HAS_SYSTEMD" = true ]; then
  warn "systemd 已启用但安装目录不存在，跳过代码/配置同步: $INSTALL_DIR"
fi

# ── 7. 启动 AutoReg 服务 ──
if [ -d "$AUTOREG_RUN_DIR" ]; then
  step "启动 AutoReg 服务 (:$AUTOREG_PORT)"
  if [ "$HAS_AUTOREG_SYSTEMD" = true ]; then
    # Keep old systemd installs compatible with repaired venvs: execute modules
    # via venv python instead of relying on console-script shebangs.
    AUTOREG_UNIT="/etc/systemd/system/llm-pool-autoreg.service"
    UNIT_CHANGED=false
    if ! grep -q "GATEWAY_ADMIN_URL" "$AUTOREG_UNIT" 2>/dev/null; then
      sed -i "/^Environment=DISPLAY/a Environment=GATEWAY_ADMIN_URL=http://127.0.0.1:${ADMIN_ADDR#:}" "$AUTOREG_UNIT" 2>/dev/null || true
      UNIT_CHANGED=true
    fi
    if grep -q "uvicorn main:app" "$AUTOREG_UNIT" 2>/dev/null; then
      sed -i "s|^ExecStart=.*uvicorn main:app.*|ExecStart=${AUTOREG_RUN_DIR}/.venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port ${AUTOREG_PORT}|" "$AUTOREG_UNIT" 2>/dev/null || true
      UNIT_CHANGED=true
    fi
    [ "$UNIT_CHANGED" = true ] && systemctl daemon-reload 2>/dev/null || true
    systemctl start llm-pool-autoreg
    sleep 2
    if systemctl is-active --quiet llm-pool-autoreg; then
      ok "AutoReg 已启动 (systemd)"
    else
      err "AutoReg 启动失败，查看: journalctl -u llm-pool-autoreg -n 30"
    fi
  else
    cd "$AUTOREG_RUN_DIR"
    DISPLAY=:99 GATEWAY_ADMIN_URL="http://127.0.0.1:${ADMIN_ADDR#:}" \
    "$AUTOREG_RUN_VENV/python" -m uvicorn main:app \
      --host 127.0.0.1 --port "$AUTOREG_PORT" \
      >"/tmp/autoreg.log" 2>&1 &
    AUTOREG_PID=$!
    info "PID=$AUTOREG_PID, 等待启动..."
    for i in $(seq 1 30); do
      if curl -s "http://127.0.0.1:${AUTOREG_PORT}/api/health" 2>/dev/null | grep -q ok; then
        ok "AutoReg 已启动 (PID=$AUTOREG_PID)"
        break
      fi
      sleep 1
    done
    if ! curl -s "http://127.0.0.1:${AUTOREG_PORT}/api/health" 2>/dev/null | grep -q ok; then
      err "AutoReg 启动失败，查看 /tmp/autoreg.log"
    fi
    cd "$ROOT"
  fi
fi

# ── 8. 启动 Kiro Gateway（如有 credentials）──
if [ -d "$KIRO_GW_RUN_DIR" ]; then
  CRED_COUNT=$(python3 -c "
import json, sys
try:
  d=json.load(open('$KIRO_GW_RUN_DIR/credentials.json'))
  print(len([x for x in d if x.get('refresh_token')]))
except: print(0)
" 2>/dev/null || echo 0)
  if [ "$CRED_COUNT" -gt 0 ]; then
    step "启动 Kiro Gateway (:$KIRO_GW_PORT)"
    if [ "$HAS_KIRO_SYSTEMD" = true ]; then
      systemctl start llm-pool-kiro-gw
      sleep 2
      if systemctl is-active --quiet llm-pool-kiro-gw; then
        ok "Kiro Gateway 已启动 (systemd)"
      else
        err "Kiro Gateway 启动失败"
      fi
    else
      cd "$KIRO_GW_RUN_DIR"
      .venv/bin/python main.py --port "$KIRO_GW_PORT" --host 127.0.0.1 \
        >"/tmp/kiro-gateway.log" 2>&1 &
      KIRO_PID=$!
      ok "Kiro Gateway 已启动 (PID=$KIRO_PID)"
      cd "$ROOT"
    fi
  else
    info "Kiro Gateway credentials.json 为空，跳过启动"
  fi
fi

# ── 9. 启动 LLM Pool Gateway ──
step "启动 LLM Pool Gateway"
if [ "$HAS_SYSTEMD" = true ]; then
  systemctl start llm-pool
  sleep 2
  if systemctl is-active --quiet llm-pool; then
    ok "LLM Pool Gateway 已启动 (systemd)"
  else
    err "Gateway 启动失败，查看: journalctl -u llm-pool -n 30"
  fi
else
  cd "$ROOT"
  PROVIDER_MODE="${PROVIDER_MODE:-real}" \
    "$GATEWAY_BIN" -config "$CONFIG" \
    >"/tmp/llm-pool-gateway.log" 2>&1 &
  GW_PID=$!
  info "PID=$GW_PID, 等待启动..."
  for i in $(seq 1 15); do
    if curl -s "http://127.0.0.1:${GW_ADDR#:}/healthz" 2>/dev/null | grep -q ok; then
      ok "LLM Pool Gateway 已启动 (PID=$GW_PID)"
      break
    fi
    sleep 1
  done
  if ! curl -s "http://127.0.0.1:${GW_ADDR#:}/healthz" 2>/dev/null | grep -q ok; then
    err "Gateway 启动失败，查看 /tmp/llm-pool-gateway.log"
  fi
fi

# ── 10. 状态汇总 ──
step "服务状态"
echo ""
printf "  %-25s %-10s %s\n" "服务" "端口" "状态"
printf "  %-25s %-10s %s\n" "─────────────────────" "──────" "──────"

check_service() {
  local name=$1 port=$2 path=$3
  if curl -s --max-time 3 "http://127.0.0.1:${port}${path}" 2>/dev/null | grep -qE 'ok|true|running|html'; then
    printf "  %-25s %-10s \033[32m运行中\033[0m\n" "$name" ":$port"
  else
    printf "  %-25s %-10s \033[31m未运行\033[0m\n" "$name" ":$port"
  fi
}

check_service "LLM Pool Gateway (API)" "${GW_ADDR#:}" "/healthz"
check_service "LLM Pool Admin UI" "${ADMIN_ADDR#:}" "/"
check_service "AutoReg Service" "$AUTOREG_PORT" "/api/health"
[ -d "$KIRO_GW_DIR" ] && check_service "Kiro Gateway" "$KIRO_GW_PORT" "/health"

echo ""
info "日志:"
if [ "$HAS_SYSTEMD" = true ]; then
  echo "  Gateway:      journalctl -u llm-pool -f"
  echo "  AutoReg:      journalctl -u llm-pool-autoreg -f"
  echo "  Kiro Gateway: journalctl -u llm-pool-kiro-gw -f"
else
  echo "  Gateway:      /tmp/llm-pool-gateway.log"
  echo "  AutoReg:      /tmp/autoreg.log"
  [ -d "$KIRO_GW_DIR" ] && echo "  Kiro Gateway: /tmp/kiro-gateway.log"
fi
echo ""
LOCAL_IP=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1")
info "Admin UI:    http://${LOCAL_IP}:${ADMIN_ADDR#:}"
info "Gateway API: http://${LOCAL_IP}:${GW_ADDR#:}"
echo ""
ok "重编译 + 重启完成"
