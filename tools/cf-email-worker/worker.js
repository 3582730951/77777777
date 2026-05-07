/**
 * Cloudflare Email Worker — 接收邮件 + 管理 API
 *
 * 功能:
 * 1. 通过 Email Routing 接收邮件，存储到 KV
 * 2. 提供 Admin API 创建邮箱地址、查询收件箱
 *
 * 部署步骤见同目录 README
 */

const ADMIN_TOKEN = "CHANGE_ME_TO_A_RANDOM_STRING"; // 在 wrangler.toml 中通过环境变量覆盖

export default {
  // ===== HTTP 请求处理 =====
  async fetch(request, env) {
    const url = new URL(request.url);
    const path = url.pathname;
    const adminToken = env.ADMIN_TOKEN || ADMIN_TOKEN;

    // CORS
    if (request.method === "OPTIONS") {
      return new Response(null, { headers: corsHeaders() });
    }

    // Admin API 认证
    if (path.startsWith("/admin")) {
      const auth = request.headers.get("x-admin-auth") || "";
      if (auth !== adminToken) {
        return json({ error: "Unauthorized" }, 401);
      }
    }

    // 路由
    if (path === "/admin/new_address" && request.method === "POST") {
      return handleNewAddress(request, env);
    }
    if (path === "/admin/mails" && request.method === "GET") {
      return handleGetMails(url, env);
    }
    if (path === "/admin/delete" && request.method === "POST") {
      return handleDelete(request, env);
    }
    if (path === "/health") {
      return json({ ok: true, service: "cf-email-worker" });
    }

    return json({ error: "Not Found" }, 404);
  },

  // ===== 邮件接收处理 =====
  async email(message, env) {
    const to = message.to;
    const from = message.from;
    const subject = message.headers.get("subject") || "";

    // 读取邮件原始内容
    let raw = "";
    try {
      const reader = message.raw.getReader();
      const chunks = [];
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        chunks.push(value);
      }
      const combined = new Uint8Array(chunks.reduce((acc, c) => acc + c.length, 0));
      let offset = 0;
      for (const chunk of chunks) {
        combined.set(chunk, offset);
        offset += chunk.length;
      }
      raw = new TextDecoder().decode(combined);
    } catch (e) {
      raw = `Error reading email: ${e.message}`;
    }

    // 存储到 KV
    // Key: mail:{to}:{timestamp}:{random}
    const mailId = `${Date.now()}_${Math.random().toString(36).slice(2, 8)}`;
    const key = `mail:${to}:${mailId}`;
    const mailData = {
      id: mailId,
      from: from,
      to: to,
      subject: subject,
      raw: raw,
      receivedAt: new Date().toISOString(),
    };

    await env.MAIL_KV.put(key, JSON.stringify(mailData), {
      expirationTtl: 86400, // 24 小时后自动过期
    });

    // 同时维护一个地址索引
    const indexKey = `index:${to}`;
    let index = [];
    try {
      const existing = await env.MAIL_KV.get(indexKey);
      if (existing) index = JSON.parse(existing);
    } catch {}
    index.push({ id: mailId, key: key, receivedAt: mailData.receivedAt });
    // 只保留最近 50 封
    if (index.length > 50) index = index.slice(-50);
    await env.MAIL_KV.put(indexKey, JSON.stringify(index), {
      expirationTtl: 86400,
    });

    console.log(`Received email for ${to} from ${from}: ${subject}`);
  },
};

// ===== API 处理函数 =====

async function handleNewAddress(request, env) {
  const body = await request.json().catch(() => ({}));
  const domain = body.domain || env.DEFAULT_DOMAIN || "";
  const name = body.name || randomString(10);

  if (!domain) {
    return json({ error: "domain is required" }, 400);
  }

  const email = `${name}@${domain}`;

  // 初始化索引
  const indexKey = `index:${email}`;
  await env.MAIL_KV.put(indexKey, JSON.stringify([]), { expirationTtl: 86400 });

  return json({
    email: email,
    address: email,
    token: email, // 用 email 本身作为 token
  });
}

async function handleGetMails(url, env) {
  const address = url.searchParams.get("address") || "";
  const limit = parseInt(url.searchParams.get("limit") || "20");

  if (!address) {
    return json({ error: "address parameter required" }, 400);
  }

  // 直接通过 KV list 按前缀查找邮件（不依赖 index，避免一致性问题）
  const prefix = `mail:${address}:`;
  const listed = await env.MAIL_KV.list({ prefix: prefix, limit: limit });

  const results = [];
  for (const key of listed.keys) {
    try {
      const mailData = await env.MAIL_KV.get(key.name);
      if (mailData) {
        results.push(JSON.parse(mailData));
      }
    } catch {}
  }

  // 按时间倒序
  results.sort((a, b) => (b.receivedAt || "").localeCompare(a.receivedAt || ""));

  return json({ results: results });
}

async function handleDelete(request, env) {
  const body = await request.json().catch(() => ({}));
  const address = body.address || "";

  if (!address) {
    return json({ error: "address required" }, 400);
  }

  // 删除索引和所有邮件
  const indexKey = `index:${address}`;
  let index = [];
  try {
    const data = await env.MAIL_KV.get(indexKey);
    if (data) index = JSON.parse(data);
  } catch {}

  for (const item of index) {
    await env.MAIL_KV.delete(item.key);
  }
  await env.MAIL_KV.delete(indexKey);

  return json({ ok: true, deleted: index.length });
}

// ===== 工具函数 =====

function randomString(length) {
  const chars = "abcdefghijklmnopqrstuvwxyz0123456789";
  let result = "";
  for (let i = 0; i < length; i++) {
    result += chars[Math.floor(Math.random() * chars.length)];
  }
  return result;
}

function json(data, status = 200) {
  return new Response(JSON.stringify(data), {
    status,
    headers: { "Content-Type": "application/json", ...corsHeaders() },
  });
}

function corsHeaders() {
  return {
    "Access-Control-Allow-Origin": "*",
    "Access-Control-Allow-Methods": "GET, POST, OPTIONS",
    "Access-Control-Allow-Headers": "Content-Type, x-admin-auth, x-fingerprint",
  };
}
