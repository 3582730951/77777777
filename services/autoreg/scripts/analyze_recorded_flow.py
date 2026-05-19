#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import sys
from collections import defaultdict
from pathlib import Path
from urllib.parse import urlparse


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from core.browser_flow_recorder import analyze_cookie_timeline, build_protocol_plan, summarize_recording_screenshots  # noqa: E402


def _domain(url: str) -> str:
    try:
        return urlparse(str(url or "")).netloc
    except Exception:
        return ""


def analyze(recording: dict, *, probe: bool = False) -> dict:
    events_by_domain = defaultdict(lambda: {"requests": 0, "responses": 0, "statuses": {}, "methods": {}, "urls": set()})
    response_shapes = defaultdict(list)
    for event in recording.get("events", []) or []:
        domain = _domain(event.get("url") or event.get("request_url") or event.get("page_url") or event.get("frame_url"))
        if not domain:
            continue
        item = events_by_domain[domain]
        event_type = event.get("type")
        if event_type == "request":
            item["requests"] += 1
            method = str(event.get("method") or "GET")
            item["methods"][method] = item["methods"].get(method, 0) + 1
            item["urls"].add(event.get("url") or "")
        elif event_type == "response":
            item["responses"] += 1
            status = str(event.get("status") or "")
            item["statuses"][status] = item["statuses"].get(status, 0) + 1
            if event.get("json_shape") is not None or event.get("body_preview"):
                response_shapes[domain].append(
                    {
                        "url": event.get("url"),
                        "status": event.get("status"),
                        "content_type": event.get("content_type"),
                        "json_shape": event.get("json_shape"),
                        "body_len": event.get("body_len"),
                    }
                )

    cookie_analysis = analyze_cookie_timeline(recording.get("cookie_events") or [])
    screenshot_analysis = summarize_recording_screenshots(recording)
    protocol_plan = build_protocol_plan(recording)
    domain_summary = {
        domain: {
            "requests": stats["requests"],
            "responses": stats["responses"],
            "methods": stats["methods"],
            "statuses": stats["statuses"],
            "sample_urls": sorted(url for url in stats["urls"] if url)[:20],
            "response_shapes": response_shapes.get(domain, [])[:20],
        }
        for domain, stats in sorted(events_by_domain.items())
    }
    result = {
        "schema": "autoreg.recording_analysis.v1",
        "start_url": recording.get("start_url"),
        "domains": domain_summary,
        "cookie_analysis": cookie_analysis,
        "screenshot_analysis": screenshot_analysis,
        "protocol_steps": len(protocol_plan.get("steps") or []),
        "protocol_plan": protocol_plan,
    }
    if probe:
        result["domain_probes"] = _probe_domains(domain_summary)
    return result


def _probe_domains(domain_summary: dict) -> dict:
    import requests

    probes = {}
    for domain in domain_summary:
        for scheme in ("https", "http"):
            url = f"{scheme}://{domain}/"
            try:
                response = requests.get(url, timeout=15, allow_redirects=True)
                probes[domain] = {
                    "url": url,
                    "final_url": response.url,
                    "status": response.status_code,
                    "content_type": response.headers.get("content-type", ""),
                    "set_cookie": response.headers.get("set-cookie", "")[:500],
                }
                break
            except Exception as exc:
                probes[domain] = {"url": url, "error": str(exc)}
    return probes


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Analyze a browser recording by domain, cookie timeline, and protocol steps.")
    parser.add_argument("recording")
    parser.add_argument("--output", default="")
    parser.add_argument("--probe", action="store_true", help="Open/probe each visited domain homepage during secondary analysis")
    args = parser.parse_args(argv)

    path = Path(args.recording)
    recording = json.loads(path.read_text(encoding="utf-8"))
    result = analyze(recording, probe=args.probe)
    output = Path(args.output) if args.output else path.with_name(f"{path.stem}_analysis.json")
    output.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({
        "output": str(output),
        "domains": sorted(result.get("domains", {}).keys()),
        "cookie_events": result.get("cookie_analysis", {}).get("total_cookie_events", 0),
        "screenshots": result.get("screenshot_analysis", {}).get("count", 0),
        "protocol_steps": result.get("protocol_steps", 0),
    }, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
