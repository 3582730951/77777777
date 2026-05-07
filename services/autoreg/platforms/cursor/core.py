"""Cursor 注册协议核心实现（动态 action hash + 新字段格式）"""
import re, uuid, json, urllib.parse, random, string, hashlib, time, base64
from typing import Optional, Callable

AUTH   = "https://authenticator.cursor.sh"
CURSOR = "https://cursor.com"

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
      "AppleWebKit/537.36 (KHTML, like Gecko) "
      "Chrome/131.0.0.0 Safari/537.36")

TURNSTILE_SITEKEY = "0x4AAAAAAAMNIvC45A4Wjjln"

# Fallback action hashes (extracted 2026-05-03, will be overridden dynamically)
_FALLBACK_HASHES = [
    "a67eb6646e43eddcbd0d038cbee664aac59f5a53",  # fingerprint init
    "b263a6ee1ac854642026b529d988f13fc058958b",  # sign-up form action
    "cbdba1d1e041e600f1d7877f5e502011e412c3cd",  # magic-code / OTP
    "83d436e6f738e8da254ef0551893aaf046f8caa1",
    "408357ea3fbf03333b5369c631d5393cc7eb4bbf",
    "ba7bb1d4eacd17ddc550f776493caea3a42e96d3",
]

# Next.js router state tree for sign-up page
_ROUTER_STATE_TREE = urllib.parse.quote(json.dumps(
    ["", {"children": ["(main)", {"children": ["(root)", {"children": [
        "sign-up", {"children": ["__PAGE__", {}, None, None]}, None, None
    ]}, None, None]}, None, None]}, None, None, True]
))


def _rand_password(n=16):
    chars = string.ascii_letters + string.digits + "!@#$"
    return "".join(random.choices(chars, k=n))


def _boundary():
    return "----WebKitFormBoundary" + "".join(
        random.choices(string.ascii_letters + string.digits, k=16))


def _multipart(fields: dict, boundary: str) -> bytes:
    parts = []
    for name, value in fields.items():
        parts.append(
            f"--{boundary}\r\n"
            f'Content-Disposition: form-data; name="{name}"\r\n\r\n'
            f"{value}\r\n"
        )
    parts.append(f"--{boundary}--\r\n")
    return "".join(parts).encode()


def _build_signals():
    """Build the 'signals' hidden field value (browser fingerprint)."""
    payload = {
        "createdAtMs": int(time.time() * 1000),
        "timezone": "Asia/Shanghai",
        "language": "en-US",
        "hardwareConcurrency": random.choice([4, 8, 12, 16]),
        "webdriver": False,
        "userAgent": UA,
        "appVersion": UA.replace("Mozilla/", ""),
    }
    return base64.b64encode(json.dumps(payload).encode()).decode()


class CursorRegister:
    def __init__(self, proxy: str = None, log_fn: Callable = print):
        from curl_cffi import requests as curl_req
        self.log = log_fn
        self.s = curl_req.Session(impersonate="chrome")
        if proxy:
            self.s.proxies = {"http": proxy, "https": proxy}
        self._action_hashes = list(_FALLBACK_HASHES)
        self._action_fingerprint = _FALLBACK_HASHES[0]
        self._action_signup = _FALLBACK_HASHES[1]
        self._action_magic_code = _FALLBACK_HASHES[2]
        self._auth_session_id = ""
        self._state_raw = ""

    def _extract_action_hashes(self, html: str):
        """Dynamically extract action hashes from page JS bundles."""
        js_urls = re.findall(r'"(/_next/static/chunks/[^"]+\.js)"', html)
        layout_urls = [u for u in js_urls if 'layout' in u]
        for url in layout_urls:
            try:
                jr = self.s.get(AUTH + url)
                js = jr.text
                hashes = re.findall(r'\$\)\("([a-f0-9]{40})"\)', js)
                if len(hashes) >= 3:
                    self._action_hashes = list(set(hashes))
                    self._action_fingerprint = hashes[0]
                    self.log(f"动态提取到 {len(hashes)} 个 action hash")
                    return hashes
            except Exception:
                pass
        self.log("使用 fallback action hash")
        return self._action_hashes

    def _extract_page_params(self, html: str):
        """Extract authorization_session_id and state from the page HTML."""
        m = re.search(r'authorization_session_id[=:][\s"]*([A-Z0-9]+)', html)
        if m:
            self._auth_session_id = m.group(1)
        m2 = re.search(r'name="state"[^>]*value="([^"]+)"', html)
        if m2:
            self._state_raw = m2.group(1)

    def _form_headers(self, next_action: str, referer: str, boundary: str):
        return {
            "user-agent": UA,
            "accept": "text/x-component",
            "content-type": f"multipart/form-data; boundary={boundary}",
            "origin": AUTH,
            "referer": referer,
            "next-action": next_action,
            "next-router-state-tree": _ROUTER_STATE_TREE,
        }

    def _action_headers(self, next_action: str, referer: str):
        return {
            "user-agent": UA,
            "accept": "text/x-component",
            "content-type": "text/plain;charset=UTF-8",
            "origin": AUTH,
            "referer": referer,
            "next-action": next_action,
            "next-router-state-tree": _ROUTER_STATE_TREE,
        }

    def step1_get_session(self):
        """Load the signup page, extract session params and action hashes."""
        nonce = str(uuid.uuid4())
        state = {"returnTo": "/dashboard", "nonce": nonce}
        state_json = json.dumps(state)
        state_encoded = urllib.parse.quote(state_json)

        # First hit cursor.com login to get proper redirect
        login_url = f"{CURSOR}/api/auth/login?redirect_uri=/dashboard"
        try:
            self.s.get(login_url, headers={"user-agent": UA, "accept": "text/html"},
                       allow_redirects=True, timeout=15)
        except Exception:
            pass

        # Then hit the signup page
        signup_url = (
            f"{AUTH}/sign-up"
            f"?client_id=client_01GS6W3C96KW4WRS6Z93JCE2RJ"
            f"&redirect_uri={urllib.parse.quote('https://cursor.com/api/auth/callback')}"
            f"&state={urllib.parse.quote(state_json)}"
        )
        r = self.s.get(signup_url, headers={"user-agent": UA, "accept": "text/html"},
                       allow_redirects=True, timeout=30)
        html = r.text

        # Extract authorization_session_id from URL or HTML
        url_match = re.search(r'authorization_session_id=([A-Z0-9]+)', r.url)
        if url_match:
            self._auth_session_id = url_match.group(1)
        self._extract_page_params(html)

        # Extract action hashes from JS bundles
        self._extract_action_hashes(html)

        # Determine action hashes based on layout JS pattern
        # First hash = fingerprint, others need to be mapped
        # Try to identify signup and magic-code actions from page JS
        for url in re.findall(r'"(/_next/static/chunks/[^"]+\.js)"', html):
            try:
                js = self.s.get(AUTH + url).text
                hashes_in_file = re.findall(r'"([a-f0-9]{40})"', js)
                if 'magic-code' in js or 'magic_code' in js:
                    for h in hashes_in_file:
                        if h not in [self._action_fingerprint]:
                            self._action_magic_code = h
                            break
                if 'formAction' in js or 'useFormState' in js:
                    for h in hashes_in_file:
                        if h != self._action_fingerprint and h != self._action_magic_code:
                            self._action_signup = h
                            break
            except Exception:
                pass

        # Send fingerprint init (like the browser does on page load)
        signals = _build_signals()
        sig_hash = hashlib.sha256(signals.encode()).hexdigest()
        try:
            self.s.post(
                signup_url,
                headers=self._action_headers(self._action_fingerprint, signup_url),
                data=json.dumps([sig_hash]),
                allow_redirects=False,
            )
        except Exception:
            pass

        # Find state cookie
        state_cookie_name = None
        for cookie in self.s.cookies.jar:
            if 'state-' in cookie.name or '__Host-state' in cookie.name:
                state_cookie_name = cookie.name
                break

        self.log(f"session_id={self._auth_session_id}, state_cookie={state_cookie_name}")
        return state_encoded, state_cookie_name

    def step2_submit_email(self, email: str, state_encoded: str,
                           first_name: str = "", last_name: str = ""):
        """Submit the email signup form."""
        if not first_name:
            first_name = ''.join(random.choices(string.ascii_lowercase, k=5)).capitalize()
        if not last_name:
            last_name = ''.join(random.choices(string.ascii_lowercase, k=5)).capitalize()

        bd = _boundary()
        referer = (
            f"{AUTH}/sign-up"
            f"?client_id=client_01GS6W3C96KW4WRS6Z93JCE2RJ"
            f"&redirect_uri={urllib.parse.quote('https://cursor.com/api/auth/callback')}"
            f"&state={state_encoded}"
        )
        if self._auth_session_id:
            referer += f"&authorization_session_id={self._auth_session_id}"

        fields = {
            "first_name": first_name,
            "last_name": last_name,
            "email": email,
            "signals": _build_signals(),
            "redirect_uri": "https://cursor.com/api/auth/callback",
            "state": state_encoded,
        }
        if self._auth_session_id:
            fields["authorization_session_id"] = self._auth_session_id

        body = _multipart(fields, bd)
        r = self.s.post(
            f"{AUTH}/sign-up",
            headers=self._form_headers(self._action_signup, referer, bd),
            data=body, allow_redirects=False,
        )
        self.log(f"Step2 status={r.status_code}")

    def step3_submit_password(self, password: str, email: str, state_encoded: str,
                              captcha_solver=None):
        """Submit the password form with Turnstile captcha."""
        captcha_token = ""
        if captcha_solver:
            self.log("获取 Turnstile token...")
            captcha_token = captcha_solver.solve_turnstile(AUTH, TURNSTILE_SITEKEY)
            if captcha_token:
                self.log(f"Turnstile token: {captcha_token[:40]}...")

        bd = _boundary()
        referer = f"{AUTH}/sign-up/password?state={state_encoded}"
        if self._auth_session_id:
            referer += f"&authorization_session_id={self._auth_session_id}"

        fields = {
            "email": email,
            "password": password,
            "signals": _build_signals(),
            "redirect_uri": "https://cursor.com/api/auth/callback",
            "state": state_encoded,
        }
        if captcha_token:
            fields["captchaToken"] = captcha_token
        if self._auth_session_id:
            fields["authorization_session_id"] = self._auth_session_id

        body = _multipart(fields, bd)
        r = self.s.post(
            f"{AUTH}/sign-up",
            headers=self._form_headers(self._action_signup, referer, bd),
            data=body, allow_redirects=False,
        )
        self.log(f"Step3 status={r.status_code}, matched={r.headers.get('x-matched-path', 'N/A')}")

    def step4_submit_otp(self, otp: str, email: str, state_encoded: str):
        """Submit the OTP verification code."""
        bd = _boundary()
        referer = f"{AUTH}/sign-up/email-verification?state={state_encoded}"

        fields = {
            "email": email,
            "otp": otp,
            "signals": _build_signals(),
            "redirect_uri": "https://cursor.com/api/auth/callback",
            "state": state_encoded,
            "intent": "magic-code",
        }
        if self._auth_session_id:
            fields["authorization_session_id"] = self._auth_session_id

        body = _multipart(fields, bd)
        r = self.s.post(
            f"{AUTH}/sign-up",
            headers=self._form_headers(self._action_magic_code, referer, bd),
            data=body, allow_redirects=False,
        )
        self.log(f"Step4 status={r.status_code}")

        # Check for redirect with auth code
        loc = r.headers.get("location", "")
        m = re.search(r'code=([\w-]+)', loc)
        if m:
            return m.group(1)

        # Also check response body for redirect URL
        try:
            body_text = r.text
            m2 = re.search(r'code=([\w-]+)', body_text)
            if m2:
                return m2.group(1)
        except Exception:
            pass

        return ""

    def step5_get_token(self, auth_code: str, state_encoded: str):
        """Exchange auth code for WorkosCursorSessionToken."""
        url = f"{CURSOR}/api/auth/callback?code={auth_code}&state={state_encoded}"
        self.s.get(url, headers={"user-agent": UA, "accept": "text/html"},
                   allow_redirects=False)
        for cookie in self.s.cookies.jar:
            if cookie.name == "WorkosCursorSessionToken":
                return urllib.parse.unquote(cookie.value)
        # Try with redirects
        self.s.get(url, headers={"user-agent": UA}, allow_redirects=True)
        for cookie in self.s.cookies.jar:
            if cookie.name == "WorkosCursorSessionToken":
                return urllib.parse.unquote(cookie.value)
        return ""


from platforms.cursor.browser_register import CursorBrowserRegister  # noqa: F401
