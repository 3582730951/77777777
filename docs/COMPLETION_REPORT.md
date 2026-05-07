# 完成度报告 (2026-04-30)

## 用户原始诉求
"我想做一个账号池方便不用老是切换账号"，扩展为：
1. 多 LLM 订阅账号池（OpenAI/Claude/Gemini），自动轮询
2. 协议互转（兼容 Cursor / Kiro / Claude Code / Codex 等下游 IDE）
3. 智能调度（额度/限流/CF/健康度综合）
4. 反 CF 风控
5. 缓存命中 + token 优化（不降模型质量）
6. 双账号无感切换 + 杜绝"假死"
7. 模型发现 + 强制重写
8. Docker 部署 + 一键 sh 安装 + Nginx 自动
9. Apple 设计 Web UI + 多 VPS 联邦视图
10. WSL ↔ Windows 实时同步开发环境
11. 1C1G VPS 可行性
12. **要求"全自动落地全部计划"**

## 现实诚实声明
- plan 总规模 ≈ 数周资深工程师工作量
- 单会话内**不可能 100% 完成**反向工程级真实集成（ChatGPT/Claude/Gemini web 协议）
- 已交付：完整 **MVP 骨架**，全部架构决策落地、可编译可运行可测试，三家协议端到端走通（mock 模式）
- 真实凭证集成（`PROVIDER_MODE=real`）需用户提供活的订阅 cookie 后逐家完成

## 验收清单 14 项全部通过

| # | 验收项 | 结果 |
|---|---|---|
| V1 | OpenAI 协议入站，model 别名重写（gpt-5.3→gpt-4o），响应保留下游模型名，流式 SSE | ✅ |
| V2 | OpenAI 非流式响应（chat.completion 对象）| ✅ |
| V3 | Anthropic 协议入站（/v1/messages，stream + non-stream）| ✅ |
| V4 | Gemini 协议入站（/v1beta/.../generateContent）| ✅ |
| V5 | model 别名三层优先级（apikey > group > tenant）| ✅ |
| V6 | model 白名单拒绝越界请求（claude-opus 不在 openai 组）| ✅ |
| V7 | 缺失 API key 返回 401（不是 5xx）| ✅ |
| V8 | /metrics 暴露完整 pool_* 指标 | ✅ |
| V9 | Admin 登录页（Apple 风 CSS） | ✅ |
| V10 | Admin 路由保护（未登录重定向） | ✅ |
| V11 | Admin 登录 → /api/accounts 返回 EWMA、置信度、状态 | ✅ |
| V12 | 内存占用 19.2 MB RSS（远超 plan 250MB 目标） | ✅ |
| V13 | P2C 在多账号间均衡选择（dev-claude-1/2 都被选过）| ✅ |
| V14 | 全部 Go 单元测试通过 | ✅ |

## 单元测试覆盖
```
ok  github.com/llm-pool/gateway/internal/protocol/openai
ok  github.com/llm-pool/gateway/internal/router
ok  github.com/llm-pool/gateway/internal/scheduler
ok  github.com/llm-pool/gateway/internal/stream
```

测试用例：
- `TestDecodeBasic` / `TestDecodeMultimodalText` / `TestStreamEncodeRoundTrip` / `TestNonStreamingResponse`
- `TestModelRewriteGroupAlias` / `TestModelRewriteAPIKeyOverridesGroup` / `TestModelWhitelistRejects` / `TestHashConversationStable`
- `TestPickHappyPath` / `TestStickyRouting` / `TestNeverFailProbeRecovers` / `TestNeverFailReturnsErrorWhenDisabled` / `TestBreakerOpensThenRecovers`
- `TestHeadBufferSilentRetry` / `TestHeadBufferCommitsAfterTimeout`

## 代码统计
- **4,811 行 Go 代码**（不含模板、CSS、配置）
- 21 个 internal package
- ~700 行 install.sh + Dockerfile + docker-compose 部署侧
- ~250 行 Apple 风格 CSS + 4 个 HTML 模板（dashboard/accounts/groups/cluster + login）

## 架构决策落地一一对照

| Plan 章节 | 关键设计 | 落地位置 |
|---|---|---|
| §一 技术栈 | Go + chi + sonic | `go.mod` + `cmd/gateway/main.go` |
| §二 顶层架构 | 8 层流水线 | `internal/server/server.go` 的 `serveRequest` |
| §四 数据模型 | Tenant/Group/Account/Quota/Health/PersonaBundle | `internal/domain/types.go` + `internal/stealth/stealth.go` |
| §五 协议互转 | IR + N+M 适配器 | `internal/protocol/{ir,openai,anthropic,gemini}/` |
| §六 调度 P2C+EWMA | `internal/scheduler/scheduler.go` | ✅ |
| §七 反 CF | Persona Bundle 数据结构 + Header 池 | `internal/stealth/stealth.go` (TLS 指纹注入待 v1) |
| §八 持久化 | SQLite + AES-GCM | `internal/store/store.go` |
| §八点五 缓存优化 | 数据结构齐全（ConvHash/sticky）；上游 conv_id 复用待真实 provider 接入后启用 | `internal/router/router.go` HashConversation, scheduler.Sticky |
| §八点六 无感失败转移 | Head Buffer + Never-Fail + Background Prober | `internal/stream/headbuffer.go` + `internal/scheduler/{scheduler,prober,classify}.go` |
| §八点七 模型发现 + 重写 | provider.Discover() + ModelRewriter 中间件 | `internal/provider/*/*.go` (mock) + `internal/router/router.go` |
| §八点八 部署 | install.sh + Dockerfile + docker-compose | `install.sh`, `deploy/*` |
| §八点九 Apple Web UI | 5 模板 + 250 行 CSS | `internal/admin/{templates,static}/` |
| §九 可观测性 | Prometheus + slog | `internal/metrics/metrics.go` |
| §十点五 联邦集群 | 拉对端 snapshot 聚合 | `internal/admin/admin.go` `handleCluster` |
| §十点六 1C1G | GOMEMLIMIT/GOGC + 实测 19MB | `cmd/gateway/main.go` `applyResourceProfile` |

## 同步通路
- WSL 主开发：`/home/12/llm-pool/`
- Windows 镜像：`/mnt/d/Code/R3_Code/MI/MI_test_account/`
- watcher 守护进程已启动（PID 在 /tmp/llm-pool-sync.log 可见）
- 验证：保存即同步（< 1s 延迟）

## 下次继续工作的起点
推荐顺序：
1. **真实 ChatGPT 集成**：捕获 chatgpt.com 的 `/backend-api/conversation` 协议样本，实现 `internal/provider/chatgpt/chatgpt.go` 的 `invokeReal`。这一步打通后整体可对外服务。
2. **TLS 指纹**：替换 `internal/stealth/stealth.go` 的 `*http.Transport` 为 `bogdanfinn/tls-client`。
3. **rod headless**：实现 `internal/browser/` 的 lazy launch + CF challenge 自动恢复。
4. **真实 Claude / Gemini**：依次反向。
5. **ConversationMapper 实战**：捕获到上游 conversation_id 后，把 sticky 路由升级为"同 conv_id 增量发送"的真实优化。

## 文件位置
- 项目根：`/home/12/llm-pool/`
- Plan：`/root/.claude/plans/lucky-percolating-pebble.md`
- 同步日志：`/tmp/llm-pool-sync.log`
- 网关日志（运行时）：`/tmp/gateway.log`
