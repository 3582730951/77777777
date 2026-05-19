from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from core.raw_cdp_recorder import (
    RawCdpFlowRecorder,
    RawCdpFlowRecorderConfig,
    _http_base_from_cdp_endpoint,
    resolve_cdp_websocket_url,
)


class _CdpVersionHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/json/version":
            body = json.dumps(
                {
                    "Browser": "Chrome/123.0.0.0",
                    "webSocketDebuggerUrl": f"ws://127.0.0.1:{self.server.server_port}/devtools/browser/test",
                }
            ).encode("utf-8")
        elif self.path == "/json/list":
            body = json.dumps(
                [
                    {
                        "type": "page",
                        "webSocketDebuggerUrl": f"ws://127.0.0.1:{self.server.server_port}/devtools/page/test",
                    }
                ]
            ).encode("utf-8")
        else:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        return


def test_resolve_cdp_websocket_url_prefers_page_target():
    server = ThreadingHTTPServer(("127.0.0.1", 0), _CdpVersionHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        resolved = resolve_cdp_websocket_url(f"http://127.0.0.1:{server.server_port}")
    finally:
        server.shutdown()
        thread.join(timeout=2)
        server.server_close()

    assert resolved == f"ws://127.0.0.1:{server.server_port}/devtools/page/test"


def test_resolve_cdp_websocket_url_converts_browser_ws_to_page_ws():
    server = ThreadingHTTPServer(("127.0.0.1", 0), _CdpVersionHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        resolved = resolve_cdp_websocket_url(f"ws://127.0.0.1:{server.server_port}/devtools/browser/test")
    finally:
        server.shutdown()
        thread.join(timeout=2)
        server.server_close()

    assert resolved == f"ws://127.0.0.1:{server.server_port}/devtools/page/test"


def test_raw_cdp_direct_page_session_translates_to_empty_protocol_session():
    recorder = RawCdpFlowRecorder(RawCdpFlowRecorderConfig())
    recorder._sessions["__direct_page__"] = {"direct": True, "url": "https://example.test/"}

    assert recorder._protocol_session_id("__direct_page__") == ""
    assert recorder._logical_session_id("") == "__direct_page__"


def test_raw_cdp_cookie_record_redacts_when_configured():
    recorder = RawCdpFlowRecorder(RawCdpFlowRecorderConfig(include_cookie_values=False))

    result = recorder._cookie_record(
        {
            "name": "sid",
            "value": "abcdef1234567890",
            "domain": "example.test",
            "path": "/",
            "httpOnly": True,
            "secure": True,
            "sameSite": "Lax",
        }
    )

    assert result["name"] == "sid"
    assert result["domain"] == "example.test"
    assert result["value_len"] == 16
    assert result["value_preview"] == "abcdef...7890"
    assert "value" not in result


def test_raw_cdp_defaults_to_low_impact_capture():
    config = RawCdpFlowRecorderConfig()

    assert config.include_response_bodies is False
    assert config.capture_all_pages is False
    assert config.include_form_snapshots is True
    assert config.include_sensitive_form_values is True
    assert config.screenshot_mode == "milestones"
    assert config.cookie_snapshot_interval_seconds == 2.0
    assert config.form_snapshot_interval_seconds == 1.0


def test_raw_cdp_form_field_values_are_plaintext_by_default():
    recorder = RawCdpFlowRecorder(RawCdpFlowRecorderConfig())

    result = recorder._form_field_record(
        {
            "index": 0,
            "selectorKey": 'input[name="password"]',
            "tag": "input",
            "type": "password",
            "name": "password",
            "value": "correct-horse",
        }
    )

    assert result["sensitive"] is True
    assert result["value"] == "correct-horse"
    assert result["value_len"] == len("correct-horse")


def test_raw_cdp_form_field_values_can_be_redacted():
    recorder = RawCdpFlowRecorder(RawCdpFlowRecorderConfig(include_sensitive_form_values=False))

    result = recorder._form_field_record(
        {
            "index": 0,
            "selectorKey": 'input[name="cvv"]',
            "tag": "input",
            "type": "text",
            "name": "cvv",
            "value": "123",
        }
    )

    assert result["sensitive"] is True
    assert result["value_redacted"] is True
    assert result["value_len"] == 3
    assert "value" not in result


def test_raw_cdp_snapshot_forms_records_main_and_child_frame_values(monkeypatch):
    recorder = RawCdpFlowRecorder(RawCdpFlowRecorderConfig())
    recorder._sessions["page:target-1"] = {
        "direct": True,
        "url": "https://example.test/",
    }
    recorder._execution_contexts["page:target-1"] = {
        "main-frame": 101,
        "captcha-frame": 202,
    }

    def fake_send(method, params=None, session_id="", timeout=10):
        params = params or {}
        if method == "Page.getFrameTree":
            return {
                "frameTree": {
                    "frame": {"id": "main-frame", "url": "https://example.test/checkout"},
                    "childFrames": [
                        {
                            "frame": {
                                "id": "captcha-frame",
                                "parentId": "main-frame",
                                "url": "https://verify.example.test/challenge",
                                "name": "challenge",
                            }
                        }
                    ],
                }
            }
        if method == "Runtime.evaluate" and params.get("contextId") == 101:
            return {
                "result": {
                    "value": {
                        "url": "https://example.test/checkout",
                        "title": "Checkout",
                        "fields": [
                            {
                                "index": 0,
                                "selectorKey": 'input[name="email"]',
                                "tag": "input",
                                "type": "email",
                                "name": "email",
                                "value": "person@example.test",
                            }
                        ],
                        "events": [
                            {
                                "ts": 1770000000000,
                                "eventType": "input",
                                "url": "https://example.test/checkout",
                                "title": "Checkout",
                                "field": {
                                    "index": 0,
                                    "selectorKey": 'input[name="email"]',
                                    "tag": "input",
                                    "type": "email",
                                    "name": "email",
                                    "value": "person@example.test",
                                },
                            }
                        ],
                    }
                }
            }
        if method == "Runtime.evaluate" and params.get("contextId") == 202:
            return {
                "result": {
                    "value": {
                        "url": "https://verify.example.test/challenge",
                        "title": "Challenge",
                        "fields": [
                            {
                                "index": 0,
                                "selectorKey": 'input[name="otp"]',
                                "tag": "input",
                                "type": "text",
                                "name": "otp",
                                "value": "654321",
                            }
                        ],
                        "events": [],
                    }
                }
            }
        return {}

    monkeypatch.setattr(recorder, "_send", fake_send)

    recorder._snapshot_forms("page:target-1", trigger="manual")

    assert len(recorder.form_snapshots) == 2
    assert recorder.form_snapshots[0]["fields"][0]["value"] == "person@example.test"
    assert recorder.form_snapshots[1]["frame"]["id"] == "captcha-frame"
    assert recorder.form_snapshots[1]["fields"][0]["value"] == "654321"
    assert any(event["event"] == "dom_input" for event in recorder.form_events)


def test_http_base_from_cdp_endpoint_accepts_browser_ws_and_http():
    assert _http_base_from_cdp_endpoint("ws://127.0.0.1:9222/devtools/browser/x") == "http://127.0.0.1:9222"
    assert _http_base_from_cdp_endpoint("http://127.0.0.1:9222/json/version") == "http://127.0.0.1:9222"
    assert _http_base_from_cdp_endpoint("127.0.0.1:9222") == "http://127.0.0.1:9222"


def test_raw_cdp_skips_cookie_diff_when_cookie_read_fails(monkeypatch):
    recorder = RawCdpFlowRecorder(RawCdpFlowRecorderConfig())
    recorder._cookie_state = {
        ("sid", "example.test", "/"): {
            "name": "sid",
            "domain": "example.test",
            "path": "/",
            "value_sha256": "abc",
        }
    }

    monkeypatch.setattr(recorder, "_all_cookies", lambda session_id: [])
    recorder._last_cookie_read_ok = False

    recorder._snapshot_cookies("__direct_page__", trigger="before_close", page_url="https://example.test/")

    assert recorder.cookie_events == []
    assert recorder.cookie_snapshots == []


def test_raw_cdp_attach_current_page_targets_connects_each_page(monkeypatch):
    targets = [
        {
            "id": "target-1",
            "type": "page",
            "url": "https://chatgpt.com/",
            "title": "ChatGPT",
            "webSocketDebuggerUrl": "ws://127.0.0.1:9222/devtools/page/target-1",
        },
        {
            "id": "target-2",
            "type": "page",
            "url": "https://www.paypal.com/checkoutnow",
            "title": "PayPal",
            "webSocketDebuggerUrl": "ws://127.0.0.1:9222/devtools/page/target-2",
        },
    ]
    connected = []

    class DummyCdp:
        def __init__(self, url):
            self.url = url

        def connect(self):
            connected.append(self.url)

        def close(self):
            return None

        def send_command(self, method, params=None, session_id=""):
            return 1

        def recv_json(self, timeout=0.25):
            return {"id": 1, "result": {}}

    recorder = RawCdpFlowRecorder(
        RawCdpFlowRecorderConfig(
            capture_all_pages=True,
            target_poll_interval_seconds=0,
        )
    )
    recorder._http_base = "http://127.0.0.1:9222"

    monkeypatch.setattr("core.raw_cdp_recorder._get_json", lambda url, timeout=5: targets)
    monkeypatch.setattr("core.raw_cdp_recorder.CdpWebSocket", DummyCdp)

    recorder._attach_current_page_targets(force=True)

    assert connected == [
        "ws://127.0.0.1:9222/devtools/page/target-1",
        "ws://127.0.0.1:9222/devtools/page/target-2",
    ]
    assert recorder._sessions["page:target-1"]["url"] == "https://chatgpt.com/"
    assert recorder._sessions["page:target-2"]["url"] == "https://www.paypal.com/checkoutnow"


def test_raw_cdp_navigation_timeout_is_recorded_as_warning(monkeypatch, tmp_path):
    recorder = RawCdpFlowRecorder(
        RawCdpFlowRecorderConfig(
            start_url="https://example.test/",
            cdp_url="ws://127.0.0.1/devtools/page/test",
            output_dir=str(tmp_path),
            duration_seconds=0,
            include_screenshots=False,
            include_response_bodies=False,
        )
    )

    class DummyCdp:
        def connect(self):
            return None

        def close(self):
            return None

    calls = []

    def fake_send(method, params=None, session_id="", timeout=10):
        calls.append((method, params, session_id, timeout))
        if method == "Page.navigate":
            raise TimeoutError("CDP command timed out: Page.navigate")
        return {}

    monkeypatch.setattr("core.raw_cdp_recorder.resolve_cdp_websocket_url", lambda value: value)
    monkeypatch.setattr("core.raw_cdp_recorder.CdpWebSocket", lambda url: DummyCdp())
    monkeypatch.setattr(recorder, "_initialize_targets", lambda: recorder._sessions.update({"__direct_page__": {"direct": True, "url": ""}}))
    monkeypatch.setattr(recorder, "_send", fake_send)
    monkeypatch.setattr(recorder, "_all_cookies", lambda session_id: [])
    monkeypatch.setattr(recorder, "_record_until_finished", lambda: None)

    recording = recorder.run()

    assert any(call[0] == "Page.navigate" for call in calls)
    assert recording.data["events"][0]["type"] == "recorder_warning"
    assert recording.data["events"][0]["method"] == "Page.navigate"


def test_raw_cdp_connection_closed_still_writes_recording(monkeypatch, tmp_path):
    recorder = RawCdpFlowRecorder(
        RawCdpFlowRecorderConfig(
            cdp_url="ws://127.0.0.1/devtools/page/test",
            output_dir=str(tmp_path),
            duration_seconds=0,
            include_screenshots=False,
        )
    )

    class DummyCdp:
        def connect(self):
            return None

        def close(self):
            return None

    def fake_recv(timeout=0.25):
        raise ConnectionError("websocket closed")

    monkeypatch.setattr("core.raw_cdp_recorder.resolve_cdp_websocket_url", lambda value: value)
    monkeypatch.setattr("core.raw_cdp_recorder.CdpWebSocket", lambda url: DummyCdp())
    monkeypatch.setattr(recorder, "_initialize_targets", lambda: recorder._sessions.update({"__direct_page__": {"direct": True, "url": ""}}))
    monkeypatch.setattr(recorder, "_send", lambda *args, **kwargs: {})
    monkeypatch.setattr(recorder, "_recv", fake_recv)
    monkeypatch.setattr(recorder, "_all_cookies", lambda session_id: [])

    recording = recorder.run()

    assert recording.data["events"][0]["type"] == "recorder_warning"
    assert recording.data["events"][0]["method"] == "websocket.recv"
    assert recorder._connection_closed is True
