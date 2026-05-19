#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from core.browser_flow_recorder import BrowserFlowRecorder, BrowserFlowRecorderConfig  # noqa: E402


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Record a headed browser flow with cookie timeline.")
    parser.add_argument("url", nargs="?", default="", help="Start URL, for example https://chatgpt.com/")
    parser.add_argument("--output-dir", default="data/recordings")
    parser.add_argument("--headless", action="store_true")
    parser.add_argument("--proxy", default="")
    parser.add_argument("--user-data-dir", default="")
    parser.add_argument("--cdp-url", default="", help="Attach to an existing Chrome/BitBrowser CDP endpoint")
    parser.add_argument("--no-navigate", action="store_true", help="Attach and record the current tab without navigating")
    parser.add_argument("--duration", type=int, default=0, help="Seconds to record; 0 means wait for Enter")
    parser.add_argument("--redact-cookie-values", action="store_true", help="Store hashes/previews instead of full cookie values")
    parser.add_argument("--no-post-data", action="store_true")
    parser.add_argument("--no-response-bodies", action="store_true")
    parser.add_argument("--no-screenshots", action="store_true")
    parser.add_argument("--viewport-only-screenshots", action="store_true")
    args = parser.parse_args(argv)

    if not args.url and not args.cdp_url:
        parser.error("url is required unless --cdp-url is provided")
    start_url = args.url or "about:blank"
    navigate = bool(args.url) and not args.no_navigate

    recording = BrowserFlowRecorder(
        BrowserFlowRecorderConfig(
            start_url=start_url,
            output_dir=args.output_dir,
            headless=args.headless,
            proxy=args.proxy,
            user_data_dir=args.user_data_dir,
            cdp_url=args.cdp_url,
            navigate=navigate,
            duration_seconds=args.duration,
            include_cookie_values=not args.redact_cookie_values,
            include_post_data=not args.no_post_data,
            include_response_bodies=not args.no_response_bodies,
            include_screenshots=not args.no_screenshots,
            screenshot_full_page=not args.viewport_only_screenshots,
        )
    ).run()
    analysis = recording.data.get("analysis") or {}
    screenshots = recording.data.get("screenshots") or {}
    print(json.dumps({
        "recording": recording.path,
        "events": len(recording.data.get("events") or []),
        "cookie_events": analysis.get("total_cookie_events", 0),
        "cookie_domains": sorted((analysis.get("domains") or {}).keys()),
        "screenshots": recording.data.get("config", {}).get("include_screenshots"),
        "screenshot_count": screenshots.get("count", 0),
        "screenshot_dir": screenshots.get("dir", ""),
    }, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
