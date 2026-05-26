from __future__ import annotations

from types import SimpleNamespace
from urllib.parse import parse_qs, urlparse

import pytest

from core.base_platform import RegisterConfig
from platforms.chatgpt import browser_register as browser_register_module
from platforms.chatgpt.constants import (
    CODEX_CLIENT_ID,
    CODEX_ORIGINATOR,
    CODEX_REDIRECT_URI,
    CODEX_SCOPE,
)
from platforms.chatgpt.oauth import generate_oauth_url
from platforms.chatgpt.plugin import (
    ChatGPTPlatform,
    _assert_complete_oauth_callback,
    _generate_chatgpt_registration_password,
)
from platforms.chatgpt.switch import extract_session_token


def test_assert_complete_oauth_callback_accepts_complete_payload():
    _assert_complete_oauth_callback({
        "account_id": "acct_123",
        "access_token": "at_123",
        "refresh_token": "rt_123",
        "id_token": "id_123",
    })


def test_assert_complete_oauth_callback_rejects_partial_payload():
    with pytest.raises(RuntimeError, match="完整 OAuth callback"):
        _assert_complete_oauth_callback({
            "account_id": "acct_123",
            "access_token": "at_123",
            "refresh_token": "",
            "id_token": "",
        })


def test_generate_chatgpt_registration_password_meets_openai_strength_requirements():
    for _ in range(8):
        password = _generate_chatgpt_registration_password()
        assert len(password) >= 12
        assert any(ch.islower() for ch in password)
        assert any(ch.isupper() for ch in password)
        assert any(ch.isdigit() for ch in password)
        assert any(ch in ",._!@#" for ch in password)


def test_codex_oauth_url_matches_official_shape():
    oauth_start = generate_oauth_url(
        redirect_uri=CODEX_REDIRECT_URI,
        scope=CODEX_SCOPE,
        client_id=CODEX_CLIENT_ID,
    )
    parsed = urlparse(oauth_start.auth_url)
    query = parse_qs(parsed.query)

    assert (
        f"{parsed.scheme}://{parsed.netloc}{parsed.path}"
        == "https://auth.openai.com/oauth/authorize"
    )
    assert query["response_type"] == ["code"]
    assert query["client_id"] == [CODEX_CLIENT_ID]
    assert query["redirect_uri"] == [CODEX_REDIRECT_URI]
    assert query["scope"] == [CODEX_SCOPE]
    assert query["code_challenge_method"] == ["S256"]
    assert query["id_token_add_organizations"] == ["true"]
    assert query["codex_cli_simplified_flow"] == ["true"]
    assert query["originator"] == [CODEX_ORIGINATOR]
    assert "prompt" not in query
    assert "screen_hint" not in query
    assert "scope=openid%20profile%20email%20offline_access" in oauth_start.auth_url
    assert "api.connectors." not in oauth_start.auth_url


def test_extract_session_token_accepts_chunked_chrome_cookie_table():
    cookies = "\n".join([
        "Name\tValue\tDomain\tPath\tExpires\tSize\tHttpOnly\tSecure\tSameSite\tPriority",
        "__Secure-next-auth.session-token.0\tpart0\t.chatgpt.com\t/\t2026-08-24T05:54:55.225Z\t3967\t✓\t✓\tLax\tMedium",
        "__Secure-next-auth.session-token.1\tpart1\t.chatgpt.com\t/\t2026-08-24T05:54:55.227Z\t77\t✓\t✓\tLax\tMedium",
        "cf_clearance\tclear-token\t.chatgpt.com\t/\t2027-05-26T05:51:27.062Z\t417\t✓\t✓\tNone\tMedium",
    ])
    assert extract_session_token(cookies=cookies) == "part0part1"


def test_chatgpt_platform_preserves_user_supplied_password():
    platform = object.__new__(ChatGPTPlatform)
    assert platform._prepare_registration_password("Secret123!") == "Secret123!"


def test_protocol_mailbox_mapper_rejects_partial_oauth_result():
    platform = object.__new__(ChatGPTPlatform)
    platform.mailbox = None
    platform.config = RegisterConfig()
    adapter = ChatGPTPlatform.build_protocol_mailbox_adapter(platform)
    ctx = SimpleNamespace(password="Secret123!", proxy=None, log=lambda message: None)
    result = SimpleNamespace(
        email="user@example.com",
        password="Secret123!",
        account_id="acct_123",
        access_token="at_123",
        refresh_token="",
        id_token="",
        session_token="sess_123",
        workspace_id="",
    )

    with pytest.raises(RuntimeError, match="完整 OAuth callback"):
        adapter.result_mapper(ctx, result)


def test_browser_register_run_rejects_session_fallback(monkeypatch):
    class FakePage:
        def __init__(self):
            self.url = "about:blank"
            self.context = SimpleNamespace(cookies=lambda: [])

        def goto(self, url, **kwargs):
            self.url = url

    class FakeBrowser:
        def __enter__(self):
            return self

        def __exit__(self, exc_type, exc, tb):
            return False

        def new_page(self):
            return FakePage()

    monkeypatch.setattr(browser_register_module, "Camoufox", lambda **kwargs: FakeBrowser())
    monkeypatch.setattr(browser_register_module, "_browser_registration_flow", lambda *args, **kwargs: {"page_type": "chatgpt_home"})
    monkeypatch.setattr(browser_register_module, "_click_first", lambda page, selectors, timeout=3: setattr(page, "url", "https://auth.openai.com/log-in") or selectors[0])
    monkeypatch.setattr(browser_register_module, "_get_cookies", lambda page: {})
    monkeypatch.setattr(browser_register_module, "_do_codex_oauth", lambda *args, **kwargs: None)
    monkeypatch.setattr(browser_register_module.ChatGPTBrowserRegister, "_retry_oauth_fresh_browser", lambda self, email, password: None)
    monkeypatch.setattr(browser_register_module.time, "sleep", lambda seconds: None)

    worker = browser_register_module.ChatGPTBrowserRegister(
        headless=True,
        proxy=None,
        otp_callback=None,
        log_fn=lambda message: None,
    )

    with pytest.raises(RuntimeError, match="已拒绝回退"):
        worker.run(email="user@example.com", password="Secret123!")
