"""Kiro Desktop OAuth PKCE flow — 获取 refreshToken。

通过 Kiro Desktop Auth 端点完成 OAuth 登录，
在本地监听回调拿 authorization code，换取 refreshToken。
"""
import hashlib, base64, json, os, secrets, threading, time, uuid
from http.server import HTTPServer, BaseHTTPRequestHandler
from urllib.parse import parse_qs, urlparse, urlencode
from typing import Callable, Optional

DESKTOP_AUTH = "https://prod.us-east-1.auth.desktop.kiro.dev"
CALLBACK_PORT_RANGE = range(19876, 19886)


def _pkce():
    verifier = base64.urlsafe_b64encode(secrets.token_bytes(32)).rstrip(b"=").decode()
    digest = hashlib.sha256(verifier.encode()).digest()
    challenge = base64.urlsafe_b64encode(digest).rstrip(b"=").decode()
    return verifier, challenge


class _CallbackHandler(BaseHTTPRequestHandler):
    """接收 OAuth callback 的 HTTP handler。"""
    result = None

    def do_GET(self):
        parsed = urlparse(self.path)
        qs = parse_qs(parsed.query)
        code = (qs.get("code") or [None])[0]
        state = (qs.get("state") or [None])[0]
        if code:
            _CallbackHandler.result = {"code": code, "state": state}
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.end_headers()
            self.wfile.write(b"<html><body><h2>Authorization successful!</h2><p>You can close this window.</p></body></html>")
        else:
            error = (qs.get("error") or ["unknown"])[0]
            _CallbackHandler.result = {"error": error}
            self.send_response(400)
            self.end_headers()
            self.wfile.write(f"Error: {error}".encode())

    def log_message(self, format, *args):
        pass  # suppress logs


def get_kiro_refresh_token(
    email: str,
    password: str,
    *,
    idp: str = "BuilderId",
    log_fn: Callable = print,
    timeout: int = 120,
) -> Optional[dict]:
    """通过 Kiro Desktop OAuth 获取 refreshToken。

    Returns dict with {accessToken, refreshToken, profileArn, expiresIn} or None。
    """
    from camoufox.sync_api import Camoufox

    code_verifier, code_challenge = _pkce()
    state = str(uuid.uuid4())

    # 找可用端口启动 callback server
    server = None
    port = None
    for p in CALLBACK_PORT_RANGE:
        try:
            server = HTTPServer(("127.0.0.1", p), _CallbackHandler)
            port = p
            break
        except OSError:
            continue
    if not server:
        log_fn("  ❌ 无可用端口启动 callback server")
        return None

    redirect_uri = f"http://127.0.0.1:{port}/oauth/callback"
    _CallbackHandler.result = None

    # 后台运行 callback server
    server_thread = threading.Thread(target=server.serve_forever, daemon=True)
    server_thread.start()
    log_fn(f"  Callback server 监听 :{port}")

    try:
        # 构造 Desktop Auth 登录 URL
        login_params = urlencode({
            "idp": idp,
            "redirectUri": redirect_uri,
            "codeChallenge": code_challenge,
            "codeChallengeMethod": "S256",
            "state": state,
        })
        login_url = f"{DESKTOP_AUTH}/login?{login_params}"
        log_fn(f"  登录 URL: {login_url[:100]}...")

        # 用 Camoufox 浏览器自动完成登录
        with Camoufox(headless=True) as browser:
            page = browser.new_page()
            page.goto(login_url, timeout=30000, wait_until="domcontentloaded")
            time.sleep(3)

            current_url = page.url
            log_fn(f"  页面 URL: {current_url[:100]}")

            # 应该重定向到 signin.aws 登录页
            if "signin.aws" in current_url:
                # 等待表单
                try:
                    page.wait_for_selector(
                        'input[name="username"], input[type="email"], input[name="email"]',
                        timeout=15000,
                    )
                except Exception:
                    pass

                # 填邮箱
                for sel in ['input[name="username"]', 'input[type="email"]', 'input[name="email"]']:
                    el = page.query_selector(sel)
                    if el and el.is_visible():
                        el.fill(email)
                        log_fn(f"  填写邮箱: {email}")
                        break
                time.sleep(0.5)

                # 点 Next/Continue
                for sel in ['button[type="submit"]', 'button:has-text("Next")', 'button:has-text("Continue")']:
                    btn = page.query_selector(sel)
                    if btn and btn.is_visible():
                        btn.click()
                        break
                time.sleep(5)

                # 填密码（需要 JWE 加密）
                # signin.aws 的密码提交需要从页面获取公钥加密
                # 检查是否到了密码页
                pwd_input = page.query_selector('input[type="password"]')
                if pwd_input and pwd_input.is_visible():
                    log_fn("  检测到密码页")
                    # signin.aws 的密码加密在前端 JS 里自动处理
                    # 直接填密码让浏览器处理加密
                    pwd_input.fill(password)
                    time.sleep(0.5)
                    for sel in ['button[type="submit"]', 'button:has-text("Sign in")', 'button:has-text("Next")']:
                        btn = page.query_selector(sel)
                        if btn and btn.is_visible():
                            btn.click()
                            break
                    log_fn("  提交密码...")
                    time.sleep(8)

                    # 检查是否登录成功（应该 redirect 回 localhost callback）
                    log_fn(f"  当前 URL: {page.url[:100]}")

            # 等待 callback
            deadline = time.time() + timeout
            while time.time() < deadline and _CallbackHandler.result is None:
                time.sleep(1)

            page.close()

        if not _CallbackHandler.result:
            log_fn("  ❌ 等待 callback 超时")
            return None

        if "error" in _CallbackHandler.result:
            log_fn(f"  ❌ OAuth 错误: {_CallbackHandler.result['error']}")
            return None

        auth_code = _CallbackHandler.result["code"]
        log_fn(f"  ✅ 拿到 authorization code: {auth_code[:40]}...")

        # 用 code + code_verifier 换 token
        from curl_cffi import requests as r
        token_resp = r.post(
            f"{DESKTOP_AUTH}/exchangeToken",
            json={
                "code": auth_code,
                "codeVerifier": code_verifier,
                "redirectUri": redirect_uri,
            },
            headers={"Content-Type": "application/json"},
            impersonate="chrome",
            timeout=15,
        )
        log_fn(f"  exchangeToken status: {token_resp.status_code}")

        if token_resp.status_code != 200:
            # 也试 /oauth/token
            token_resp = r.post(
                f"{DESKTOP_AUTH}/oauth/token",
                json={
                    "code": auth_code,
                    "code_verifier": code_verifier,
                    "redirect_uri": redirect_uri,
                    "grant_type": "authorization_code",
                },
                headers={"Content-Type": "application/json"},
                impersonate="chrome",
                timeout=15,
            )
            log_fn(f"  /oauth/token status: {token_resp.status_code}")

        if token_resp.status_code == 200:
            data = token_resp.json()
            log_fn(f"  ✅ 拿到 tokens: keys={list(data.keys())}")
            return {
                "accessToken": data.get("accessToken", ""),
                "refreshToken": data.get("refreshToken", ""),
                "profileArn": data.get("profileArn", ""),
                "expiresIn": data.get("expiresIn", 0),
            }
        else:
            log_fn(f"  ❌ token 交换失败: {token_resp.text[:200]}")
            return None

    finally:
        server.shutdown()
