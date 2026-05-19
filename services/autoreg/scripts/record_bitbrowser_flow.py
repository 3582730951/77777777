#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from core.browser_flow_recorder import BrowserFlowRecorder, BrowserFlowRecorderConfig  # noqa: E402
from core.raw_cdp_recorder import RawCdpFlowRecorder, RawCdpFlowRecorderConfig  # noqa: E402


def normalize_bitbrowser_api_base(value: str) -> str:
    text = str(value or "").strip().rstrip("/")
    if not text:
        text = "http://127.0.0.1:54345"
    if "://" not in text:
        text = f"http://{text}"
    return text.rstrip("/")


def normalize_cdp_url(value: str) -> str:
    text = str(value or "").strip()
    if not text:
        return ""
    if text.startswith(("ws://", "wss://", "http://", "https://")):
        return text
    return f"http://{text}"


def extract_cdp_url(response_payload: dict[str, Any]) -> str:
    data = response_payload.get("data") if isinstance(response_payload, dict) else None
    candidates: list[Any] = []
    if isinstance(data, dict):
        candidates.extend(
            [
                _nested_get(data, ("ws", "puppeteer")),
                _nested_get(data, ("ws", "selenium")),
                _nested_get(data, ("ws", "playwright")),
                data.get("ws"),
                data.get("http"),
            ]
        )
    if isinstance(response_payload, dict):
        candidates.extend(
            [
                _nested_get(response_payload, ("ws", "puppeteer")),
                _nested_get(response_payload, ("ws", "selenium")),
                _nested_get(response_payload, ("ws", "playwright")),
                response_payload.get("ws"),
                response_payload.get("http"),
            ]
        )
    for candidate in candidates:
        if not isinstance(candidate, str):
            continue
        cdp_url = normalize_cdp_url(str(candidate or ""))
        if cdp_url:
            return cdp_url
    return ""


def _nested_get(value: dict[str, Any], keys: tuple[str, ...]) -> Any:
    current: Any = value
    for key in keys:
        if not isinstance(current, dict):
            return None
        current = current.get(key)
    return current


def open_bitbrowser_profile(
    *,
    api_base: str,
    browser_id: str,
    launch_args: list[str] | None = None,
    queue: bool = True,
    timeout: int = 60,
    open_timeout: float = 90,
    retry_interval: float = 1.5,
) -> dict[str, Any]:
    if not browser_id:
        raise ValueError("browser_id is required")
    payload: dict[str, Any] = {
        "id": browser_id,
        "queue": bool(queue),
    }
    if launch_args:
        payload["args"] = launch_args
    open_url = f"{normalize_bitbrowser_api_base(api_base)}/browser/open"
    deadline = time.time() + max(float(open_timeout or 0), 1.0)
    last_data: dict[str, Any] = {}
    last_error = ""
    attempt = 0
    while time.time() <= deadline:
        attempt += 1
        try:
            data = _post_json(open_url, payload, timeout=timeout)
        except (OSError, TimeoutError, urllib.error.URLError) as exc:
            last_error = str(exc)
            time.sleep(max(float(retry_interval or 0.5), 0.1))
            continue
        last_data = data
        cdp_url = extract_cdp_url(data)
        if cdp_url:
            data["_autoreg_cdp_url"] = cdp_url
            data["_autoreg_open_attempts"] = attempt
            return data
        if not _is_retryable_open_response(data):
            break
        time.sleep(max(float(retry_interval or 0.5), 0.1))
    detail = last_data if last_data else {"error": last_error or "no response"}
    raise RuntimeError(f"BitBrowser open response did not include a CDP endpoint after {attempt} attempt(s): {detail}")


def _is_retryable_open_response(data: dict[str, Any]) -> bool:
    msg = str(data.get("msg") or data.get("message") or data.get("error") or "").lower()
    if not msg:
        return bool(data.get("success") is False)
    retry_markers = (
        "正在打开",
        "打开中",
        "启动中",
        "队列",
        "排队",
        "opening",
        "starting",
        "pending",
        "queue",
        "busy",
    )
    return any(marker in msg for marker in retry_markers)


def _post_json(url: str, payload: dict[str, Any], *, timeout: int = 60) -> dict[str, Any]:
    request = urllib.request.Request(
        url,
        data=json.dumps(payload).encode("utf-8"),
        headers={
            "Content-Type": "application/json",
            "Accept": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        body = response.read().decode("utf-8", errors="replace")
    return json.loads(body)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Open a BitBrowser profile through local API and record it over CDP.")
    parser.add_argument("--api", default=os.getenv("BITBROWSER_API_BASE", "http://127.0.0.1:54345"))
    parser.add_argument("--id", required=True, help="BitBrowser profile/browser id")
    parser.add_argument("--url", default="", help="Optional URL to navigate after attaching")
    parser.add_argument("--no-navigate", action="store_true", help="Attach to the current tab without CDP navigation")
    parser.add_argument("--output-dir", default="data/recordings/bit_real")
    parser.add_argument("--arg", action="append", default=[], help="Extra BitBrowser launch arg, repeatable")
    parser.add_argument("--debug-port", type=int, default=0, help="Optional fixed remote-debugging-port")
    parser.add_argument("--engine", choices=("raw-cdp", "playwright"), default="raw-cdp")
    parser.add_argument("--target-mode", choices=("all-pages", "single-page"), default="all-pages")
    parser.add_argument("--no-queue", action="store_true")
    parser.add_argument("--api-timeout", type=int, default=10, help="Single BitBrowser local API request timeout seconds")
    parser.add_argument("--open-timeout", type=float, default=90, help="Seconds to wait for BitBrowser to return a CDP endpoint")
    parser.add_argument("--open-interval", type=float, default=1.5, help="Seconds between BitBrowser open retries")
    parser.add_argument("--duration", type=int, default=0, help="Seconds to record; 0 means wait for Enter")
    parser.add_argument("--redact-cookie-values", action="store_true")
    parser.add_argument("--no-form-capture", action="store_true", help="Disable DOM form snapshots and input/change event capture")
    parser.add_argument("--redact-form-values", action="store_true", help="Redact values from sensitive form fields")
    parser.add_argument("--form-interval", type=float, default=1.0, help="Seconds between form snapshots")
    parser.add_argument("--no-post-data", action="store_true")
    parser.add_argument("--response-bodies", action="store_true", help="Capture response body previews; heavier on active pages")
    parser.add_argument("--no-response-bodies", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--no-screenshots", action="store_true")
    parser.add_argument("--screenshot-mode", choices=("milestones", "all", "off"), default="milestones")
    parser.add_argument("--cookie-interval", type=float, default=2.0, help="Seconds between cookie snapshots for request/response events")
    parser.add_argument("--full-capture", action="store_true", help="Capture every event screenshot, response bodies, and unthrottled cookies")
    parser.add_argument("--viewport-only-screenshots", action="store_true")
    args = parser.parse_args(argv)

    launch_args = list(args.arg or [])
    if args.debug_port:
        launch_args.append(f"--remote-debugging-port={args.debug_port}")

    opened = open_bitbrowser_profile(
        api_base=args.api,
        browser_id=args.id,
        launch_args=launch_args,
        queue=not args.no_queue,
        timeout=args.api_timeout,
        open_timeout=args.open_timeout,
        retry_interval=args.open_interval,
    )
    cdp_url = opened["_autoreg_cdp_url"]
    start_url = args.url or "about:blank"
    navigate = bool(args.url) and not args.no_navigate
    screenshot_mode = "off" if args.no_screenshots else args.screenshot_mode
    include_response_bodies = bool(args.response_bodies) and not args.no_response_bodies
    cookie_interval = max(float(args.cookie_interval or 0), 0.0)
    form_interval = max(float(args.form_interval or 0), 0.0)
    if args.full_capture:
        screenshot_mode = "all" if not args.no_screenshots else "off"
        include_response_bodies = not args.no_response_bodies
        cookie_interval = 0.0
        form_interval = 0.0
    if args.engine == "playwright":
        recording = BrowserFlowRecorder(
            BrowserFlowRecorderConfig(
                start_url=start_url,
                output_dir=args.output_dir,
                cdp_url=cdp_url,
                navigate=navigate,
                duration_seconds=args.duration,
                include_cookie_values=not args.redact_cookie_values,
                include_post_data=not args.no_post_data,
                include_response_bodies=include_response_bodies,
                include_screenshots=not args.no_screenshots,
                screenshot_full_page=not args.viewport_only_screenshots,
                wait_message="比特浏览器操作完成后回到终端按 Enter 结束录制...",
            )
        ).run()
    else:
        recording = RawCdpFlowRecorder(
            RawCdpFlowRecorderConfig(
                start_url=start_url,
                output_dir=args.output_dir,
                cdp_url=cdp_url,
                navigate=navigate,
                duration_seconds=args.duration,
                include_cookie_values=not args.redact_cookie_values,
                include_form_snapshots=not args.no_form_capture,
                include_sensitive_form_values=not args.redact_form_values,
                include_post_data=not args.no_post_data,
                include_response_bodies=include_response_bodies,
                include_screenshots=not args.no_screenshots,
                capture_all_pages=args.target_mode == "all-pages",
                screenshot_mode=screenshot_mode,
                cookie_snapshot_interval_seconds=cookie_interval,
                form_snapshot_interval_seconds=form_interval,
                screenshot_full_page=not args.viewport_only_screenshots,
                wait_message="比特浏览器操作完成后回到终端按 Enter 结束录制...",
            )
        ).run()
    analysis = recording.data.get("analysis") or {}
    screenshots = recording.data.get("screenshots") or {}
    print(json.dumps({
        "bitbrowser_api": normalize_bitbrowser_api_base(args.api),
        "browser_id": args.id,
        "engine": args.engine,
        "cdp_url": cdp_url,
        "open_attempts": opened.get("_autoreg_open_attempts", 1),
        "recording": recording.path,
        "events": len(recording.data.get("events") or []),
        "form_events": len(recording.data.get("form_events") or []),
        "form_snapshots": len(recording.data.get("form_snapshots") or []),
        "cookie_events": analysis.get("total_cookie_events", 0),
        "cookie_domains": sorted((analysis.get("domains") or {}).keys()),
        "screenshot_count": screenshots.get("count", 0),
        "screenshot_dir": screenshots.get("dir", ""),
    }, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
