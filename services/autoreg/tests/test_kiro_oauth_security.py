import pytest
import stat
from urllib.parse import parse_qs

from platforms.kiro.browser_oauth import (
    _extract_callback_authorization,
    _is_expected_oauth_callback_url,
)
from platforms.kiro.core import (
    KiroRegister,
    _append_private_jsonl,
    _safe_json_for_log,
    _safe_url_for_log,
    _single_query_value,
)
from platforms.kiro.desktop_oauth import _callback_result_from_path


def test_kiro_browser_oauth_accepts_only_expected_callback_state():
    url = "https://app.kiro.dev/signin/oauth?code=abc123&state=state-1"

    assert _extract_callback_authorization(url, "state-1") == ("abc123", "state-1")
    assert _is_expected_oauth_callback_url(url, "state-1") is True

    assert _is_expected_oauth_callback_url(
        "https://app.kiro.dev/signin/oauth?code=abc123",
        "state-1",
    ) is False
    assert _is_expected_oauth_callback_url(
        "https://app.kiro.dev/signin/oauth?code=abc123&state=wrong",
        "state-1",
    ) is False
    assert _is_expected_oauth_callback_url(
        "https://evil.example/signin/oauth?code=abc123&state=state-1",
        "state-1",
    ) is False


def test_kiro_browser_oauth_rejects_malformed_callback():
    with pytest.raises(RuntimeError, match="state"):
        _extract_callback_authorization(
            "https://app.kiro.dev/signin/oauth?code=abc123",
            "state-1",
        )

    with pytest.raises(RuntimeError, match="地址"):
        _extract_callback_authorization(
            "https://app.kiro.dev/other?code=abc123&state=state-1",
            "state-1",
        )

    with pytest.raises(RuntimeError, match="数量"):
        _extract_callback_authorization(
            "https://app.kiro.dev/signin/oauth?code=abc123&code=def456&state=state-1",
            "state-1",
        )


def test_kiro_desktop_oauth_callback_validates_path_code_and_state():
    assert _callback_result_from_path(
        "/oauth/callback?code=abc123&state=state-1",
        "state-1",
    ) == {"code": "abc123", "state": "state-1"}

    assert _callback_result_from_path(
        "/other?code=abc123&state=state-1",
        "state-1",
    )["error"] == "invalid_callback_path"
    assert _callback_result_from_path(
        "/oauth/callback?state=state-1",
        "state-1",
    )["error"] == "missing_code"
    assert _callback_result_from_path(
        "/oauth/callback?code=abc123",
        "state-1",
    )["error"] == "missing_state"
    assert _callback_result_from_path(
        "/oauth/callback?code=abc123&state=wrong",
        "state-1",
    )["error"] == "state_mismatch"
    assert _callback_result_from_path(
        "/oauth/callback?code=abc123&code=def456&state=state-1",
        "state-1",
    )["error"] == "invalid_code_count"
    assert _callback_result_from_path(
        "/oauth/callback?code=abc123&state=state-1&state=other",
        "state-1",
    )["error"] == "invalid_state_count"


def test_kiro_protocol_flow_rejects_missing_or_mismatched_state():
    reg = KiroRegister()
    reg.log = lambda message: None

    assert reg._validate_oauth_state(reg.state) is True
    assert reg._validate_oauth_state("") is False
    assert reg._validate_oauth_state("wrong-state") is False


def test_kiro_account_jsonl_export_uses_private_mode(tmp_path):
    out = tmp_path / "kiro_accounts.txt"

    _append_private_jsonl(str(out), {"email": "u@example.com", "refreshToken": "rt"})

    assert stat.S_IMODE(out.stat().st_mode) == 0o600
    assert "rt" in out.read_text()


def test_kiro_safe_url_for_log_strips_query_and_fragment():
    assert (
        _safe_url_for_log("https://app.kiro.dev/signin/oauth?code=abc&state=secret#frag")
        == "https://app.kiro.dev/signin/oauth?...#..."
    )


def test_kiro_safe_json_for_log_redacts_nested_secrets():
    logged = _safe_json_for_log(
        {
            "accessToken": "access-token-secret",
            "nested": {
                "refresh_token": "refresh-token-secret",
                "clientSecret": "client-secret-value",
            },
            "safe": "visible",
        }
    )

    assert "visible" in logged
    assert "access-token-secret" not in logged
    assert "refresh-token-secret" not in logged
    assert "client-secret-value" not in logged


def test_kiro_single_query_value_rejects_missing_or_duplicate_values():
    assert _single_query_value(parse_qs("state=one"), "state") == "one"
    assert _single_query_value(parse_qs(""), "state") is None
    assert _single_query_value(parse_qs("state=one&state=two"), "state") is None
