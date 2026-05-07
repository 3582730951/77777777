"""Unit tests for ChatGPT browser_register utility functions.

These are pure functions that don't need a browser or network.
"""
from __future__ import annotations

from platforms.chatgpt.browser_register import (
    _about_you_input_hints,
    _build_proxy_config,
    _pick_best_about_you_input,
    _extract_code_from_url,
    _infer_page_type,
    _extract_flow_state,
    _handle_add_phone_challenge,
    _normalize_url,
    _decode_jwt_payload,
    _derive_registration_state_from_page,
    _sync_hidden_birthday_input,
)


class TestBuildProxyConfig:
    def test_none_input(self):
        assert _build_proxy_config(None) is None

    def test_empty_string(self):
        assert _build_proxy_config("") is None

    def test_simple_proxy(self):
        result = _build_proxy_config("socks5://1.2.3.4:1080")
        assert result["server"] == "socks5://1.2.3.4:1080"

    def test_proxy_with_auth(self):
        result = _build_proxy_config("http://user:pass@1.2.3.4:8080")
        assert result["server"] == "http://1.2.3.4:8080"
        assert result["username"] == "user"
        assert result["password"] == "pass"

    def test_bare_proxy(self):
        result = _build_proxy_config("1.2.3.4:8080")
        assert result == {"server": "1.2.3.4:8080"}


class TestExtractCodeFromUrl:
    def test_no_code(self):
        assert _extract_code_from_url("https://example.com/callback") == ""

    def test_with_code(self):
        url = "https://example.com/callback?code=abc123&state=xyz"
        assert _extract_code_from_url(url) == "abc123"

    def test_empty_url(self):
        assert _extract_code_from_url("") == ""

    def test_code_only(self):
        url = "https://example.com?code=mycode"
        assert _extract_code_from_url(url) == "mycode"


class TestInferPageType:
    def test_from_data(self):
        data = {"page": {"type": "login-password"}}
        assert _infer_page_type(data) == "login_password"

    def test_from_url_email_verification(self):
        assert _infer_page_type(None, "https://auth.openai.com/email-verification") == "email_otp_verification"

    def test_from_url_about_you(self):
        assert _infer_page_type(None, "https://auth.openai.com/about-you") == "about_you"

    def test_from_url_chatgpt_home(self):
        assert _infer_page_type(None, "https://chatgpt.com/") == "chatgpt_home"

    def test_from_url_consent(self):
        assert _infer_page_type(None, "https://auth.openai.com/sign-in-with-chatgpt/codex/consent") == "consent"

    def test_empty(self):
        assert _infer_page_type(None, "") == ""

    def test_none_data(self):
        assert _infer_page_type(None) == ""


class TestExtractFlowState:
    def test_basic(self):
        data = {"page": {"type": "login-password"}, "continue_url": "/next"}
        state = _extract_flow_state(data, "https://auth.openai.com/login")
        assert state["page_type"] == "login_password"
        assert "auth.openai.com" in state["continue_url"]

    def test_none_data(self):
        state = _extract_flow_state(None, "https://auth.openai.com/about-you")
        assert state["page_type"] == "about_you"


class TestNormalizeUrl:
    def test_absolute_url(self):
        assert _normalize_url("https://example.com/path") == "https://example.com/path"

    def test_relative_url(self):
        result = _normalize_url("/api/next")
        assert result == "https://auth.openai.com/api/next"

    def test_empty(self):
        assert _normalize_url("") == ""


class TestDecodeJwtPayload:
    def test_valid_jwt(self):
        import base64 as _b64
        import json as _json

        payload_data = {"sub": "123"}
        payload = _b64.urlsafe_b64encode(
            _json.dumps(payload_data, separators=(",", ":")).encode()
        ).decode().rstrip("=")
        header = _b64.urlsafe_b64encode(
            _json.dumps({"alg": "HS256"}, separators=(",", ":")).encode()
        ).decode().rstrip("=")
        token = f"{header}.{payload}.signature"
        result = _decode_jwt_payload(token)
        assert result["sub"] == "123"

    def test_invalid_token(self):
        assert _decode_jwt_payload("not-a-jwt") == {}

    def test_empty(self):
        assert _decode_jwt_payload("") == {}


class TestAboutYouInputSelection:
    def test_hints_include_label_placeholder_name_and_parent_text(self):
        entry = {
            "labels": ["Full name"],
            "wrappedLabel": "",
            "labelledByText": "",
            "ariaLabel": "",
            "placeholder": "Enter your name",
            "name": "name",
            "id": "full-name",
            "parentText": "Profile",
        }
        hints = _about_you_input_hints(entry)
        assert "full name" in hints
        assert "enter your name" in hints
        assert "profile" in hints

    def test_pick_best_age_input_does_not_select_full_name_field(self):
        entries = [
            {
                "visibleIndex": 0,
                "labels": ["Full name"],
                "wrappedLabel": "",
                "labelledByText": "",
                "ariaLabel": "",
                "placeholder": "Full name",
                "name": "name",
                "id": "name",
                "parentText": "Full name",
            },
            {
                "visibleIndex": 1,
                "labels": ["Age"],
                "wrappedLabel": "",
                "labelledByText": "",
                "ariaLabel": "",
                "placeholder": "Age",
                "name": "age",
                "id": "age",
                "parentText": "Age",
            },
        ]

        name_entry = _pick_best_about_you_input(entries, "name")
        age_entry = _pick_best_about_you_input(entries, "age", exclude_visible_indices={0})

        assert name_entry["visibleIndex"] == 0
        assert age_entry["visibleIndex"] == 1


class TestHandleAddPhoneChallenge:
    def test_successful_phone_verification_navigates_to_resume_url(self, monkeypatch):
        import platforms.chatgpt.browser_register as mod

        class FakePage:
            def __init__(self):
                self.url = "https://auth.openai.com/add-phone"

            def goto(self, url, **kwargs):
                self.url = url

        page = FakePage()
        responses = iter(
            [
                {"ok": True, "status": 200, "url": page.url, "data": {}, "text": ""},
                {"ok": True, "status": 204, "url": page.url, "data": {}, "text": ""},
            ]
        )
        phone_values = iter(["+15551234567", "654321"])

        monkeypatch.setattr(mod, "_browser_pause", lambda page: None)
        monkeypatch.setattr(mod, "_browser_fetch", lambda *args, **kwargs: next(responses))

        state = _handle_add_phone_challenge(
            page,
            lambda: next(phone_values),
            device_id="did_123",
            user_agent="Mozilla/5.0",
            log=lambda message: None,
            resume_url="https://chatgpt.com/",
        )

        assert page.url == "https://chatgpt.com/"
        assert state["page_type"] == "chatgpt_home"


class TestDeriveRegistrationStateFromPage:
    def test_password_input_without_diagnostic_url_is_treated_as_registration_password(self):
        class FakePage:
            def __init__(self):
                self.url = "https://platform.openai.com/login"

            def query_selector(self, selector):
                if selector == 'input[type="password"]':
                    return object()
                return None

            def evaluate(self, script):
                return False

        state = _derive_registration_state_from_page(FakePage())
        assert state["page_type"] == "create_account_password"

    def test_login_password_url_stays_login_password(self):
        class FakePage:
            def __init__(self):
                self.url = "https://auth.openai.com/log-in/password"

            def query_selector(self, selector):
                if selector == 'input[type="password"]':
                    return object()
                return None

            def evaluate(self, script):
                return False

        state = _derive_registration_state_from_page(FakePage())
        assert state["page_type"] == "login_password"


class TestWaitForSignupEntryTransition:
    def test_clicks_passwordless_button_before_waiting_for_otp_state(self, monkeypatch):
        import platforms.chatgpt.browser_register as mod

        class FakePage:
            def __init__(self):
                self.url = "https://auth.openai.com/log-in-or-create-account"
                self.passwordless_clicked = False

        page = FakePage()
        now = {"value": 1000.0}

        def fake_sleep(seconds):
            now["value"] += float(seconds)

        def fake_time():
            return now["value"]

        def fake_click_passwordless(page_obj, log, context):
            if not page_obj.passwordless_clicked:
                page_obj.passwordless_clicked = True
                page_obj.url = "https://auth.openai.com/email-verification"
                return True
            return False

        def fake_state(page_obj):
            if page_obj.passwordless_clicked:
                return {"page_type": "email_otp_verification", "current_url": page_obj.url}
            return {"page_type": "", "current_url": page_obj.url}

        monkeypatch.setattr(mod.time, "sleep", fake_sleep)
        monkeypatch.setattr(mod.time, "time", fake_time)
        monkeypatch.setattr(mod, "_click_passwordless_login_if_available", fake_click_passwordless)
        monkeypatch.setattr(mod, "_derive_registration_state_from_page", fake_state)
        monkeypatch.setattr(mod, "_extract_auth_error_text", lambda page_obj: "")

        state = mod._wait_for_signup_entry_transition(page, lambda message: None, timeout=5)

        assert page.passwordless_clicked is True
        assert state["page_type"] == "email_otp_verification"


class TestSubmitPasswordViaPage:
    def test_does_not_retry_form_fallback_after_click_submit(self, monkeypatch):
        import platforms.chatgpt.browser_register as mod

        class FakePage:
            def __init__(self):
                self.url = "https://auth.openai.com/create-account/password"
                self.fallback_submitted = False

        page = FakePage()
        now = {"value": 1000.0}

        def fake_sleep(seconds):
            now["value"] += float(seconds)

        def fake_time():
            return now["value"]

        def fake_submit_form(page_obj, selector):
            page_obj.fallback_submitted = True
            page_obj.url = "https://auth.openai.com/email-verification"
            return True

        def fake_state(page_obj):
            if page_obj.fallback_submitted:
                return {"page_type": "email_otp_verification", "current_url": page_obj.url}
            return {"page_type": "create_account_password", "current_url": page_obj.url}

        monkeypatch.setattr(mod.time, "sleep", fake_sleep)
        monkeypatch.setattr(mod.time, "time", fake_time)
        monkeypatch.setattr(mod, "_recover_signup_password_page", lambda page_obj, log: False)
        monkeypatch.setattr(mod, "_wait_for_any_selector", lambda page_obj, selectors, timeout=15: 'input[type="password"]')
        monkeypatch.setattr(mod, "_fill_input_like_user", lambda page_obj, selector, password: True)
        monkeypatch.setattr(mod, "_click_first", lambda page_obj, selectors, timeout=8: selectors[0])
        monkeypatch.setattr(mod, "_derive_registration_state_from_page", fake_state)
        monkeypatch.setattr(mod, "_extract_auth_error_text", lambda page_obj: "")
        monkeypatch.setattr(mod, "_submit_form_with_fallback", fake_submit_form)
        monkeypatch.setattr(mod, "_dump_debug", lambda page_obj, prefix: None)

        result = mod._submit_password_via_page(page, "Secret123!", lambda message: None)

        assert result["ok"] is False
        assert result["status"] == 0
        assert page.fallback_submitted is False
        assert result["url"] == "https://auth.openai.com/create-account/password"

    def test_uses_form_fallback_when_submit_button_missing(self, monkeypatch):
        import platforms.chatgpt.browser_register as mod

        class FakePage:
            def __init__(self):
                self.url = "https://auth.openai.com/create-account/password"
                self.fallback_submitted = False

        page = FakePage()
        now = {"value": 1000.0}

        def fake_sleep(seconds):
            now["value"] += float(seconds)

        def fake_time():
            return now["value"]

        def fake_submit_form(page_obj, selector):
            page_obj.fallback_submitted = True
            page_obj.url = "https://auth.openai.com/email-verification"
            return True

        def fake_state(page_obj):
            if page_obj.fallback_submitted:
                return {"page_type": "email_otp_verification", "current_url": page_obj.url}
            return {"page_type": "create_account_password", "current_url": page_obj.url}

        monkeypatch.setattr(mod.time, "sleep", fake_sleep)
        monkeypatch.setattr(mod.time, "time", fake_time)
        monkeypatch.setattr(mod, "_recover_signup_password_page", lambda page_obj, log: False)
        monkeypatch.setattr(mod, "_wait_for_any_selector", lambda page_obj, selectors, timeout=15: 'input[type="password"]')
        monkeypatch.setattr(mod, "_fill_input_like_user", lambda page_obj, selector, password: True)
        monkeypatch.setattr(mod, "_click_first", lambda page_obj, selectors, timeout=8: None)
        monkeypatch.setattr(mod, "_derive_registration_state_from_page", fake_state)
        monkeypatch.setattr(mod, "_extract_auth_error_text", lambda page_obj: "")
        monkeypatch.setattr(mod, "_submit_form_with_fallback", fake_submit_form)
        monkeypatch.setattr(mod, "_dump_debug", lambda page_obj, prefix: None)

        result = mod._submit_password_via_page(page, "Secret123!", lambda message: None)

        assert result["ok"] is True
        assert result["status"] == 200
        assert page.fallback_submitted is True
        assert result["url"] == "https://auth.openai.com/email-verification"


class TestBrowserRegistrationFlow:
    def test_login_password_state_falls_back_to_existing_account_login_flow(self, monkeypatch):
        import platforms.chatgpt.browser_register as mod

        class FakePage:
            def __init__(self):
                self.url = "https://auth.openai.com/log-in/password"

            def evaluate(self, script):
                return "Mozilla/5.0"

        page = FakePage()
        states = iter(
            [
                {"page_type": "login_password", "current_url": page.url, "continue_url": "", "method": "GET"},
            ]
        )
        login_calls = {"count": 0}

        def fake_submit_login_password(page_obj, password, log):
            login_calls["count"] += 1
            page_obj.url = "https://chatgpt.com/"
            return {"ok": True, "status": 200, "url": page_obj.url, "data": None, "text": ""}

        def fake_extract_flow_state(data, current_url=""):
            if current_url == "https://chatgpt.com/":
                return {"page_type": "chatgpt_home", "current_url": current_url, "continue_url": "", "method": "GET"}
            return {"page_type": "", "current_url": current_url, "continue_url": "", "method": "GET"}

        monkeypatch.setattr(mod, "_seed_browser_device_id", lambda page_obj, device_id: None)
        monkeypatch.setattr(mod, "_start_browser_signup_via_page", lambda page_obj, email, log: next(states))
        monkeypatch.setattr(mod, "_get_cookies", lambda page_obj: {})
        monkeypatch.setattr(mod, "_is_registration_complete", lambda state: str(state.get("page_type")) == "chatgpt_home")
        monkeypatch.setattr(mod, "_is_password_registration", lambda state: False)
        monkeypatch.setattr(mod, "_recover_signup_password_page", lambda page_obj, log: False)
        monkeypatch.setattr(mod, "_submit_oauth_password_direct", fake_submit_login_password)
        monkeypatch.setattr(mod, "_extract_flow_state", fake_extract_flow_state)
        monkeypatch.setattr(mod, "_derive_registration_state_from_page", lambda page_obj: {"page_type": "chatgpt_home", "current_url": "https://chatgpt.com/", "continue_url": "", "method": "GET"})
        monkeypatch.setattr(mod, "_handle_post_signup_onboarding", lambda page_obj, log: None)

        state = mod._browser_registration_flow(page, "test@example.com", "Secret123!", None, None, lambda message: None)

        assert login_calls["count"] == 1
        assert state["page_type"] == "chatgpt_home"


class TestDoCodexOAuth:
    def test_prefers_phone_callback_when_oauth_hits_add_phone(self, monkeypatch):
        import platforms.chatgpt.browser_register as mod

        class FakeOAuthStart:
            auth_url = "https://auth.openai.com/oauth/authorize?state=test_state"
            state = "test_state"
            code_verifier = "verifier"

        class FakePage:
            def __init__(self):
                self.url = "https://auth.openai.com/add-phone"

            def evaluate(self, script):
                return "Mozilla/5.0"

            def goto(self, url, **kwargs):
                self.url = url

        page = FakePage()
        phone_calls = {"count": 0}

        monkeypatch.setattr("platforms.chatgpt.oauth.generate_oauth_url", lambda: FakeOAuthStart())
        monkeypatch.setattr(
            mod,
            "_derive_oauth_state_from_page",
            lambda page_obj: {"page_type": "oauth_callback" if "code=" in page_obj.url else "add_phone", "current_url": page_obj.url, "continue_url": "", "method": "GET"},
        )
        monkeypatch.setattr(mod, "_get_page_oauth_url", lambda page_obj: "")
        monkeypatch.setattr(mod, "_get_cookies", lambda page_obj: {})
        monkeypatch.setattr(mod, "_extract_code_from_url", lambda url: "abc" if "code=abc" in str(url) else "")
        monkeypatch.setattr(mod, "_complete_oauth_with_session", lambda *args, **kwargs: None)
        monkeypatch.setattr(mod, "_extract_callback_url_from_exception", lambda exc: "")
        monkeypatch.setattr(mod, "_extract_auth_error_text", lambda page_obj: "")
        monkeypatch.setattr(mod.time, "sleep", lambda seconds: None)

        def fake_handle_add_phone(page_obj, phone_callback, *, device_id, user_agent, log, resume_url=""):
            phone_calls["count"] += 1
            page_obj.url = "https://localhost/callback?code=abc&state=test_state"
            return {"page_type": "oauth_callback", "current_url": page_obj.url, "continue_url": "", "method": "GET"}

        monkeypatch.setattr(mod, "_handle_add_phone_challenge", fake_handle_add_phone)
        monkeypatch.setattr(mod, "_submit_callback_result", lambda callback_url, oauth_start, proxy: {"account_id": "acct_1", "access_token": "token"})

        result = mod._do_codex_oauth(
            page,
            cookies_dict={},
            email="test@example.com",
            password="Secret123!",
            otp_callback=None,
            phone_callback=lambda: "unused",
            proxy=None,
            log=lambda message: None,
        )

        assert phone_calls["count"] == 1
        assert result == {"account_id": "acct_1", "access_token": "token"}

    def test_stops_oauth_when_add_phone_sms_flow_fails(self, monkeypatch):
        import platforms.chatgpt.browser_register as mod

        class FakeOAuthStart:
            auth_url = "https://auth.openai.com/oauth/authorize?state=test_state"
            state = "test_state"
            code_verifier = "verifier"

        class FakePage:
            def __init__(self):
                self.url = "https://auth.openai.com/add-phone"

            def evaluate(self, script):
                return "Mozilla/5.0"

            def goto(self, url, **kwargs):
                self.url = url

        monkeypatch.setattr("platforms.chatgpt.oauth.generate_oauth_url", lambda: FakeOAuthStart())
        monkeypatch.setattr(mod, "_derive_oauth_state_from_page", lambda page_obj: {"page_type": "add_phone", "current_url": page_obj.url, "continue_url": "", "method": "GET"})
        monkeypatch.setattr(mod, "_get_page_oauth_url", lambda page_obj: "")
        monkeypatch.setattr(mod, "_extract_code_from_url", lambda url: "")
        monkeypatch.setattr(mod, "_extract_auth_error_text", lambda page_obj: "")
        monkeypatch.setattr(mod.time, "sleep", lambda seconds: None)
        monkeypatch.setattr(mod, "_handle_add_phone_challenge", lambda *args, **kwargs: (_ for _ in ()).throw(RuntimeError("sms down")))

        result = mod._do_codex_oauth(
            FakePage(),
            cookies_dict={},
            email="test@example.com",
            password="Secret123!",
            otp_callback=None,
            phone_callback=lambda: "unused",
            proxy=None,
            log=lambda message: None,
        )

        assert result is None


class TestSyncHiddenBirthdayInput:
    def test_sets_hidden_birthday_and_dispatches_events(self):
        class FakePage:
            def __init__(self):
                self.recorded_birthdate = ""

            def evaluate(self, script, value):
                self.recorded_birthdate = value
                return True

        page = FakePage()
        logs: list[str] = []

        result = _sync_hidden_birthday_input(page, "1994-08-17", logs.append)

        assert result is True
        assert page.recorded_birthdate == "1994-08-17"
        assert any("隐藏 birthday" in message for message in logs)
