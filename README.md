# LLM Pool Gateway

多 LLM 订阅账号池网关 (ChatGPT Plus/Pro · Claude Pro/Max · Gemini Advanced)。
将订阅版 web 凭证封装为统一的 OpenAI / Anthropic / Gemini 兼容 API 端点，
通过智能调度路由到健康账号，支持协议互转、无感失败转移、上游缓存优化与
反 Cloudflare 风控。

> ⚠️ **法律与 ToS 风险**：把订阅版 web 会话封装成 API 调用通常违反 OpenAI /
> Anthropic / Google 的服务条款，可能导致账号封禁。本项目仅作个人学习与
> 研究用途。

---

## 当前完成度

这是 **MVP 骨架**（plan 中所有架构决策已落地，可编译可运行可测试）。
核心设计草案和阶段记录保留在 `plan/` 目录，发布前检查清单见
[`docs/GITHUB_PUSH_CHECKLIST.md`](docs/GITHUB_PUSH_CHECKLIST.md)。

### 已实现
- ✅ 三家协议入站解析（OpenAI / Anthropic / Gemini），含流式 SSE
- ✅ 内部 IR + 三家 encode/decode（N+M 适配器，非 N×M）
- ✅ 调度器：三态置信度 + P2C+EWMA + Breaker + Sticky + Background Prober
- ✅ **"绝不放弃"决策路径**（永不返回 5xx，全员 cooling 时主动 probe）
- ✅ **Mid-Stream Head Buffer 失败转移**（500ms / 16KB / 5events 静默切换窗口）
- ✅ 模型强制重写（per-apikey > per-group 三层覆盖）+ 白名单
- ✅ 多租户（Tenant → Group → Account 三层模型）
- ✅ Persona Bundle 数据模型（TLS profile + UA + Client Hints + Cookie Jar）
- ✅ SQLite 持久化（账号 + 凭证 AES-GCM 加密 + 审计日志）
- ✅ Apple 设计语言 Web UI（HTMX + Tailwind 风格 CSS，自动深浅色）
- ✅ Prometheus metrics（覆盖 plan 的全部 pool_* 指标）
- ✅ 集群联邦观测（peer snapshot 拉取，零 Raft 复杂度）
- ✅ 一键 install.sh（交互式数字选择 + 自动 Docker / Nginx / certbot）
- ✅ Docker multi-stage build（含 chromium 和 slim 两变体）
- ✅ 1C1G 资源约束（GOMEMLIMIT + GOGC + Chromium 懒启动；实测稳态 19MB RSS）
- ✅ 单元测试覆盖关键模块（OpenAI 解码、scheduler、head buffer、router）
- ✅ 端到端验收（V1-V14 全部通过）

### 待实现（v1+）
- ❌ ChatGPT 真实反向工程（`internal/provider/chatgpt`，arkose token / `/backend-api/conversation`）
- ❌ Claude.ai 真实反向工程（`internal/provider/claude`）
- ❌ Gemini 真实反向工程（`internal/provider/gemini`）
- ❌ utls / tls-client 实际接入（stealth 包目前只设结构和 Headers，未做 TLS 指纹伪装）
- ❌ rod headless 浏览器自动 CF challenge 恢复
- ❌ 2captcha / capsolver 集成
- ❌ ConversationMapper 上游对话 ID 复用（数据结构已定义，未在主流程调用）
- ❌ HTTP/3 (QUIC) 上游
- ❌ Admin UI 完整 CRUD（账号增删改 / Token 管理 / 实时日志流）

**工作模式**：默认 `PROVIDER_MODE=mock`，三家 provider 都返回确定性测试响应。
切换到 `PROVIDER_MODE=real` 后会显式报错（pending），明确告知未实现而非
静默失败。

---

## 快速开始

### 1. 本地构建运行

```bash
cd llm-pool

# 编译
go build -o bin/gateway ./cmd/gateway

# 准备配置
cp config/config.example.yaml config/config.yaml

# 启动（DEV_SEED=1 自动注入 5 个 mock 账号便于演示）
DEV_SEED=1 POOL_MASTER_KEY=any-secret PROVIDER_MODE=mock \
  ./bin/gateway -config config/config.yaml
```

启动后：
- 网关：`http://127.0.0.1:8787` (健康 `/healthz`、metrics `/metrics`)
- 管理：`http://127.0.0.1:8788` （admin 密码在 `data/passwd.txt`）

### 2. 通过 Docker

```bash
cd deploy
POOL_MASTER_KEY=$(openssl rand -hex 32) docker compose up -d
```

### 3. 一键部署到 VPS

```bash
sudo bash install.sh
```

### 4. 试一下 API

```bash
# OpenAI 协议（下游传 gpt-5.3，被自动 alias 到 gpt-4o）
curl -N http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer sk-pool-CHANGEME-openai" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}'

# Anthropic 协议
curl http://127.0.0.1:8787/v1/messages \
  -H "x-api-key: sk-pool-CHANGEME-claude" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-opus-4-7","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'

# Gemini 协议
curl http://127.0.0.1:8787/v1beta/models/gemini-2.0-pro:generateContent \
  -H "x-goog-api-key: sk-pool-CHANGEME-gemini" \
  -H "Content-Type: application/json" \
  -d '{"contents":[{"parts":[{"text":"hi"}]}]}'
```

---

## 目录结构

```
llm-pool/
├── cmd/gateway/             # 二进制入口
├── internal/
│   ├── domain/              # 核心数据模型（Tenant/Group/Account/QuotaState/...）
│   ├── config/              # YAML 配置加载与默认值
│   ├── store/               # SQLite + AES-GCM 加密
│   ├── auth/                # apikey → tenant/group 解析
│   ├── router/              # model 别名重写、conversation 哈希
│   ├── protocol/
│   │   ├── ir/              # 内部表示
│   │   ├── openai/          # OpenAI Chat Completions decode/encode
│   │   ├── anthropic/       # Anthropic Messages decode/encode
│   │   └── gemini/          # Gemini generateContent decode/encode
│   ├── provider/
│   │   ├── chatgpt/         # ChatGPT web 适配器（mock + real stub）
│   │   ├── claude/          # Claude.ai web 适配器（mock + real stub）
│   │   └── gemini/          # Gemini web 适配器（mock + real stub）
│   ├── scheduler/           # 三态置信度 + P2C + Breaker + 绝不放弃
│   ├── stream/              # Head Buffer 状态机（无感失败转移）
│   ├── stealth/             # Persona Bundle、UA / Client Hints 池
│   ├── server/              # HTTP 入站层（chi 路由 + 中间件 + 流水线）
│   ├── admin/               # Web UI + admin JSON API
│   └── metrics/             # Prometheus 指标
├── deploy/
│   ├── Dockerfile           # 含 chromium 主镜像
│   ├── Dockerfile.slim      # 不含 chromium 轻量变体
│   └── docker-compose.yml
├── config/
│   └── config.example.yaml  # 完整带注释的配置模板
├── scripts/
│   └── dev-sync-to-windows.sh  # WSL → Windows 镜像 rsync watcher
├── install.sh               # 一键部署脚本
└── docs/                    # 留待补充
```

---

## 部署形态约定

- 推荐在 WSL/Linux 原生文件系统中开发（Go/Node 构建性能更稳定）
- Windows 侧可作为最终 `git add/commit/push` 工作区
- 默认监听：网关 `:8787`、Web UI `:8788`
- 卷挂载：`/data`（SQLite + passwd.txt）、`/config`（config.yaml）

---

## 测试

```bash
go test ./...
```

覆盖：
- `internal/protocol/openai`：解码、流式编码、非流式编码、多模态文本
- `internal/router`：alias 优先级、白名单拒绝、对话哈希稳定性
- `internal/scheduler`：P2C 选择、sticky 路由、never-fail probe 恢复、breaker 开闭
- `internal/stream`：head buffer 静默重试、超时后 commit + 继续 piping

---

## 开发约定

- Go 1.22+（实际依赖 GOMEMLIMIT、log/slog 等 1.21+ 特性）
- 不引入前端 SPA 框架；HTMX + 自定义 CSS
- 严禁在 `data/` 目录下做开发期变更（容器写入区）
- 所有敏感字段走 AES-GCM；主密钥从 `POOL_MASTER_KEY` 环境变量读取

---

## 切换到 Real 模式

把单家 provider 从 mock 切到 real 的步骤（以 ChatGPT 为例）：
1. 用浏览器登录 `chatgpt.com`，DevTools 抓取 cookies / accessToken
2. 通过 admin API 注册账号，把 cookies 存入 SQLite（凭证字段加密）
3. 在 `internal/provider/chatgpt/chatgpt.go` 的 `invokeReal` 中实现：
   - 通过 stealth.Bundle 构造 `*http.Client`
   - POST `/backend-api/conversation` with action=next、proper headers
   - 解析 SSE 流，转换为 `ir.Event` 推到 channel
4. 启动时 `PROVIDER_MODE=real`

具体协议形态在 plan §六 / §七 / §八点七 中有详细说明。
