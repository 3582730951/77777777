from __future__ import annotations

import hashlib
import json
import os
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlparse


_SENSITIVE_HEADER_NAMES = {
    "authorization",
    "cookie",
    "set-cookie",
    "proxy-authorization",
}

_HOP_BY_HOP_HEADERS = {
    "accept-encoding",
    "connection",
    "content-length",
    "host",
    "proxy-authorization",
    "proxy-connection",
    "te",
    "trailer",
    "transfer-encoding",
    "upgrade",
}


@dataclass
class BrowserFlowRecorderConfig:
    start_url: str
    output_dir: str = "data/recordings"
    headless: bool = False
    proxy: str = ""
    user_data_dir: str = ""
    cdp_url: str = ""
    navigate: bool = True
    duration_seconds: int = 0
    include_cookie_values: bool = True
    include_post_data: bool = True
    include_response_bodies: bool = True
    include_headers: bool = True
    max_response_body_chars: int = 2000
    include_screenshots: bool = True
    screenshot_full_page: bool = True
    redact_terminal: bool = True
    wait_message: str = "操作完成后回到终端按 Enter 结束录制..."


@dataclass
class BrowserFlowRecording:
    path: str
    data: dict[str, Any]


class BrowserFlowRecorder:
    def __init__(self, config: BrowserFlowRecorderConfig):
        self.config = config
        self.started_at = time.time()
        self.events: list[dict[str, Any]] = []
        self.cookie_events: list[dict[str, Any]] = []
        self.cookie_snapshots: list[dict[str, Any]] = []
        self._cookie_state: dict[tuple[str, str, str], dict[str, Any]] = {}
        self._request_seq = 0
        self._attached_pages: set[int] = set()
        self._screenshot_seq = 0
        self._screenshot_dir: Path | None = None

    def run(self) -> BrowserFlowRecording:
        from playwright.sync_api import sync_playwright

        output_dir = Path(self.config.output_dir)
        output_dir.mkdir(parents=True, exist_ok=True)
        started_label = time.strftime("%Y%m%d_%H%M%S", time.localtime(self.started_at))
        output_path = output_dir / f"browser_flow_{started_label}.json"
        self._screenshot_dir = output_dir / f"browser_flow_{started_label}_screenshots"
        if self.config.include_screenshots:
            self._screenshot_dir.mkdir(parents=True, exist_ok=True)

        with sync_playwright() as p:
            browser = None
            browser_owned = False
            context_owned = False
            if self.config.cdp_url:
                browser = p.chromium.connect_over_cdp(self.config.cdp_url)
                contexts = list(browser.contexts)
                if contexts:
                    context = contexts[0]
                else:
                    context = browser.new_context()
                    context_owned = True
            else:
                launch_kwargs: dict[str, Any] = {
                    "headless": self.config.headless,
                }
                proxy_config = self._playwright_proxy(self.config.proxy)
                if proxy_config:
                    launch_kwargs["proxy"] = proxy_config

                if self.config.user_data_dir:
                    context = p.chromium.launch_persistent_context(
                        self.config.user_data_dir,
                        **launch_kwargs,
                    )
                    context_owned = True
                else:
                    browser = p.chromium.launch(**launch_kwargs)
                    browser_owned = True
                    context = browser.new_context()
                    context_owned = True

            try:
                context.on("page", lambda page: self._attach_page(context, page))
                for open_page in list(context.pages):
                    self._attach_page(context, open_page)
                page = self._active_page(context) or context.new_page()
                self._attach_page(context, page)
                initial_trigger = "cdp_context_attached" if self.config.cdp_url else "context_created"
                self._snapshot_cookies(context, trigger=initial_trigger, page_url=self.config.start_url)
                if self.config.navigate and self.config.start_url and self.config.start_url != "about:blank":
                    page.goto(self.config.start_url, wait_until="domcontentloaded", timeout=90_000)
                    self._capture_screenshot(page, reason="initial_navigation")
                    self._snapshot_cookies(context, trigger="initial_navigation", page_url=page.url)
                else:
                    self._capture_screenshot(page, reason="attached_initial")
                    self._snapshot_cookies(context, trigger="attached_initial", page_url=page.url)
                if self.config.duration_seconds > 0:
                    page.wait_for_timeout(self.config.duration_seconds * 1000)
                else:
                    input(self.config.wait_message)
                for open_page in context.pages:
                    try:
                        self._capture_screenshot(open_page, reason="before_close")
                        self._snapshot_cookies(context, trigger="before_close", page_url=open_page.url)
                    except Exception:
                        pass
                storage_state = context.storage_state()
            finally:
                if context_owned:
                    context.close()
                if browser_owned and browser is not None:
                    browser.close()

        data = {
            "schema": "autoreg.browser_flow_recording.v1",
            "started_at": self._ts(self.started_at),
            "finished_at": self._ts(time.time()),
            "start_url": self.config.start_url,
            "config": {
                "headless": self.config.headless,
                "proxy": self._mask_proxy(self.config.proxy),
                "cdp_url": self._mask_cdp_url(self.config.cdp_url),
                "navigate": self.config.navigate,
                "include_cookie_values": self.config.include_cookie_values,
                "include_post_data": self.config.include_post_data,
                "include_response_bodies": self.config.include_response_bodies,
                "include_headers": self.config.include_headers,
                "include_screenshots": self.config.include_screenshots,
            },
            "events": self.events,
            "cookie_events": self.cookie_events,
            "cookie_snapshots": self.cookie_snapshots,
            "screenshots": self._screenshot_manifest(),
            "final_storage_state": self._storage_state(storage_state),
            "analysis": analyze_cookie_timeline(self.cookie_events),
        }
        output_path.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
        return BrowserFlowRecording(path=str(output_path), data=data)

    def _attach_page(self, context, page) -> None:
        page_id = id(page)
        if page_id in self._attached_pages:
            return
        self._attached_pages.add(page_id)
        page.on("request", lambda request: self._on_request(context, page, request))
        page.on("response", lambda response: self._on_response(context, page, response))
        page.on("framenavigated", lambda frame: self._on_frame_navigated(context, page, frame))

    @staticmethod
    def _active_page(context) -> Any:
        pages = list(getattr(context, "pages", []) or [])
        if not pages:
            return None
        for page in reversed(pages):
            try:
                if not page.is_closed() and str(page.url or "") != "about:blank":
                    return page
            except Exception:
                continue
        return pages[-1]

    def _on_request(self, context, page, request) -> None:
        self._request_seq += 1
        entry = {
            "id": self._request_seq,
            "ts": self._ts(),
            "type": "request",
            "page_url": self._safe_page_url(page),
            "method": request.method,
            "url": request.url,
            "resource_type": request.resource_type,
            "headers": self._headers(request.headers) if self.config.include_headers else {},
        }
        if self.config.include_post_data:
            try:
                post_data = request.post_data
            except Exception:
                post_data = None
            if post_data not in (None, ""):
                entry["post_data"] = post_data
                entry["post_data_sha256"] = self._sha256(post_data)
        screenshot = self._capture_screenshot(page, reason="request")
        if screenshot:
            entry["screenshot"] = screenshot
        self.events.append(entry)
        self._snapshot_cookies(context, trigger="request", page_url=self._safe_page_url(page), request_url=request.url)

    def _on_response(self, context, page, response) -> None:
        headers = self._response_headers(response)
        set_cookie_headers = self._set_cookie_headers(response, headers)
        entry = {
            "ts": self._ts(),
            "type": "response",
            "page_url": self._safe_page_url(page),
            "url": response.url,
            "status": response.status,
            "resource_type": self._response_resource_type(response),
            "headers": headers if self.config.include_headers else {},
            "set_cookie_headers": set_cookie_headers,
        }
        if self.config.include_response_bodies:
            entry.update(self._response_body_summary(response, headers))
        screenshot = self._capture_screenshot(page, reason="response")
        if screenshot:
            entry["screenshot"] = screenshot
        self.events.append(entry)
        self._snapshot_cookies(
            context,
            trigger="response",
            page_url=self._safe_page_url(page),
            request_url=response.url,
            set_cookie_headers=set_cookie_headers,
        )

    def _on_frame_navigated(self, context, page, frame) -> None:
        try:
            page_url = frame.url
        except Exception:
            page_url = self._safe_page_url(page)
        self.events.append(
            {
                "ts": self._ts(),
                "type": "navigation",
                "page_url": self._safe_page_url(page),
                "frame_url": page_url,
                "screenshot": self._capture_screenshot(page, reason="navigation"),
            }
        )
        self._snapshot_cookies(context, trigger="navigation", page_url=page_url)

    def _snapshot_cookies(
        self,
        context,
        *,
        trigger: str,
        page_url: str = "",
        request_url: str = "",
        set_cookie_headers: list[str] | None = None,
    ) -> None:
        try:
            cookies = context.cookies()
        except Exception:
            return
        snapshot = {
            "ts": self._ts(),
            "trigger": trigger,
            "page_url": page_url,
            "request_url": request_url,
            "count": len(cookies),
        }
        self.cookie_snapshots.append(snapshot)

        new_state: dict[tuple[str, str, str], dict[str, Any]] = {}
        for cookie in cookies:
            record = self._cookie_record(cookie)
            key = (record["name"], record["domain"], record["path"])
            new_state[key] = record

        for key, record in new_state.items():
            previous = self._cookie_state.get(key)
            if previous is None:
                screenshot = self._capture_screenshot_for_url(context, page_url, reason=f"cookie_created_{record['name']}")
                self.cookie_events.append(
                    {
                        "ts": self._ts(),
                        "event": "created",
                        "trigger": trigger,
                        "page_url": page_url,
                        "request_url": request_url,
                        "cookie": record,
                        "set_cookie_headers": list(set_cookie_headers or []),
                        "screenshot": screenshot,
                    }
                )
            elif self._cookie_fingerprint(previous) != self._cookie_fingerprint(record):
                screenshot = self._capture_screenshot_for_url(context, page_url, reason=f"cookie_updated_{record['name']}")
                self.cookie_events.append(
                    {
                        "ts": self._ts(),
                        "event": "updated",
                        "trigger": trigger,
                        "page_url": page_url,
                        "request_url": request_url,
                        "cookie": record,
                        "previous": self._cookie_previous(previous),
                        "set_cookie_headers": list(set_cookie_headers or []),
                        "screenshot": screenshot,
                    }
                )

        for key, previous in self._cookie_state.items():
            if key not in new_state:
                screenshot = self._capture_screenshot_for_url(context, page_url, reason=f"cookie_deleted_{previous['name']}")
                self.cookie_events.append(
                    {
                        "ts": self._ts(),
                        "event": "deleted",
                        "trigger": trigger,
                        "page_url": page_url,
                        "request_url": request_url,
                        "cookie": self._cookie_previous(previous),
                        "screenshot": screenshot,
                    }
                )

        self._cookie_state = new_state

    def _cookie_record(self, cookie: dict[str, Any]) -> dict[str, Any]:
        value = str(cookie.get("value") or "")
        record = {
            "name": str(cookie.get("name") or ""),
            "domain": str(cookie.get("domain") or ""),
            "path": str(cookie.get("path") or "/"),
            "expires": cookie.get("expires"),
            "httpOnly": bool(cookie.get("httpOnly")),
            "secure": bool(cookie.get("secure")),
            "sameSite": str(cookie.get("sameSite") or ""),
            "value_sha256": self._sha256(value),
            "value_len": len(value),
        }
        if self.config.include_cookie_values:
            record["value"] = value
        else:
            record["value_preview"] = self._preview(value)
        return record

    def _storage_state(self, storage_state: dict[str, Any]) -> dict[str, Any]:
        if self.config.include_cookie_values:
            return storage_state
        cloned = json.loads(json.dumps(storage_state or {}))
        for cookie in cloned.get("cookies", []) or []:
            value = str(cookie.get("value") or "")
            cookie["value_sha256"] = self._sha256(value)
            cookie["value_len"] = len(value)
            cookie["value"] = self._preview(value)
        return cloned

    @staticmethod
    def _cookie_fingerprint(cookie: dict[str, Any]) -> tuple[Any, ...]:
        return (
            cookie.get("value_sha256"),
            cookie.get("expires"),
            cookie.get("httpOnly"),
            cookie.get("secure"),
            cookie.get("sameSite"),
        )

    @staticmethod
    def _cookie_previous(cookie: dict[str, Any]) -> dict[str, Any]:
        return {
            key: value
            for key, value in cookie.items()
            if key != "value"
        }

    def _headers(self, headers: dict[str, str]) -> dict[str, str]:
        result: dict[str, str] = {}
        for key, value in dict(headers or {}).items():
            lower = key.lower()
            if lower in _SENSITIVE_HEADER_NAMES and not self.config.include_cookie_values:
                result[key] = self._preview(value)
            else:
                result[key] = value
        return result

    def _response_headers(self, response) -> dict[str, str]:
        try:
            headers = response.all_headers()
        except Exception:
            try:
                headers = response.headers
            except Exception:
                headers = {}
        return self._headers(headers)

    def _set_cookie_headers(self, response, headers: dict[str, str]) -> list[str]:
        values: list[str] = []
        try:
            for item in response.headers_array():
                if str(item.get("name") or "").lower() == "set-cookie":
                    values.append(str(item.get("value") or ""))
        except Exception:
            pass
        if not values:
            for key, value in headers.items():
                if key.lower() == "set-cookie":
                    values.append(value)
        return values

    def _capture_screenshot_for_url(self, context, page_url: str, *, reason: str) -> str:
        if not self.config.include_screenshots:
            return ""
        target = None
        for page in getattr(context, "pages", []) or []:
            if self._safe_page_url(page) == page_url:
                target = page
                break
        if target is None:
            pages = list(getattr(context, "pages", []) or [])
            target = pages[-1] if pages else None
        if target is None:
            return ""
        return self._capture_screenshot(target, reason=reason)

    def _capture_screenshot(self, page, *, reason: str) -> str:
        if not self.config.include_screenshots or self._screenshot_dir is None:
            return ""
        try:
            self._screenshot_seq += 1
            safe_reason = "".join(ch if ch.isalnum() or ch in {"_", "-"} else "_" for ch in str(reason or "event"))[:80]
            path = self._screenshot_dir / f"{self._screenshot_seq:05d}_{safe_reason}.png"
            page.screenshot(path=str(path), full_page=self.config.screenshot_full_page)
            return str(path)
        except Exception:
            return ""

    def _screenshot_manifest(self) -> dict[str, Any]:
        manifest = {
            "enabled": self.config.include_screenshots,
            "dir": str(self._screenshot_dir or ""),
            "count": 0,
            "files": [],
        }
        if not self.config.include_screenshots or self._screenshot_dir is None:
            return manifest
        files = []
        for path in sorted(self._screenshot_dir.glob("*.png")):
            item = {
                "path": str(path),
                "name": path.name,
            }
            try:
                item["bytes"] = path.stat().st_size
            except OSError:
                item["bytes"] = 0
            files.append(item)
        manifest["count"] = len(files)
        manifest["files"] = files
        return manifest

    @staticmethod
    def _response_resource_type(response) -> str:
        try:
            return str(response.request.resource_type or "")
        except Exception:
            return ""

    def _response_body_summary(self, response, headers: dict[str, str]) -> dict[str, Any]:
        content_type = ""
        for key, value in headers.items():
            if key.lower() == "content-type":
                content_type = str(value or "")
                break
        lower_type = content_type.lower()
        if not any(marker in lower_type for marker in ("json", "text", "html", "javascript", "x-www-form-urlencoded")):
            return {"content_type": content_type}
        try:
            text = response.text()
        except Exception as exc:
            return {"content_type": content_type, "body_error": str(exc)}
        summary = {
            "content_type": content_type,
            "body_sha256": self._sha256(text),
            "body_len": len(text),
            "body_preview": text[: self.config.max_response_body_chars],
        }
        if "json" in lower_type:
            try:
                summary["json_shape"] = self._json_shape(json.loads(text))
            except Exception:
                pass
        return summary

    def _json_shape(self, value: Any, depth: int = 0) -> Any:
        if depth >= 3:
            return type(value).__name__
        if isinstance(value, dict):
            return {
                str(key): self._json_shape(item, depth + 1)
                for key, item in list(value.items())[:50]
            }
        if isinstance(value, list):
            if not value:
                return []
            return [self._json_shape(value[0], depth + 1)]
        return type(value).__name__

    @staticmethod
    def _playwright_proxy(proxy: str) -> dict[str, str] | None:
        text = str(proxy or "").strip()
        if not text:
            return None
        parsed = urlparse(text)
        if not parsed.scheme or not parsed.hostname:
            return {"server": text}
        server = f"{parsed.scheme}://{parsed.hostname}"
        if parsed.port:
            server = f"{server}:{parsed.port}"
        config = {"server": server}
        if parsed.username:
            config["username"] = parsed.username
        if parsed.password:
            config["password"] = parsed.password
        return config

    @staticmethod
    def _sha256(value: Any) -> str:
        return hashlib.sha256(str(value or "").encode("utf-8", errors="replace")).hexdigest()

    @staticmethod
    def _preview(value: Any) -> str:
        text = str(value or "")
        if not text:
            return ""
        if len(text) <= 12:
            return "***"
        return f"{text[:6]}...{text[-4:]}"

    @staticmethod
    def _safe_page_url(page) -> str:
        try:
            return str(page.url or "")
        except Exception:
            return ""

    @staticmethod
    def _mask_proxy(proxy: str) -> str:
        text = str(proxy or "")
        if "@" not in text:
            return text
        prefix, _, host = text.rpartition("@")
        scheme, _, _auth = prefix.partition("://")
        return f"{scheme}://***@{host}" if scheme else f"***@{host}"

    @staticmethod
    def _mask_cdp_url(cdp_url: str) -> str:
        text = str(cdp_url or "")
        if not text:
            return ""
        parsed = urlparse(text)
        if parsed.query:
            return text.split("?", 1)[0] + "?***"
        return text

    @staticmethod
    def _ts(value: float | None = None) -> str:
        return time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(value or time.time()))


def analyze_cookie_timeline(cookie_events: list[dict[str, Any]]) -> dict[str, Any]:
    by_domain: dict[str, dict[str, Any]] = {}
    by_cookie: dict[str, dict[str, Any]] = {}
    by_page: dict[str, dict[str, Any]] = {}
    timeline: list[dict[str, Any]] = []
    for event in cookie_events or []:
        cookie = dict(event.get("cookie") or {})
        domain = str(cookie.get("domain") or "")
        name = str(cookie.get("name") or "")
        page_url = str(event.get("page_url") or "")
        action = str(event.get("event") or "")
        key = f"{domain}|{cookie.get('path') or '/'}|{name}"
        domain_stats = by_domain.setdefault(domain, {"created": 0, "updated": 0, "deleted": 0, "cookies": set()})
        if action in {"created", "updated", "deleted"}:
            domain_stats[action] += 1
        domain_stats["cookies"].add(name)
        cookie_stats = by_cookie.setdefault(
            key,
            {
                "name": name,
                "domain": domain,
                "path": cookie.get("path") or "/",
                "created": 0,
                "updated": 0,
                "deleted": 0,
                "first_seen": event.get("ts"),
                "last_seen": event.get("ts"),
                "httpOnly": cookie.get("httpOnly"),
                "secure": cookie.get("secure"),
                "sameSite": cookie.get("sameSite"),
            },
        )
        if action in {"created", "updated", "deleted"}:
            cookie_stats[action] += 1
        cookie_stats["last_seen"] = event.get("ts")
        timeline_item = {
            "ts": event.get("ts"),
            "event": action,
            "trigger": event.get("trigger"),
            "page_url": page_url,
            "request_url": event.get("request_url") or "",
            "cookie_name": name,
            "domain": domain,
            "path": cookie.get("path") or "/",
            "httpOnly": cookie.get("httpOnly"),
            "secure": cookie.get("secure"),
            "sameSite": cookie.get("sameSite"),
            "screenshot": event.get("screenshot") or "",
        }
        timeline.append(timeline_item)
        page_stats = by_page.setdefault(page_url, {"created": 0, "updated": 0, "deleted": 0, "events": []})
        if action in {"created", "updated", "deleted"}:
            page_stats[action] += 1
        page_stats["events"].append(timeline_item)

    serializable_domains = {
        domain: {
            **stats,
            "cookies": sorted(stats["cookies"]),
        }
        for domain, stats in by_domain.items()
    }
    return {
        "total_cookie_events": len(cookie_events or []),
        "domains": serializable_domains,
        "cookies": list(by_cookie.values()),
        "pages": by_page,
        "timeline": timeline,
    }


def summarize_recording_screenshots(recording: dict[str, Any]) -> dict[str, Any]:
    seen: set[str] = set()
    references: list[dict[str, Any]] = []
    by_source = {"events": 0, "cookie_events": 0}
    by_event_type: dict[str, int] = {}

    def add_reference(source: str, item: dict[str, Any]) -> None:
        path = str(item.get("screenshot") or "")
        if not path:
            return
        event_type = str(item.get("type") or item.get("event") or "")
        references.append(
            {
                "source": source,
                "type": event_type,
                "ts": item.get("ts"),
                "page_url": item.get("page_url") or "",
                "request_url": item.get("request_url") or item.get("url") or "",
                "screenshot": path,
            }
        )
        by_source[source] = by_source.get(source, 0) + 1
        by_event_type[event_type] = by_event_type.get(event_type, 0) + 1
        seen.add(path)

    for event in recording.get("events", []) or []:
        add_reference("events", dict(event or {}))
    for event in recording.get("cookie_events", []) or []:
        add_reference("cookie_events", dict(event or {}))

    manifest = dict(recording.get("screenshots") or {})
    manifest_files = list(manifest.get("files") or [])
    for item in manifest_files:
        path = str(dict(item).get("path") or "")
        if path:
            seen.add(path)

    return {
        "enabled": bool(manifest.get("enabled", True)),
        "dir": str(manifest.get("dir") or ""),
        "count": max(int(manifest.get("count") or 0), len(seen)),
        "referenced_count": len({item["screenshot"] for item in references}),
        "by_source": by_source,
        "by_event_type": by_event_type,
        "files": manifest_files,
        "references": references,
    }


def build_protocol_plan(recording: dict[str, Any], *, resource_types: set[str] | None = None) -> dict[str, Any]:
    allowed = resource_types or {"document", "xhr", "fetch"}
    steps: list[dict[str, Any]] = []
    for event in recording.get("events", []) or []:
        if event.get("type") != "request":
            continue
        resource_type = str(event.get("resource_type") or "")
        if resource_type not in allowed:
            continue
        headers = {
            key: value
            for key, value in dict(event.get("headers") or {}).items()
            if key.lower() not in _HOP_BY_HOP_HEADERS and not key.lower().startswith("sec-")
        }
        headers.pop("cookie", None)
        steps.append(
            {
                "method": event.get("method") or "GET",
                "url": event.get("url") or "",
                "resource_type": resource_type,
                "headers": headers,
                "post_data": event.get("post_data"),
            }
        )
    return {
        "schema": "autoreg.protocol_plan.v1",
        "source_recording": recording.get("start_url", ""),
        "created_at": BrowserFlowRecorder._ts(),
        "steps": steps,
        "cookie_jar": recording.get("final_storage_state", {}).get("cookies", []),
    }
