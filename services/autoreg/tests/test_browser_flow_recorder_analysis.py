from __future__ import annotations

from types import SimpleNamespace

from core.browser_flow_recorder import analyze_cookie_timeline, summarize_recording_screenshots
from scripts.analyze_recorded_flow import analyze
from scripts import record_external_flow


def test_cookie_timeline_groups_cookie_changes_by_page_with_screenshots():
    cookie_events = [
        {
            "ts": "2026-05-09T15:00:00",
            "event": "created",
            "trigger": "response",
            "page_url": "https://example.test/signup",
            "request_url": "https://example.test/api/session",
            "cookie": {
                "name": "sid",
                "domain": "example.test",
                "path": "/",
                "httpOnly": True,
                "secure": True,
                "sameSite": "Lax",
            },
            "screenshot": "shots/00001_cookie_created_sid.png",
        },
        {
            "ts": "2026-05-09T15:00:02",
            "event": "updated",
            "trigger": "request",
            "page_url": "https://example.test/checkout",
            "request_url": "https://example.test/api/checkout",
            "cookie": {
                "name": "sid",
                "domain": "example.test",
                "path": "/",
                "httpOnly": True,
                "secure": True,
                "sameSite": "Lax",
            },
            "screenshot": "shots/00002_cookie_updated_sid.png",
        },
    ]

    result = analyze_cookie_timeline(cookie_events)

    assert result["total_cookie_events"] == 2
    assert result["domains"]["example.test"]["created"] == 1
    assert result["domains"]["example.test"]["updated"] == 1
    assert result["pages"]["https://example.test/signup"]["created"] == 1
    assert result["pages"]["https://example.test/checkout"]["updated"] == 1
    assert result["timeline"][0]["cookie_name"] == "sid"
    assert result["timeline"][0]["screenshot"] == "shots/00001_cookie_created_sid.png"


def test_screenshot_summary_collects_event_and_cookie_references():
    recording = {
        "screenshots": {
            "enabled": True,
            "dir": "shots",
            "count": 3,
            "files": [
                {"path": "shots/00001_request.png", "name": "00001_request.png", "bytes": 10},
                {"path": "shots/00002_response.png", "name": "00002_response.png", "bytes": 11},
                {"path": "shots/00003_cookie_created_sid.png", "name": "00003_cookie_created_sid.png", "bytes": 12},
            ],
        },
        "events": [
            {
                "ts": "2026-05-09T15:00:00",
                "type": "request",
                "page_url": "https://example.test/",
                "url": "https://example.test/",
                "screenshot": "shots/00001_request.png",
            },
            {
                "ts": "2026-05-09T15:00:01",
                "type": "response",
                "page_url": "https://example.test/",
                "url": "https://example.test/",
                "screenshot": "shots/00002_response.png",
            },
        ],
        "cookie_events": [
            {
                "ts": "2026-05-09T15:00:02",
                "event": "created",
                "page_url": "https://example.test/",
                "request_url": "https://example.test/",
                "screenshot": "shots/00003_cookie_created_sid.png",
            }
        ],
    }

    result = summarize_recording_screenshots(recording)

    assert result["count"] == 3
    assert result["referenced_count"] == 3
    assert result["by_source"] == {"events": 2, "cookie_events": 1}
    assert result["by_event_type"]["request"] == 1
    assert result["by_event_type"]["created"] == 1


def test_recording_analysis_includes_screenshots_and_page_cookie_timeline():
    recording = {
        "start_url": "https://example.test/",
        "screenshots": {"enabled": True, "dir": "shots", "count": 1, "files": []},
        "events": [
            {
                "ts": "2026-05-09T15:00:00",
                "type": "request",
                "method": "GET",
                "url": "https://example.test/",
                "resource_type": "document",
                "headers": {"accept": "text/html"},
                "screenshot": "shots/00001_request.png",
            },
            {
                "ts": "2026-05-09T15:00:01",
                "type": "response",
                "url": "https://example.test/",
                "status": 200,
                "resource_type": "document",
                "content_type": "text/html",
            },
        ],
        "cookie_events": [
            {
                "ts": "2026-05-09T15:00:02",
                "event": "created",
                "trigger": "response",
                "page_url": "https://example.test/",
                "request_url": "https://example.test/",
                "cookie": {"name": "sid", "domain": "example.test", "path": "/"},
                "screenshot": "shots/00002_cookie_created_sid.png",
            }
        ],
    }

    result = analyze(recording)

    assert result["screenshot_analysis"]["count"] == 2
    assert result["cookie_analysis"]["pages"]["https://example.test/"]["events"][0]["cookie_name"] == "sid"
    assert result["protocol_steps"] == 1


def test_record_external_flow_allows_cdp_attach_without_navigation(monkeypatch):
    captured = {}

    class DummyRecorder:
        def __init__(self, config):
            captured["config"] = config

        def run(self):
            return SimpleNamespace(
                path="data/recordings/test/browser_flow.json",
                data={
                    "config": {"include_screenshots": True},
                    "events": [],
                    "analysis": {"total_cookie_events": 0, "domains": {}},
                    "screenshots": {"count": 0, "dir": "data/recordings/test/screenshots"},
                },
            )

    monkeypatch.setattr(record_external_flow, "BrowserFlowRecorder", DummyRecorder)

    assert record_external_flow.main(
        [
            "--cdp-url",
            "http://127.0.0.1:54321",
            "--no-navigate",
            "--output-dir",
            "data/recordings/bitbrowser",
        ]
    ) == 0

    config = captured["config"]
    assert config.start_url == "about:blank"
    assert config.cdp_url == "http://127.0.0.1:54321"
    assert config.navigate is False
    assert config.output_dir == "data/recordings/bitbrowser"
