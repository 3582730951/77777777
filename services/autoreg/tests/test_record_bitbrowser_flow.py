from __future__ import annotations

from types import SimpleNamespace

from scripts import record_bitbrowser_flow
from scripts.record_bitbrowser_flow import (
    extract_cdp_url,
    normalize_bitbrowser_api_base,
    normalize_cdp_url,
    open_bitbrowser_profile,
    _is_retryable_open_response,
)


def test_normalize_bitbrowser_api_base():
    assert normalize_bitbrowser_api_base("") == "http://127.0.0.1:54345"
    assert normalize_bitbrowser_api_base("127.0.0.1:54345/") == "http://127.0.0.1:54345"
    assert normalize_bitbrowser_api_base("http://127.0.0.1:54345/") == "http://127.0.0.1:54345"


def test_normalize_cdp_url():
    assert normalize_cdp_url("127.0.0.1:53325") == "http://127.0.0.1:53325"
    assert normalize_cdp_url("ws://127.0.0.1:53325/devtools/browser/x") == "ws://127.0.0.1:53325/devtools/browser/x"


def test_extract_cdp_url_from_common_bitbrowser_shapes():
    assert extract_cdp_url({"data": {"ws": "ws://127.0.0.1:1/devtools/browser/a"}}) == "ws://127.0.0.1:1/devtools/browser/a"
    assert extract_cdp_url({"data": {"http": "127.0.0.1:53325"}}) == "http://127.0.0.1:53325"
    assert extract_cdp_url({"data": {"ws": {"selenium": "ws://127.0.0.1:2/devtools/browser/b"}}}) == (
        "ws://127.0.0.1:2/devtools/browser/b"
    )


def test_open_bitbrowser_profile_posts_id_args_and_extracts_endpoint(monkeypatch):
    captured = {}

    def fake_post_json(url, payload, timeout):
        captured["url"] = url
        captured["json"] = payload
        captured["timeout"] = timeout
        return {"success": True, "data": {"http": "127.0.0.1:53325"}}

    monkeypatch.setattr("scripts.record_bitbrowser_flow._post_json", fake_post_json)

    result = open_bitbrowser_profile(
        api_base="127.0.0.1:54345",
        browser_id="profile-1",
        launch_args=["--remote-debugging-port=9222"],
        queue=False,
        timeout=10,
        open_timeout=1,
        retry_interval=0.1,
    )

    assert captured["url"] == "http://127.0.0.1:54345/browser/open"
    assert captured["json"] == {
        "id": "profile-1",
        "queue": False,
        "args": ["--remote-debugging-port=9222"],
    }
    assert captured["timeout"] == 10
    assert result["_autoreg_cdp_url"] == "http://127.0.0.1:53325"
    assert result["_autoreg_open_attempts"] == 1


def test_open_bitbrowser_profile_retries_while_browser_is_opening(monkeypatch):
    responses = [
        {"success": False, "msg": "浏览器正在打开中"},
        {"success": True, "data": {"http": "127.0.0.1:53325"}},
    ]
    calls = []

    def fake_post_json(url, payload, timeout):
        calls.append((url, payload, timeout))
        return responses.pop(0)

    monkeypatch.setattr("scripts.record_bitbrowser_flow._post_json", fake_post_json)
    monkeypatch.setattr("scripts.record_bitbrowser_flow.time.sleep", lambda seconds: None)

    result = open_bitbrowser_profile(
        api_base="127.0.0.1:54345",
        browser_id="profile-1",
        open_timeout=3,
        retry_interval=0.1,
    )

    assert len(calls) == 2
    assert result["_autoreg_cdp_url"] == "http://127.0.0.1:53325"
    assert result["_autoreg_open_attempts"] == 2


def test_open_bitbrowser_profile_retries_api_timeout(monkeypatch):
    calls = []

    def fake_post_json(url, payload, timeout):
        calls.append((url, payload, timeout))
        if len(calls) == 1:
            raise TimeoutError("timed out")
        return {"success": True, "data": {"http": "127.0.0.1:53325"}}

    monkeypatch.setattr("scripts.record_bitbrowser_flow._post_json", fake_post_json)
    monkeypatch.setattr("scripts.record_bitbrowser_flow.time.sleep", lambda seconds: None)

    result = open_bitbrowser_profile(
        api_base="127.0.0.1:54345",
        browser_id="profile-1",
        timeout=2,
        open_timeout=3,
        retry_interval=0.1,
    )

    assert len(calls) == 2
    assert calls[0][2] == 2
    assert result["_autoreg_cdp_url"] == "http://127.0.0.1:53325"
    assert result["_autoreg_open_attempts"] == 2


def test_open_bitbrowser_retryable_response_detection():
    assert _is_retryable_open_response({"success": False, "msg": "浏览器正在打开中"})
    assert _is_retryable_open_response({"success": False, "msg": "opening"})
    assert not _is_retryable_open_response({"success": False, "msg": "id not found"})


def test_main_uses_raw_cdp_engine_by_default(monkeypatch):
    captured = {}

    def fake_open(**kwargs):
        captured["open"] = kwargs
        return {"_autoreg_cdp_url": "ws://127.0.0.1:9222/devtools/browser/test"}

    class DummyRecorder:
        def __init__(self, config):
            captured["config"] = config

        def run(self):
            return SimpleNamespace(
                path="data/recordings/bit_real/browser_flow.json",
                data={
                    "events": [],
                    "analysis": {"total_cookie_events": 0, "domains": {}},
                    "screenshots": {"count": 0, "dir": "data/recordings/bit_real/screenshots"},
                },
            )

    monkeypatch.setattr(record_bitbrowser_flow, "open_bitbrowser_profile", fake_open)
    monkeypatch.setattr(record_bitbrowser_flow, "RawCdpFlowRecorder", DummyRecorder)

    assert record_bitbrowser_flow.main(["--id", "profile-1", "--url", "https://example.test/"]) == 0

    assert captured["open"]["browser_id"] == "profile-1"
    assert captured["open"]["timeout"] == 10
    assert captured["open"]["open_timeout"] == 90
    assert captured["open"]["retry_interval"] == 1.5
    assert captured["config"].cdp_url == "ws://127.0.0.1:9222/devtools/browser/test"
    assert captured["config"].start_url == "https://example.test/"
    assert captured["config"].navigate is True
    assert captured["config"].include_response_bodies is False
    assert captured["config"].include_form_snapshots is True
    assert captured["config"].include_sensitive_form_values is True
    assert captured["config"].capture_all_pages is True
    assert captured["config"].screenshot_mode == "milestones"
    assert captured["config"].cookie_snapshot_interval_seconds == 2.0
    assert captured["config"].form_snapshot_interval_seconds == 1.0


def test_main_can_attach_without_cdp_navigation(monkeypatch):
    captured = {}

    def fake_open(**kwargs):
        return {"_autoreg_cdp_url": "ws://127.0.0.1:9222/devtools/page/test"}

    class DummyRecorder:
        def __init__(self, config):
            captured["config"] = config

        def run(self):
            return SimpleNamespace(
                path="data/recordings/bit_real/browser_flow.json",
                data={
                    "events": [],
                    "analysis": {"total_cookie_events": 0, "domains": {}},
                    "screenshots": {"count": 0, "dir": "data/recordings/bit_real/screenshots"},
                },
            )

    monkeypatch.setattr(record_bitbrowser_flow, "open_bitbrowser_profile", fake_open)
    monkeypatch.setattr(record_bitbrowser_flow, "RawCdpFlowRecorder", DummyRecorder)

    assert record_bitbrowser_flow.main(
        ["--id", "profile-1", "--url", "https://example.test/", "--no-navigate"]
    ) == 0

    assert captured["config"].start_url == "https://example.test/"
    assert captured["config"].navigate is False


def test_main_full_capture_enables_heavy_capture(monkeypatch):
    captured = {}

    def fake_open(**kwargs):
        return {"_autoreg_cdp_url": "ws://127.0.0.1:9222/devtools/page/test"}

    class DummyRecorder:
        def __init__(self, config):
            captured["config"] = config

        def run(self):
            return SimpleNamespace(
                path="data/recordings/bit_real/browser_flow.json",
                data={
                    "events": [],
                    "analysis": {"total_cookie_events": 0, "domains": {}},
                    "screenshots": {"count": 0, "dir": "data/recordings/bit_real/screenshots"},
                },
            )

    monkeypatch.setattr(record_bitbrowser_flow, "open_bitbrowser_profile", fake_open)
    monkeypatch.setattr(record_bitbrowser_flow, "RawCdpFlowRecorder", DummyRecorder)

    assert record_bitbrowser_flow.main(["--id", "profile-1", "--full-capture"]) == 0

    assert captured["config"].include_response_bodies is True
    assert captured["config"].screenshot_mode == "all"
    assert captured["config"].cookie_snapshot_interval_seconds == 0.0
    assert captured["config"].form_snapshot_interval_seconds == 0.0


def test_main_form_capture_flags(monkeypatch):
    captured = {}

    def fake_open(**kwargs):
        return {"_autoreg_cdp_url": "ws://127.0.0.1:9222/devtools/page/test"}

    class DummyRecorder:
        def __init__(self, config):
            captured["config"] = config

        def run(self):
            return SimpleNamespace(
                path="data/recordings/bit_real/browser_flow.json",
                data={
                    "events": [],
                    "form_events": [],
                    "form_snapshots": [],
                    "analysis": {"total_cookie_events": 0, "domains": {}},
                    "screenshots": {"count": 0, "dir": "data/recordings/bit_real/screenshots"},
                },
            )

    monkeypatch.setattr(record_bitbrowser_flow, "open_bitbrowser_profile", fake_open)
    monkeypatch.setattr(record_bitbrowser_flow, "RawCdpFlowRecorder", DummyRecorder)

    assert record_bitbrowser_flow.main(
        [
            "--id",
            "profile-1",
            "--no-form-capture",
            "--redact-form-values",
            "--form-interval",
            "0.25",
        ]
    ) == 0

    assert captured["config"].include_form_snapshots is False
    assert captured["config"].include_sensitive_form_values is False
    assert captured["config"].form_snapshot_interval_seconds == 0.25


def test_main_can_force_single_page_target_mode(monkeypatch):
    captured = {}

    def fake_open(**kwargs):
        return {"_autoreg_cdp_url": "ws://127.0.0.1:9222/devtools/page/test"}

    class DummyRecorder:
        def __init__(self, config):
            captured["config"] = config

        def run(self):
            return SimpleNamespace(
                path="data/recordings/bit_real/browser_flow.json",
                data={
                    "events": [],
                    "analysis": {"total_cookie_events": 0, "domains": {}},
                    "screenshots": {"count": 0, "dir": "data/recordings/bit_real/screenshots"},
                },
            )

    monkeypatch.setattr(record_bitbrowser_flow, "open_bitbrowser_profile", fake_open)
    monkeypatch.setattr(record_bitbrowser_flow, "RawCdpFlowRecorder", DummyRecorder)

    assert record_bitbrowser_flow.main(["--id", "profile-1", "--target-mode", "single-page"]) == 0

    assert captured["config"].capture_all_pages is False
