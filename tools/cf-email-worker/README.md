# CF Email Worker 部署指南

## 前提条件
- 一个 Cloudflare 账号
- 域名（如 cnmlgb.de）已添加到 Cloudflare
- 安装了 Node.js

## 部署步骤

### 1. 安装 Wrangler CLI
```bash
npm install -g wrangler
wrangler login
```

### 2. 创建 KV 存储
```bash
wrangler kv namespace create "MAIL_KV"
```
会输出类似：
```
id = "abc123def456..."
```
把这个 ID 填入 `wrangler.toml` 的 `id` 字段。

### 3. 修改配置
编辑 `wrangler.toml`：
- `ADMIN_TOKEN`：改为一个随机字符串（自己生成，如 `openssl rand -hex 32`）
- `DEFAULT_DOMAIN`：你的邮箱域名（如 `cnmlgb.de`）
- `id`：第 2 步得到的 KV ID

### 4. 部署 Worker
```bash
cd /path/to/cf-email-worker
wrangler deploy
```
部署成功后会显示 Worker URL，类似：
```
https://email-receiver.your-account.workers.dev
```

### 5. 配置 Email Routing
在 Cloudflare Dashboard：
1. 进入你的域名 → **Email** → **Email Routing**
2. **Routes** 标签 → 点击 **Catch-all address**
3. Action 选择 **Send to a Worker**
4. Destination 选择 `email-receiver`（刚部署的 Worker）
5. 保存

### 6. 在 autoreg 中配置
在 autoreg 前端的邮箱设置中添加 CF Worker：
- **API 地址**：`https://email-receiver.your-account.workers.dev`
- **Admin Token**：第 3 步设置的 ADMIN_TOKEN
- **邮箱域名**：`cnmlgb.de`

### 7. 测试
```bash
# 创建邮箱
curl -X POST https://email-receiver.xxx.workers.dev/admin/new_address \
  -H "x-admin-auth: YOUR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"domain":"cnmlgb.de"}'

# 查询收件箱
curl "https://email-receiver.xxx.workers.dev/admin/mails?address=xxx@cnmlgb.de" \
  -H "x-admin-auth: YOUR_TOKEN"
```

## API 说明

| 端点 | 方法 | 说明 |
|------|------|------|
| `/admin/new_address` | POST | 创建邮箱地址，body: `{"domain":"xxx.com","name":"可选"}` |
| `/admin/mails` | GET | 查询收件箱，params: `address=xxx@xxx.com&limit=20` |
| `/admin/delete` | POST | 删除邮箱，body: `{"address":"xxx@xxx.com"}` |
| `/health` | GET | 健康检查 |

认证：所有 `/admin` 路径需要 `x-admin-auth` header。
