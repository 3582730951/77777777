"""Kiro OAuth 浏览器流程。"""
from urllib.parse import parse_qs, urlparse

import cbor2

from core.oauth_browser import (
    OAuthBrowser,
    browser_login_method_text,
    finalize_oauth_email,
    oauth_provider_label,
)
from platforms.kiro.core import KIRO, KiroRegister, UA, _uuid


def _extract_callback_authorization(callback_url: str, expected_state: str) -> tuple[str, str]:
    parsed = urlparse(callback_url)
    expected = urlparse(KIRO)
    if parsed.scheme != expected.scheme or parsed.netloc != expected.netloc or parsed.path != "/signin/oauth":
        raise RuntimeError("Kiro OAuth 回调地址不正确")

    query = parse_qs(parsed.query)
    auth_codes = query.get("code") or []
    redirect_states = query.get("state") or []
    if not auth_codes:
        raise RuntimeError("Kiro OAuth 回调里缺少 code")
    if len(auth_codes) != 1:
        raise RuntimeError("Kiro OAuth 回调里的 code 参数数量不正确")
    if not redirect_states:
        raise RuntimeError("Kiro OAuth 回调里缺少 state")
    if len(redirect_states) != 1:
        raise RuntimeError("Kiro OAuth 回调里的 state 参数数量不正确")
    auth_code = auth_codes[0]
    redirect_state = redirect_states[0]
    if redirect_state != expected_state:
        raise RuntimeError("Kiro OAuth state 不匹配")
    return auth_code, redirect_state


def _is_expected_oauth_callback_url(url: str, expected_state: str) -> bool:
    try:
        _extract_callback_authorization(url, expected_state)
        return True
    except RuntimeError:
        return False


def _exchange_callback_tokens(reg: KiroRegister, callback_url: str):
    auth_code, redirect_state = _extract_callback_authorization(callback_url, reg.state)

    exchange_body = cbor2.dumps({
        "code": auth_code,
        "codeVerifier": reg.cv,
        "idp": "BuilderId",
        "redirectUri": f"{KIRO}/signin/oauth",
        "state": redirect_state,
    })
    exchange_headers = {
        **UA,
        "accept": "application/cbor",
        "content-type": "application/cbor",
        "smithy-protocol": "rpc-v2-cbor",
        "origin": KIRO,
        "referer": f"{KIRO}/signin",
        "x-kiro-visitorid": reg.vid,
        "amz-sdk-invocation-id": _uuid(),
        "amz-sdk-request": "attempt=1; max=1",
        "x-amz-user-agent": "aws-sdk-js/1.0.0 ua/2.1 os/macOS lang/js md/browser#Chromium_131 m/N,M,E",
    }
    response = reg.s.post(
        f"{KIRO}/service/KiroWebPortalService/operation/ExchangeToken",
        headers=exchange_headers,
        data=exchange_body,
        cookies={"kiro-visitor-id": reg.vid},
    )
    if response.status_code != 200:
        raise RuntimeError(f"Kiro ExchangeToken 失败: HTTP {response.status_code}")
    data = cbor2.loads(response.content)
    access_token = data.get("accessToken", "")
    if not access_token:
        raise RuntimeError("Kiro ExchangeToken 响应里缺少 accessToken")
    return {
        "accessToken": access_token,
        "csrfToken": data.get("csrfToken", ""),
        "expiresIn": data.get("expiresIn", 0),
    }


def register_with_browser_oauth(
    *,
    proxy: str | None = None,
    oauth_provider: str = "",
    email_hint: str = "",
    timeout: int = 300,
    log_fn=print,
    headless: bool = False,
    chrome_user_data_dir: str = "",
    chrome_cdp_url: str = "",
) -> dict:
    method_text = browser_login_method_text(oauth_provider)
    reg = KiroRegister(proxy=proxy, tag="KIRO-OAUTH")
    reg.log = log_fn
    redirect_url = reg.step1_kiro_init()
    if not redirect_url:
        raise RuntimeError("Kiro InitiateLogin 失败")

    with OAuthBrowser(
        proxy=proxy,
        headless=headless,
        chrome_user_data_dir=chrome_user_data_dir,
        chrome_cdp_url=chrome_cdp_url,
        log_fn=log_fn,
    ) as browser:
        browser.goto(redirect_url)
        if oauth_provider:
            browser.try_click_provider(oauth_provider)

        if chrome_user_data_dir or chrome_cdp_url:
            browser.auto_select_google_account()
        else:
            log_fn(f"请在浏览器中完成登录，可使用 {method_text}，最长等待 {timeout} 秒")
            if email_hint:
                log_fn(f"请确认最终登录账号邮箱为: {email_hint}")

        callback_url = browser.wait_for_url(
            lambda url: _is_expected_oauth_callback_url(url, reg.state),
            timeout=timeout,
        )
        if not callback_url:
            raise RuntimeError(f"Kiro 浏览器登录未在 {timeout} 秒内完成")

        token_info = _exchange_callback_tokens(reg, callback_url)
        resolved_email = finalize_oauth_email("", email_hint, "Kiro")
        return {
            "email": resolved_email,
            "accessToken": token_info.get("accessToken", ""),
            "sessionToken": browser.cookie_value("__Secure-authjs.session-token", domain_substrings=("kiro.dev",)),
            "csrfToken": token_info.get("csrfToken", ""),
            "expiresIn": token_info.get("expiresIn", 0),
        }


# Backward-compat alias
register_with_manual_oauth = register_with_browser_oauth
