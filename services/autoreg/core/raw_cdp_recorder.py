from __future__ import annotations

import base64
import hashlib
import json
import os
import select
import socket
import ssl
import struct
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from core.browser_flow_recorder import analyze_cookie_timeline, summarize_recording_screenshots


_SENSITIVE_HEADER_NAMES = {
    "authorization",
    "cookie",
    "set-cookie",
    "proxy-authorization",
}

_DIRECT_PAGE_SESSION_ID = "__direct_page__"

_FORM_SNAPSHOT_SCRIPT = r"""
(() => {
  const STORE_KEY = "__autoregFormCapture";
  const MAX_EVENTS = 2000;
  function cssPath(el) {
    if (!el || !el.tagName) return "";
    if (el.id) return `${el.tagName.toLowerCase()}#${el.id}`;
    const name = el.getAttribute("name");
    const type = el.getAttribute("type");
    const base = `${el.tagName.toLowerCase()}${name ? `[name="${String(name).replace(/"/g, '\\"')}"]` : ""}${type ? `[type="${String(type).replace(/"/g, '\\"')}"]` : ""}`;
    const all = Array.from(document.querySelectorAll(base));
    const idx = all.indexOf(el);
    return idx >= 0 ? `${base}:nth-match(${idx + 1})` : base;
  }
  function fieldValue(el) {
    const tag = (el.tagName || "").toLowerCase();
    const type = (el.getAttribute("type") || "").toLowerCase();
    if (tag === "select") {
      if (el.multiple) return Array.from(el.selectedOptions || []).map((item) => item.value).join(",");
      return el.value || "";
    }
    if (type === "checkbox" || type === "radio") return el.checked ? (el.value || "on") : "";
    return el.value || "";
  }
  function fieldInfo(el, index) {
    const form = el.form || null;
    return {
      index,
      selectorKey: cssPath(el),
      tag: (el.tagName || "").toLowerCase(),
      type: (el.getAttribute("type") || "").toLowerCase(),
      name: el.getAttribute("name") || "",
      id: el.id || "",
      autocomplete: el.getAttribute("autocomplete") || "",
      placeholder: el.getAttribute("placeholder") || "",
      ariaLabel: el.getAttribute("aria-label") || "",
      value: fieldValue(el),
      checked: typeof el.checked === "boolean" ? el.checked : null,
      disabled: !!el.disabled,
      readOnly: !!el.readOnly,
      required: !!el.required,
      formName: form ? (form.getAttribute("name") || "") : "",
      formId: form ? (form.id || "") : "",
      formAction: form ? (form.action || form.getAttribute("action") || "") : "",
      formMethod: form ? (form.method || form.getAttribute("method") || "") : "",
    };
  }
  function fields() {
    return Array.from(document.querySelectorAll("input, textarea, select")).map((el, index) => fieldInfo(el, index));
  }
  if (!window[STORE_KEY]) {
    const state = { events: [] };
    const pushEvent = (event, el) => {
      if (!el || !el.matches || !el.matches("input, textarea, select")) return;
      const all = Array.from(document.querySelectorAll("input, textarea, select"));
      state.events.push({
        ts: Date.now(),
        eventType: event.type,
        field: fieldInfo(el, all.indexOf(el)),
        url: location.href,
        title: document.title,
      });
      if (state.events.length > MAX_EVENTS) state.events.splice(0, state.events.length - MAX_EVENTS);
    };
    const handler = (event) => pushEvent(event, event.target);
    for (const eventName of ["input", "change", "paste", "keyup", "blur"]) {
      document.addEventListener(eventName, handler, true);
    }
    document.addEventListener("submit", (event) => {
      for (const el of Array.from(document.querySelectorAll("input, textarea, select"))) {
        pushEvent(event, el);
      }
    }, true);
    window[STORE_KEY] = state;
  }
  const state = window[STORE_KEY];
  const events = state.events.splice(0, state.events.length);
  return { url: location.href, title: document.title, fields: fields(), events };
})()
"""


@dataclass
class RawCdpFlowRecorderConfig:
    start_url: str = "about:blank"
    cdp_url: str = ""
    output_dir: str = "data/recordings"
    navigate: bool = True
    duration_seconds: int = 0
    include_cookie_values: bool = True
    include_form_snapshots: bool = True
    include_sensitive_form_values: bool = True
    include_post_data: bool = True
    include_response_bodies: bool = False
    include_headers: bool = True
    include_screenshots: bool = True
    capture_all_pages: bool = False
    screenshot_mode: str = "milestones"
    screenshot_full_page: bool = True
    max_response_body_chars: int = 2000
    cookie_snapshot_interval_seconds: float = 2.0
    form_snapshot_interval_seconds: float = 1.0
    target_poll_interval_seconds: float = 1.0
    wait_message: str = "操作完成后回到终端按 Enter 结束录制..."


@dataclass
class RawCdpFlowRecording:
    path: str
    data: dict[str, Any]


class RawCdpFlowRecorder:
    def __init__(self, config: RawCdpFlowRecorderConfig):
        self.config = config
        self.started_at = time.time()
        self.events: list[dict[str, Any]] = []
        self.form_events: list[dict[str, Any]] = []
        self.form_snapshots: list[dict[str, Any]] = []
        self.cookie_events: list[dict[str, Any]] = []
        self.cookie_snapshots: list[dict[str, Any]] = []
        self._form_state: dict[tuple[str, str, str, str], str] = {}
        self._cookie_state: dict[tuple[str, str, str], dict[str, Any]] = {}
        self._request_seq = 0
        self._screenshot_seq = 0
        self._screenshot_dir: Path | None = None
        self._cdp: CdpWebSocket | None = None
        self._cdps: dict[str, CdpWebSocket] = {}
        self._http_base = ""
        self._websocket_url = ""
        self._sessions: dict[str, dict[str, Any]] = {}
        self._execution_contexts: dict[str, dict[str, int]] = {}
        self._responses_by_request: dict[tuple[str, str], dict[str, Any]] = {}
        self._request_to_session: dict[str, str] = {}
        self._pending_command_responses: dict[tuple[str, int], dict[str, Any]] = {}
        self._last_cookie_snapshot_at = 0.0
        self._last_form_snapshot_at = 0.0
        self._last_target_poll_at = 0.0
        self._connection_closed = False
        self._last_cookie_read_ok = True

    def run(self) -> RawCdpFlowRecording:
        output_dir = Path(self.config.output_dir)
        output_dir.mkdir(parents=True, exist_ok=True)
        started_label = time.strftime("%Y%m%d_%H%M%S", time.localtime(self.started_at))
        output_path = output_dir / f"browser_flow_{started_label}.json"
        self._screenshot_dir = output_dir / f"browser_flow_{started_label}_screenshots"
        if self.config.include_screenshots:
            self._screenshot_dir.mkdir(parents=True, exist_ok=True)

        try:
            if self.config.capture_all_pages:
                self._http_base = _http_base_from_cdp_endpoint(self.config.cdp_url)
                if self._http_base:
                    self._initialize_all_page_targets()
                self._initialize_browser_target_monitor()
            if not self._sessions:
                websocket_url = resolve_cdp_websocket_url(self.config.cdp_url)
                if not websocket_url:
                    raise RuntimeError(f"CDP endpoint did not expose a websocket URL: {self.config.cdp_url}")
                self._websocket_url = websocket_url
                self._cdp = CdpWebSocket(websocket_url)
                self._cdp.connect()
                self._initialize_targets()
            primary_session_id = self._primary_session_id()
            if not primary_session_id:
                raise RuntimeError("No CDP page target was available for recording")
            self._snapshot_cookies(primary_session_id, trigger="cdp_attached", page_url=self.config.start_url)
            self._snapshot_forms_for_sessions(force=True, trigger="cdp_attached")
            if self.config.navigate and self.config.start_url and self.config.start_url != "about:blank":
                try:
                    self._send("Page.navigate", {"url": self.config.start_url}, session_id=primary_session_id, timeout=5)
                except Exception as exc:
                    self.events.append(
                        {
                            "ts": self._ts(),
                            "type": "recorder_warning",
                            "page_url": self._page_url(primary_session_id),
                            "method": "Page.navigate",
                            "url": self.config.start_url,
                            "error": str(exc),
                        }
                    )
                self._capture_screenshot(primary_session_id, reason="initial_navigation")
            else:
                self._capture_screenshot(primary_session_id, reason="attached_initial")
            self._snapshot_cookies(primary_session_id, trigger="initial", page_url=self._page_url(primary_session_id))
            self._snapshot_forms_for_sessions(force=True, trigger="initial")
            self._record_until_finished()
            if not self._connection_closed:
                for session_id in list(self._sessions):
                    self._snapshot_forms(session_id, trigger="before_close")
                    self._capture_screenshot(session_id, reason="before_close")
                    self._snapshot_cookies(session_id, trigger="before_close", page_url=self._page_url(session_id))
            if self._connection_closed:
                final_cookies = []
            else:
                try:
                    final_cookies = self._all_cookies(primary_session_id)
                except Exception:
                    final_cookies = []
            final_storage_state = {"cookies": final_cookies, "origins": []}
        finally:
            for cdp in list(self._cdps.values()):
                try:
                    cdp.close()
                except Exception:
                    pass
            try:
                if self._cdp is not None:
                    self._cdp.close()
            except Exception:
                pass

        data = {
            "schema": "autoreg.browser_flow_recording.v1",
            "recorder": "raw_cdp",
            "started_at": self._ts(self.started_at),
            "finished_at": self._ts(time.time()),
            "start_url": self.config.start_url,
            "config": {
                "cdp_url": self._mask_cdp_url(self.config.cdp_url),
                "navigate": self.config.navigate,
                "include_cookie_values": self.config.include_cookie_values,
                "include_form_snapshots": self.config.include_form_snapshots,
                "include_sensitive_form_values": self.config.include_sensitive_form_values,
                "include_post_data": self.config.include_post_data,
                "include_response_bodies": self.config.include_response_bodies,
                "include_headers": self.config.include_headers,
                "include_screenshots": self.config.include_screenshots,
                "capture_all_pages": self.config.capture_all_pages,
                "screenshot_mode": self.config.screenshot_mode,
                "cookie_snapshot_interval_seconds": self.config.cookie_snapshot_interval_seconds,
                "form_snapshot_interval_seconds": self.config.form_snapshot_interval_seconds,
                "target_poll_interval_seconds": self.config.target_poll_interval_seconds,
            },
            "events": self.events,
            "form_events": self.form_events,
            "form_snapshots": self.form_snapshots,
            "cookie_events": self.cookie_events,
            "cookie_snapshots": self.cookie_snapshots,
            "screenshots": self._screenshot_manifest(),
            "final_storage_state": self._storage_state(final_storage_state),
            "analysis": analyze_cookie_timeline(self.cookie_events),
        }
        data["analysis"]["screenshots"] = summarize_recording_screenshots(data)
        data["coverage"] = self._coverage_report(data)
        output_path.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
        return RawCdpFlowRecording(path=str(output_path), data=data)

    def _initialize_targets(self) -> None:
        if _is_page_websocket_url(self._websocket_url):
            self._attach_direct_page()
            return
        self._send("Target.setDiscoverTargets", {"discover": True})
        targets = self._send("Target.getTargets").get("targetInfos") or []
        page_targets = [target for target in targets if target.get("type") == "page"]
        if not page_targets:
            target_id = self._send("Target.createTarget", {"url": self.config.start_url or "about:blank"}).get("targetId")
            if target_id:
                page_targets.append({"targetId": target_id, "type": "page", "url": self.config.start_url or "about:blank"})
        for target in page_targets:
            self._attach_target(str(target.get("targetId") or ""), str(target.get("url") or ""))

    def _initialize_browser_target_monitor(self) -> None:
        websocket_url = resolve_cdp_browser_websocket_url(self.config.cdp_url)
        if not websocket_url or _is_page_websocket_url(websocket_url):
            return
        try:
            self._websocket_url = websocket_url
            self._cdp = CdpWebSocket(websocket_url)
            self._cdp.connect()
            for method, params in (
                ("Target.setDiscoverTargets", {"discover": True}),
                (
                    "Target.setAutoAttach",
                    {
                        "autoAttach": True,
                        "waitForDebuggerOnStart": False,
                        "flatten": True,
                        "filter": [{"type": "page", "exclude": False}],
                    },
                ),
            ):
                try:
                    self._send(method, params, timeout=3)
                except Exception as exc:
                    self.events.append(
                        {
                            "ts": self._ts(),
                            "type": "recorder_warning",
                            "method": method,
                            "error": str(exc),
                        }
                    )
            try:
                targets = self._send("Target.getTargets", timeout=3).get("targetInfos") or []
            except Exception:
                targets = []
            for target in targets:
                if isinstance(target, dict) and target.get("type") == "page":
                    self._attach_target(str(target.get("targetId") or ""), str(target.get("url") or ""))
        except Exception as exc:
            self.events.append(
                {
                    "ts": self._ts(),
                    "type": "recorder_warning",
                    "method": "browser_target_monitor",
                    "error": str(exc),
                }
            )

    def _attach_target(self, target_id: str, url: str = "") -> str:
        if not target_id:
            return ""
        for existing_session_id, info in list(self._sessions.items()):
            if str(info.get("target_id") or "") == target_id and not info.get("direct"):
                return existing_session_id
        result = self._send("Target.attachToTarget", {"targetId": target_id, "flatten": True})
        session_id = str(result.get("sessionId") or "")
        if not session_id:
            return ""
        self._sessions[session_id] = {
            "target_id": target_id,
            "url": url,
            "enabled": False,
            "protocol_session_id": session_id,
            "direct": False,
        }
        self._enable_session_capture(session_id)
        self._sessions[session_id]["enabled"] = True
        self._snapshot_forms(session_id, trigger="target_attached")
        return session_id

    def _on_attached_to_target(self, params: dict[str, Any]) -> None:
        target_info = params.get("targetInfo") or {}
        if target_info.get("type") != "page":
            return
        session_id = str(params.get("sessionId") or "")
        if not session_id:
            return
        if session_id in self._sessions and self._sessions[session_id].get("enabled"):
            return
        self._sessions[session_id] = {
            "target_id": str(target_info.get("targetId") or ""),
            "url": str(target_info.get("url") or ""),
            "enabled": False,
            "protocol_session_id": session_id,
            "direct": False,
            "title": str(target_info.get("title") or ""),
        }
        self._enable_session_capture(session_id)
        self._sessions[session_id]["enabled"] = True
        self._snapshot_forms(session_id, trigger="target_auto_attached")

    def _initialize_all_page_targets(self) -> None:
        self._attach_current_page_targets(force=True)

    def _attach_current_page_targets(self, *, force: bool = False) -> None:
        if not self._http_base:
            return
        now = time.time()
        interval = float(self.config.target_poll_interval_seconds or 0)
        if not force and interval > 0 and (now - self._last_target_poll_at) < interval:
            return
        self._last_target_poll_at = now
        try:
            targets = _get_json(f"{self._http_base.rstrip('/')}/json/list", timeout=2)
        except Exception:
            return
        if not isinstance(targets, list):
            return
        for target in targets:
            if not isinstance(target, dict):
                continue
            if target.get("type") != "page":
                continue
            websocket_url = str(target.get("webSocketDebuggerUrl") or "")
            if not websocket_url:
                continue
            target_id = str(target.get("id") or websocket_url)
            session_id = f"page:{target_id}"
            if session_id in self._sessions:
                if target.get("url"):
                    self._sessions[session_id]["url"] = str(target.get("url") or "")
                continue
            self._attach_page_websocket(session_id=session_id, websocket_url=websocket_url, target=target)

    def _attach_page_websocket(self, *, session_id: str, websocket_url: str, target: dict[str, Any] | None = None) -> str:
        target = dict(target or {})
        cdp = CdpWebSocket(websocket_url)
        cdp.connect()
        self._cdps[session_id] = cdp
        self._sessions[session_id] = {
            "target_id": str(target.get("id") or ""),
            "url": str(target.get("url") or ""),
            "enabled": False,
            "protocol_session_id": "",
            "direct": True,
            "websocket_url": websocket_url,
            "title": str(target.get("title") or ""),
        }
        self._enable_session_capture(session_id)
        try:
            frame_tree = self._send("Page.getFrameTree", {}, session_id=session_id, timeout=2)
            frame = ((frame_tree.get("frameTree") or {}).get("frame") or {})
            if frame.get("url"):
                self._sessions[session_id]["url"] = str(frame.get("url") or "")
        except Exception:
            pass
        self._sessions[session_id]["enabled"] = True
        self._snapshot_forms(session_id, trigger="target_attached")
        return session_id

    def _attach_direct_page(self) -> str:
        session_id = _DIRECT_PAGE_SESSION_ID
        self._sessions[session_id] = {
            "target_id": "",
            "url": "",
            "enabled": False,
            "protocol_session_id": "",
            "direct": True,
        }
        self._enable_session_capture(session_id)
        try:
            frame_tree = self._send("Page.getFrameTree", {}, session_id=session_id, timeout=2)
            frame = ((frame_tree.get("frameTree") or {}).get("frame") or {})
            if frame.get("url"):
                self._sessions[session_id]["url"] = str(frame.get("url") or "")
        except Exception:
            pass
        self._sessions[session_id]["enabled"] = True
        self._snapshot_forms(session_id, trigger="target_attached")
        return session_id

    def _enable_session_capture(self, session_id: str) -> None:
        commands: list[tuple[str, dict[str, Any]]] = [
            ("Page.enable", {}),
            ("Runtime.enable", {}),
            ("Page.setLifecycleEventsEnabled", {"enabled": True}),
        ]
        if self.config.include_form_snapshots:
            commands.append(("Page.addScriptToEvaluateOnNewDocument", {"source": _FORM_SNAPSHOT_SCRIPT}))
        commands.append(("Network.enable", {"maxTotalBufferSize": 100_000_000, "maxResourceBufferSize": 10_000_000}))
        for method, params in commands:
            try:
                self._send(method, params, session_id=session_id, timeout=3)
            except Exception:
                pass

    def _record_until_finished(self) -> None:
        stop_event = threading.Event()
        input_thread = None
        deadline = 0.0
        if self.config.duration_seconds > 0:
            deadline = time.time() + self.config.duration_seconds
        else:
            input_thread = threading.Thread(target=self._wait_for_enter, args=(stop_event,), daemon=True)
            input_thread.start()
        first_poll = True
        while True:
            if deadline and time.time() >= deadline:
                break
            if stop_event.is_set() and not first_poll:
                break
            if self.config.capture_all_pages:
                self._attach_current_page_targets()
            try:
                message = self._recv(timeout=0.25)
                first_poll = False
            except (ConnectionError, OSError) as exc:
                first_poll = False
                self._connection_closed = True
                self.events.append(
                    {
                        "ts": self._ts(),
                        "type": "recorder_warning",
                        "page_url": self._page_url(self._primary_session_id()),
                        "method": "websocket.recv",
                        "error": str(exc),
                    }
                )
                break
            if message:
                source_session_id = str(message.pop("__source_session_id", ""))
                self._handle_message(message, source_session_id=source_session_id)
            self._snapshot_forms_for_sessions(force=False, trigger="interval")
        if input_thread is not None:
            input_thread.join(timeout=0.1)

    def _wait_for_enter(self, stop_event: threading.Event) -> None:
        try:
            input(self.config.wait_message)
        except (EOFError, OSError):
            pass
        stop_event.set()

    def _handle_message(self, message: dict[str, Any], *, source_session_id: str = "") -> None:
        method = str(message.get("method") or "")
        params = dict(message.get("params") or {})
        session_id = self._logical_session_id(str(message.get("sessionId") or ""), source_session_id=source_session_id)
        if method == "Target.targetCreated":
            target_info = params.get("targetInfo") or {}
            if target_info.get("type") == "page":
                self._attach_target(str(target_info.get("targetId") or ""), str(target_info.get("url") or ""))
            return
        if method == "Target.attachedToTarget":
            self._on_attached_to_target(params)
            return
        if method == "Target.detachedFromTarget":
            detached_session_id = str(params.get("sessionId") or "")
            if detached_session_id:
                self._sessions.pop(detached_session_id, None)
                self._execution_contexts.pop(detached_session_id, None)
            return
        if method == "Runtime.executionContextCreated":
            self._on_execution_context_created(session_id, params)
            return
        if method == "Runtime.executionContextDestroyed":
            self._on_execution_context_destroyed(session_id, params)
            return
        if method == "Runtime.executionContextsCleared":
            self._execution_contexts.pop(session_id, None)
            return
        if method == "Page.frameAttached":
            self.events.append(
                {
                    "ts": self._ts(),
                    "type": "frame_attached",
                    "page_url": self._page_url(session_id),
                    "frame_id": str(params.get("frameId") or ""),
                    "parent_frame_id": str(params.get("parentFrameId") or ""),
                }
            )
            self._snapshot_forms(session_id, trigger="frame_attached")
            return
        if method == "Page.frameDetached":
            frame_id = str(params.get("frameId") or "")
            self.events.append(
                {
                    "ts": self._ts(),
                    "type": "frame_detached",
                    "page_url": self._page_url(session_id),
                    "frame_id": frame_id,
                    "reason": str(params.get("reason") or ""),
                }
            )
            self._forget_frame_context(session_id, frame_id)
            return
        if method == "Page.frameNavigated":
            frame = params.get("frame") or {}
            page_url = str(frame.get("url") or "")
            parent_frame_id = str(frame.get("parentId") or "")
            top_level = not parent_frame_id
            if page_url and top_level and session_id in self._sessions:
                self._sessions[session_id]["url"] = page_url
            self.events.append(
                {
                    "ts": self._ts(),
                    "type": "navigation",
                    "page_url": self._page_url(session_id),
                    "frame_id": str(frame.get("id") or ""),
                    "parent_frame_id": parent_frame_id,
                    "top_level": top_level,
                    "frame_url": page_url,
                    "screenshot": self._capture_screenshot(session_id, reason="navigation"),
                }
            )
            self._snapshot_cookies(session_id, trigger="navigation", page_url=page_url)
            self._snapshot_forms(session_id, trigger="navigation")
            return
        if method == "Page.lifecycleEvent":
            if str(params.get("name") or "") in {"DOMContentLoaded", "load", "networkIdle"}:
                self._snapshot_forms(session_id, trigger="lifecycle_" + str(params.get("name") or ""))
            return
        if method == "Page.javascriptDialogOpening":
            self.events.append(
                {
                    "ts": self._ts(),
                    "type": "javascript_dialog_opening",
                    "page_url": self._page_url(session_id),
                    "url": str(params.get("url") or ""),
                    "message": str(params.get("message") or ""),
                    "dialog_type": str(params.get("type") or ""),
                    "default_prompt": str(params.get("defaultPrompt") or ""),
                    "screenshot": self._capture_screenshot(session_id, reason="javascript_dialog_opening"),
                }
            )
            self._snapshot_forms(session_id, trigger="javascript_dialog_opening")
            return
        if method == "Page.javascriptDialogClosed":
            self.events.append(
                {
                    "ts": self._ts(),
                    "type": "javascript_dialog_closed",
                    "page_url": self._page_url(session_id),
                    "accepted": bool(params.get("result")),
                    "user_input": str(params.get("userInput") or ""),
                    "screenshot": self._capture_screenshot(session_id, reason="javascript_dialog_closed"),
                }
            )
            self._snapshot_forms(session_id, trigger="javascript_dialog_closed")
            return
        if method == "Network.requestWillBeSent":
            self._on_request(session_id, params)
            return
        if method == "Network.responseReceived":
            self._on_response_received(session_id, params)
            return
        if method == "Network.loadingFinished":
            self._on_loading_finished(session_id, params)
            return

    def _on_request(self, session_id: str, params: dict[str, Any]) -> None:
        request = dict(params.get("request") or {})
        request_id = str(params.get("requestId") or "")
        self._request_to_session[request_id] = session_id
        if request.get("url") and session_id in self._sessions:
            if str(params.get("type") or "").lower() == "document":
                self._sessions[session_id]["url"] = str(request.get("url") or "")
        self._request_seq += 1
        entry = {
            "id": self._request_seq,
            "ts": self._ts(),
            "type": "request",
            "page_url": self._page_url(session_id),
            "method": request.get("method") or "GET",
            "url": request.get("url") or "",
            "resource_type": str(params.get("type") or "").lower(),
            "headers": self._headers(request.get("headers") or {}) if self.config.include_headers else {},
        }
        post_data = request.get("postData")
        if self.config.include_post_data and post_data not in (None, ""):
            entry["post_data"] = post_data
            entry["post_data_sha256"] = self._sha256(post_data)
        screenshot = self._capture_screenshot(session_id, reason="request")
        if screenshot:
            entry["screenshot"] = screenshot
        self.events.append(entry)
        self._snapshot_cookies(
            session_id,
            trigger="request",
            page_url=self._page_url(session_id),
            request_url=str(request.get("url") or ""),
            force=False,
        )

    def _on_response_received(self, session_id: str, params: dict[str, Any]) -> None:
        response = dict(params.get("response") or {})
        request_id = str(params.get("requestId") or "")
        headers = self._headers(response.get("headers") or {}) if self.config.include_headers else {}
        self._responses_by_request[(session_id, request_id)] = {
            "ts": self._ts(),
            "type": "response",
            "page_url": self._page_url(session_id),
            "url": response.get("url") or "",
            "status": int(response.get("status") or 0),
            "resource_type": str(params.get("type") or "").lower(),
            "headers": headers,
            "set_cookie_headers": self._set_cookie_headers(response.get("headers") or {}),
            "content_type": self._content_type(response.get("headers") or {}),
        }

    def _on_loading_finished(self, session_id: str, params: dict[str, Any]) -> None:
        request_id = str(params.get("requestId") or "")
        entry = self._responses_by_request.pop((session_id, request_id), None)
        if not entry:
            return
        if self.config.include_response_bodies:
            entry.update(self._response_body_summary(session_id, request_id, entry.get("content_type") or ""))
        screenshot = self._capture_screenshot(session_id, reason="response")
        if screenshot:
            entry["screenshot"] = screenshot
        self.events.append(entry)
        self._snapshot_cookies(
            session_id,
            trigger="response",
            page_url=self._page_url(session_id),
            request_url=str(entry.get("url") or ""),
            set_cookie_headers=list(entry.get("set_cookie_headers") or []),
            force=bool(entry.get("set_cookie_headers")),
        )

    def _response_body_summary(self, session_id: str, request_id: str, content_type: str) -> dict[str, Any]:
        lower_type = str(content_type or "").lower()
        if not any(marker in lower_type for marker in ("json", "text", "html", "javascript", "x-www-form-urlencoded")):
            return {"content_type": content_type}
        try:
            result = self._send("Network.getResponseBody", {"requestId": request_id}, session_id=session_id, timeout=2)
        except Exception as exc:
            return {"content_type": content_type, "body_error": str(exc)}
        body = str(result.get("body") or "")
        if result.get("base64Encoded"):
            try:
                body = base64.b64decode(body).decode("utf-8", errors="replace")
            except Exception as exc:
                return {"content_type": content_type, "body_error": str(exc)}
        summary = {
            "content_type": content_type,
            "body_sha256": self._sha256(body),
            "body_len": len(body),
            "body_preview": body[: self.config.max_response_body_chars],
        }
        if "json" in lower_type:
            try:
                summary["json_shape"] = self._json_shape(json.loads(body))
            except Exception:
                pass
        return summary

    def _snapshot_cookies(
        self,
        session_id: str,
        *,
        trigger: str,
        page_url: str = "",
        request_url: str = "",
        set_cookie_headers: list[str] | None = None,
        force: bool = True,
    ) -> None:
        if not self._should_snapshot_cookies(trigger, force=force):
            return
        cookies = self._all_cookies(session_id)
        if not self._last_cookie_read_ok:
            return
        self._last_cookie_snapshot_at = time.time()
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
                screenshot = self._capture_screenshot(session_id, reason=f"cookie_created_{record['name']}")
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
                screenshot = self._capture_screenshot(session_id, reason=f"cookie_updated_{record['name']}")
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
                screenshot = self._capture_screenshot(session_id, reason=f"cookie_deleted_{previous['name']}")
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

    def _all_cookies(self, session_id: str) -> list[dict[str, Any]]:
        for method in ("Network.getAllCookies", "Storage.getCookies"):
            try:
                result = self._send(method, {}, session_id=session_id, timeout=2)
                cookies = result.get("cookies") or []
                if isinstance(cookies, list):
                    self._last_cookie_read_ok = True
                    return [dict(cookie) for cookie in cookies if isinstance(cookie, dict)]
            except Exception:
                continue
        self._last_cookie_read_ok = False
        return []

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

    def _capture_screenshot(self, session_id: str, *, reason: str) -> str:
        if self._connection_closed:
            return ""
        if not self.config.include_screenshots or self._screenshot_dir is None or not session_id:
            return ""
        if not self._should_capture_screenshot(reason):
            return ""
        try:
            self._screenshot_seq += 1
            safe_reason = "".join(ch if ch.isalnum() or ch in {"_", "-"} else "_" for ch in str(reason or "event"))[:80]
            path = self._screenshot_dir / f"{self._screenshot_seq:05d}_{safe_reason}.png"
            params = {
                "format": "png",
                "fromSurface": True,
                "captureBeyondViewport": bool(self.config.screenshot_full_page),
            }
            result = self._send("Page.captureScreenshot", params, session_id=session_id, timeout=3)
            image_data = str(result.get("data") or "")
            if not image_data:
                return ""
            path.write_bytes(base64.b64decode(image_data))
            return str(path)
        except Exception:
            return ""

    def _should_capture_screenshot(self, reason: str) -> bool:
        mode = str(self.config.screenshot_mode or "milestones").strip().lower()
        if mode in {"off", "none", "false", "0"}:
            return False
        if mode in {"all", "full"}:
            return True
        text = str(reason or "")
        return (
            text.startswith("initial")
            or text.startswith("attached")
            or text.startswith("before_close")
            or text.startswith("navigation")
            or text.startswith("cookie_")
        )

    def _should_snapshot_cookies(self, trigger: str, *, force: bool = False) -> bool:
        if force:
            return True
        trigger_text = str(trigger or "")
        if trigger_text not in {"request", "response"}:
            return True
        interval = float(self.config.cookie_snapshot_interval_seconds or 0)
        if interval <= 0:
            return True
        return (time.time() - self._last_cookie_snapshot_at) >= interval

    def _snapshot_forms_for_sessions(self, *, force: bool = False, trigger: str = "interval") -> None:
        if not self.config.include_form_snapshots:
            return
        if not self._should_snapshot_forms(force=force):
            return
        self._last_form_snapshot_at = time.time()
        for session_id in list(self._sessions):
            self._snapshot_forms(session_id, trigger=trigger)

    def _snapshot_forms(self, session_id: str, *, trigger: str) -> None:
        if not self.config.include_form_snapshots:
            return
        frames = self._frames_for_session(session_id)
        if not frames:
            frames = [
                {
                    "id": "",
                    "parentId": "",
                    "url": self._page_url(session_id),
                    "name": "",
                    "mimeType": "",
                    "securityOrigin": "",
                }
            ]
        for frame in frames:
            self._snapshot_forms_in_frame(session_id, trigger=trigger, frame=frame)

    def _snapshot_forms_in_frame(self, session_id: str, *, trigger: str, frame: dict[str, Any]) -> None:
        frame_record = self._frame_record(frame)
        params: dict[str, Any] = {
            "expression": _FORM_SNAPSHOT_SCRIPT,
            "returnByValue": True,
            "awaitPromise": False,
        }
        context_id = self._context_id_for_frame(session_id, frame_record["id"])
        if context_id:
            params["contextId"] = context_id
        elif frame_record["id"] and frame_record["parent_id"]:
            try:
                isolated = self._send(
                    "Page.createIsolatedWorld",
                    {
                        "frameId": frame_record["id"],
                        "worldName": "autoreg_form_capture",
                        "grantUniveralAccess": True,
                    },
                    session_id=session_id,
                    timeout=2,
                )
                isolated_context_id = isolated.get("executionContextId")
                if isolated_context_id:
                    params["contextId"] = int(isolated_context_id)
            except Exception as exc:
                self._append_form_snapshot_error(session_id, trigger=trigger, frame=frame_record, error=exc)
                return
        try:
            result = self._send("Runtime.evaluate", params, session_id=session_id, timeout=2)
        except Exception as exc:
            if params.get("contextId") and not frame_record["parent_id"]:
                params.pop("contextId", None)
                try:
                    result = self._send("Runtime.evaluate", params, session_id=session_id, timeout=2)
                except Exception as fallback_exc:
                    self._append_form_snapshot_error(session_id, trigger=trigger, frame=frame_record, error=fallback_exc)
                    return
            else:
                self._append_form_snapshot_error(session_id, trigger=trigger, frame=frame_record, error=exc)
                return
        value = ((result.get("result") or {}).get("value") or {})
        raw_fields = (value.get("fields") or [])
        raw_input_events = (value.get("events") or [])
        fields = [self._form_field_record(dict(field or {})) for field in raw_fields if isinstance(field, dict)]
        snapshot = {
            "ts": self._ts(),
            "trigger": trigger,
            "page_url": self._page_url(session_id),
            "session_id": session_id,
            "frame": frame_record,
            "document_url": str(value.get("url") or ""),
            "document_title": str(value.get("title") or ""),
            "count": len(fields),
            "fields": fields,
        }
        self.form_snapshots.append(snapshot)
        for raw_event in raw_input_events:
            if not isinstance(raw_event, dict):
                continue
            field = self._form_field_record(dict(raw_event.get("field") or {}))
            self.form_events.append(
                {
                    "ts": self._ts_from_ms(raw_event.get("ts")) or snapshot["ts"],
                    "event": "dom_" + str(raw_event.get("eventType") or "input"),
                    "trigger": "dom_listener",
                    "page_url": snapshot["page_url"],
                    "frame": frame_record,
                    "document_url": str(raw_event.get("url") or ""),
                    "document_title": str(raw_event.get("title") or ""),
                    "field": field,
                }
            )
        for field in fields:
            key = (
                session_id,
                frame_record["id"],
                snapshot["document_url"] or snapshot["page_url"],
                str(field.get("selector_key") or field.get("index") or ""),
            )
            fingerprint = self._form_field_fingerprint(field)
            previous = self._form_state.get(key)
            if previous is None:
                if field.get("value_len") or field.get("checked") is not None:
                    self.form_events.append(
                        {
                            "ts": snapshot["ts"],
                            "event": "observed",
                            "trigger": trigger,
                            "page_url": snapshot["page_url"],
                            "frame": frame_record,
                            "field": field,
                        }
                    )
            elif previous != fingerprint:
                self.form_events.append(
                    {
                        "ts": snapshot["ts"],
                        "event": "changed",
                        "trigger": trigger,
                        "page_url": snapshot["page_url"],
                        "frame": frame_record,
                        "field": field,
                    }
                )
            self._form_state[key] = fingerprint

    def _append_form_snapshot_error(
        self,
        session_id: str,
        *,
        trigger: str,
        frame: dict[str, Any],
        error: Exception,
    ) -> None:
        self.form_events.append(
            {
                "ts": self._ts(),
                "event": "snapshot_error",
                "trigger": trigger,
                "page_url": self._page_url(session_id),
                "frame": frame,
                "error": str(error),
            }
        )

    def _frames_for_session(self, session_id: str) -> list[dict[str, Any]]:
        try:
            result = self._send("Page.getFrameTree", {}, session_id=session_id, timeout=2)
        except Exception:
            return []
        root = result.get("frameTree") or {}
        frames: list[dict[str, Any]] = []
        self._collect_frames(root, frames)
        return frames

    def _collect_frames(self, node: dict[str, Any], frames: list[dict[str, Any]]) -> None:
        frame = node.get("frame") or {}
        if isinstance(frame, dict):
            frames.append(dict(frame))
        for child in node.get("childFrames") or []:
            if isinstance(child, dict):
                self._collect_frames(child, frames)

    @staticmethod
    def _frame_record(frame: dict[str, Any]) -> dict[str, str]:
        return {
            "id": str(frame.get("id") or ""),
            "parent_id": str(frame.get("parentId") or frame.get("parent_id") or ""),
            "url": str(frame.get("url") or ""),
            "name": str(frame.get("name") or ""),
            "mime_type": str(frame.get("mimeType") or frame.get("mime_type") or ""),
            "security_origin": str(frame.get("securityOrigin") or frame.get("security_origin") or ""),
        }

    def _on_execution_context_created(self, session_id: str, params: dict[str, Any]) -> None:
        context = params.get("context") or {}
        aux_data = context.get("auxData") or {}
        frame_id = str(aux_data.get("frameId") or "")
        context_id = context.get("id")
        if not frame_id or not isinstance(context_id, int):
            return
        if aux_data.get("isDefault") is False:
            return
        self._execution_contexts.setdefault(session_id, {})[frame_id] = context_id

    def _on_execution_context_destroyed(self, session_id: str, params: dict[str, Any]) -> None:
        context_id = params.get("executionContextId")
        contexts = self._execution_contexts.get(session_id) or {}
        for frame_id, stored_context_id in list(contexts.items()):
            if stored_context_id == context_id:
                contexts.pop(frame_id, None)
        if not contexts:
            self._execution_contexts.pop(session_id, None)

    def _forget_frame_context(self, session_id: str, frame_id: str) -> None:
        contexts = self._execution_contexts.get(session_id) or {}
        contexts.pop(frame_id, None)
        if not contexts:
            self._execution_contexts.pop(session_id, None)

    def _context_id_for_frame(self, session_id: str, frame_id: str) -> int:
        try:
            return int((self._execution_contexts.get(session_id) or {}).get(frame_id) or 0)
        except (TypeError, ValueError):
            return 0

    def _form_field_record(self, field: dict[str, Any]) -> dict[str, Any]:
        value = str(field.get("value") or "")
        sensitive = self._is_sensitive_form_field(field)
        record = {
            "index": int(field.get("index") or 0),
            "selector_key": str(field.get("selectorKey") or ""),
            "tag": str(field.get("tag") or ""),
            "type": str(field.get("type") or ""),
            "name": str(field.get("name") or ""),
            "id": str(field.get("id") or ""),
            "autocomplete": str(field.get("autocomplete") or ""),
            "placeholder": str(field.get("placeholder") or ""),
            "aria_label": str(field.get("ariaLabel") or ""),
            "form_name": str(field.get("formName") or ""),
            "form_id": str(field.get("formId") or ""),
            "form_action": str(field.get("formAction") or ""),
            "form_method": str(field.get("formMethod") or ""),
            "checked": field.get("checked"),
            "disabled": bool(field.get("disabled")),
            "readOnly": bool(field.get("readOnly")),
            "required": bool(field.get("required")),
            "value_sha256": self._sha256(value),
            "value_len": len(value),
            "sensitive": sensitive,
        }
        if value and (not sensitive or self.config.include_sensitive_form_values):
            record["value"] = value
        elif value:
            record["value_redacted"] = True
        return record

    def _is_sensitive_form_field(self, field: dict[str, Any]) -> bool:
        text = " ".join(
            str(field.get(key) or "").lower()
            for key in ("type", "name", "id", "autocomplete", "placeholder", "ariaLabel")
        )
        markers = (
            "password",
            "passwd",
            "passcode",
            "pwd",
            "card",
            "cc-",
            "cc_",
            "credit",
            "cvv",
            "cvc",
            "security code",
            "verification",
            "otp",
            "one-time",
            "token",
            "secret",
            "hidden",
        )
        return any(marker in text for marker in markers)

    def _form_field_fingerprint(self, field: dict[str, Any]) -> str:
        return self._sha256(
            json.dumps(
                {
                    "value_sha256": field.get("value_sha256"),
                    "value_len": field.get("value_len"),
                    "checked": field.get("checked"),
                    "name": field.get("name"),
                    "id": field.get("id"),
                },
                sort_keys=True,
            )
        )

    def _should_snapshot_forms(self, *, force: bool = False) -> bool:
        if force:
            return True
        interval = float(self.config.form_snapshot_interval_seconds or 0)
        if interval <= 0:
            return True
        return (time.time() - self._last_form_snapshot_at) >= interval

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

    def _send(self, method: str, params: dict[str, Any] | None = None, *, session_id: str = "", timeout: float = 10) -> dict[str, Any]:
        cdp = self._socket_for_session(session_id)
        if cdp is None:
            raise RuntimeError("CDP websocket is not connected")
        socket_key = self._socket_key(session_id)
        command_id = cdp.send_command(method, params or {}, session_id=self._protocol_session_id(session_id))
        deadline = time.time() + timeout
        pending = self._pending_command_responses.pop((socket_key, command_id), None)
        if pending is not None:
            if "error" in pending:
                raise RuntimeError(json.dumps(pending.get("error"), ensure_ascii=False))
            return dict(pending.get("result") or {})
        while time.time() < deadline:
            message = cdp.recv_json(timeout=max(0.05, min(0.25, deadline - time.time())))
            if not message:
                continue
            if message.get("id") == command_id:
                if "error" in message:
                    raise RuntimeError(json.dumps(message.get("error"), ensure_ascii=False))
                return dict(message.get("result") or {})
            other_id = message.get("id")
            if isinstance(other_id, int):
                self._pending_command_responses[(socket_key, other_id)] = message
            else:
                self._handle_message(message, source_session_id=session_id)
        raise TimeoutError(f"CDP command timed out: {method}")

    def _recv(self, *, timeout: float = 0.25) -> dict[str, Any] | None:
        if self._cdps:
            deadline = time.time() + timeout
            while time.time() < deadline:
                for session_id, cdp in list(self._cdps.items()):
                    try:
                        message = cdp.recv_json(timeout=0)
                    except (ConnectionError, OSError):
                        self._detach_page_websocket(session_id)
                        continue
                    if message:
                        message["__source_session_id"] = session_id
                        return message
                time.sleep(min(0.05, max(deadline - time.time(), 0)))
            return None
        if self._cdp is None:
            return None
        return self._cdp.recv_json(timeout=timeout)

    def _detach_page_websocket(self, session_id: str) -> None:
        cdp = self._cdps.pop(session_id, None)
        if cdp is not None:
            try:
                cdp.close()
            except Exception:
                pass
        self._sessions.pop(session_id, None)

    def _primary_session_id(self) -> str:
        if not self._sessions:
            return ""
        for session_id, info in self._sessions.items():
            if str(info.get("url") or "") not in {"", "about:blank"}:
                return session_id
        return next(iter(self._sessions))

    def _page_url(self, session_id: str) -> str:
        return str((self._sessions.get(session_id) or {}).get("url") or "")

    def _logical_session_id(self, protocol_session_id: str, *, source_session_id: str = "") -> str:
        if protocol_session_id:
            return protocol_session_id
        if source_session_id:
            return source_session_id
        if _DIRECT_PAGE_SESSION_ID in self._sessions:
            return _DIRECT_PAGE_SESSION_ID
        return ""

    def _protocol_session_id(self, session_id: str) -> str:
        info = self._sessions.get(session_id) or {}
        if info.get("direct"):
            return ""
        return str(info.get("protocol_session_id") or session_id or "")

    def _socket_for_session(self, session_id: str) -> CdpWebSocket | None:
        if session_id in self._cdps:
            return self._cdps[session_id]
        return self._cdp

    def _socket_key(self, session_id: str) -> str:
        if session_id in self._cdps:
            return session_id
        return "__browser__"

    def _coverage_report(self, data: dict[str, Any]) -> dict[str, Any]:
        events = list(data.get("events") or [])
        form_events = list(data.get("form_events") or [])
        form_snapshots = list(data.get("form_snapshots") or [])
        cookie_events = list(data.get("cookie_events") or [])
        urls = [
            str(event.get("url") or event.get("request_url") or event.get("frame_url") or event.get("page_url") or "")
            for event in events
        ]
        page_urls = {
            str(item.get("url") or "")
            for item in self._sessions.values()
            if item.get("url")
        }
        domains = sorted({urllib.parse.urlparse(url).netloc for url in urls if urllib.parse.urlparse(url).netloc})
        field_count = sum(int(snapshot.get("count") or 0) for snapshot in form_snapshots)
        warnings: list[str] = []
        if self.config.capture_all_pages and not any("paypal." in url for url in urls + list(page_urls)):
            warnings.append("paypal_target_or_requests_not_observed")
        if self.config.capture_all_pages and not any("stripe." in url or "stripe.com" in url for url in urls + list(page_urls)):
            warnings.append("stripe_target_or_requests_not_observed")
        if self.config.include_form_snapshots and not form_snapshots:
            warnings.append("form_snapshots_not_collected")
        if self.config.include_form_snapshots and field_count == 0:
            warnings.append("no_form_fields_observed")
        if self.config.include_form_snapshots and not form_events:
            warnings.append("no_form_input_or_change_events_observed")
        if not any(event.get("post_data") for event in events if event.get("type") == "request"):
            warnings.append("no_request_post_data_observed")
        if not cookie_events:
            warnings.append("no_cookie_events_observed")
        return {
            "targets": [
                {
                    "session_id": session_id,
                    "url": str(info.get("url") or ""),
                    "title": str(info.get("title") or ""),
                    "direct": bool(info.get("direct")),
                }
                for session_id, info in self._sessions.items()
            ],
            "domains": domains,
            "event_count": len(events),
            "request_count": sum(1 for event in events if event.get("type") == "request"),
            "response_count": sum(1 for event in events if event.get("type") == "response"),
            "post_data_request_count": sum(1 for event in events if event.get("type") == "request" and event.get("post_data")),
            "cookie_event_count": len(cookie_events),
            "cookie_snapshot_count": len(data.get("cookie_snapshots") or []),
            "form_snapshot_count": len(form_snapshots),
            "form_event_count": len(form_events),
            "form_field_observation_count": field_count,
            "warnings": warnings,
            "complete": not warnings,
        }

    @staticmethod
    def _headers(headers: dict[str, Any]) -> dict[str, str]:
        return {str(key): str(value) for key, value in dict(headers or {}).items()}

    @staticmethod
    def _set_cookie_headers(headers: dict[str, Any]) -> list[str]:
        values: list[str] = []
        for key, value in dict(headers or {}).items():
            if str(key).lower() == "set-cookie":
                text = str(value or "")
                values.extend(part for part in text.split("\n") if part.strip())
        return values

    @staticmethod
    def _content_type(headers: dict[str, Any]) -> str:
        for key, value in dict(headers or {}).items():
            if str(key).lower() == "content-type":
                return str(value or "")
        return ""

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
        return {key: value for key, value in cookie.items() if key != "value"}

    def _json_shape(self, value: Any, depth: int = 0) -> Any:
        if depth >= 3:
            return type(value).__name__
        if isinstance(value, dict):
            return {str(key): self._json_shape(item, depth + 1) for key, item in list(value.items())[:50]}
        if isinstance(value, list):
            if not value:
                return []
            return [self._json_shape(value[0], depth + 1)]
        return type(value).__name__

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
    def _mask_cdp_url(cdp_url: str) -> str:
        text = str(cdp_url or "")
        if not text:
            return ""
        if "?" in text:
            return text.split("?", 1)[0] + "?***"
        return text

    @staticmethod
    def _ts(value: float | None = None) -> str:
        return time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(value or time.time()))

    @staticmethod
    def _ts_from_ms(value: Any) -> str:
        try:
            number = float(value) / 1000.0
        except (TypeError, ValueError):
            return ""
        return RawCdpFlowRecorder._ts(number)


def resolve_cdp_websocket_url(cdp_url: str) -> str:
    text = str(cdp_url or "").strip()
    if not text:
        return ""
    if text.startswith(("ws://", "wss://")):
        if _is_page_websocket_url(text):
            return text
        page_websocket = _page_websocket_from_http_base(_http_base_from_websocket_url(text))
        if page_websocket:
            return page_websocket
        return text
    if not text.startswith(("http://", "https://")):
        text = f"http://{text}"
    page_websocket = _page_websocket_from_http_base(text)
    if page_websocket:
        return page_websocket
    try:
        payload = _get_json(f"{text.rstrip('/')}/json/version")
        websocket_url = str(payload.get("webSocketDebuggerUrl") or "")
        if websocket_url:
            return websocket_url
    except Exception:
        pass
    try:
        targets = _get_json(f"{text.rstrip('/')}/json/list")
        if isinstance(targets, list):
            for target in targets:
                if isinstance(target, dict) and target.get("type") == "page" and target.get("webSocketDebuggerUrl"):
                    return str(target.get("webSocketDebuggerUrl") or "")
    except Exception:
        pass
    return text


def _page_websocket_from_http_base(http_base: str) -> str:
    if not http_base:
        return ""
    try:
        targets = _get_json(f"{http_base.rstrip('/')}/json/list")
        if isinstance(targets, list):
            for target in targets:
                if isinstance(target, dict) and target.get("type") == "page" and target.get("webSocketDebuggerUrl"):
                    return str(target.get("webSocketDebuggerUrl") or "")
            for target in targets:
                if isinstance(target, dict) and target.get("webSocketDebuggerUrl"):
                    return str(target.get("webSocketDebuggerUrl") or "")
    except Exception:
        return ""
    return ""


def _http_base_from_websocket_url(websocket_url: str) -> str:
    parsed = urllib.parse.urlparse(str(websocket_url or ""))
    if parsed.scheme not in {"ws", "wss"} or not parsed.hostname:
        return ""
    scheme = "https" if parsed.scheme == "wss" else "http"
    netloc = parsed.hostname
    if parsed.port:
        netloc = f"{netloc}:{parsed.port}"
    return f"{scheme}://{netloc}"


def _http_base_from_cdp_endpoint(cdp_url: str) -> str:
    text = str(cdp_url or "").strip()
    if not text:
        return ""
    if text.startswith(("ws://", "wss://")):
        return _http_base_from_websocket_url(text)
    if not text.startswith(("http://", "https://")):
        text = f"http://{text}"
    parsed = urllib.parse.urlparse(text)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        return ""
    netloc = parsed.hostname
    if parsed.port:
        netloc = f"{netloc}:{parsed.port}"
    return f"{parsed.scheme}://{netloc}"


def _is_page_websocket_url(websocket_url: str) -> bool:
    path = urllib.parse.urlparse(str(websocket_url or "")).path
    return "/devtools/page/" in path


def _get_json(url: str, *, timeout: int = 5) -> Any:
    request = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8", errors="replace"))


class CdpWebSocket:
    def __init__(self, url: str):
        self.url = url
        self._socket: socket.socket | ssl.SSLSocket | None = None
        self._next_id = 0

    def connect(self) -> None:
        parsed = urllib.parse.urlparse(self.url)
        if parsed.scheme not in {"ws", "wss"}:
            raise ValueError(f"CDP URL must be ws:// or wss://, got {self.url}")
        host = parsed.hostname or "127.0.0.1"
        port = parsed.port or (443 if parsed.scheme == "wss" else 80)
        raw_socket = socket.create_connection((host, port), timeout=10)
        if parsed.scheme == "wss":
            raw_socket = ssl.create_default_context().wrap_socket(raw_socket, server_hostname=host)
        raw_socket.settimeout(None)
        key = base64.b64encode(os.urandom(16)).decode("ascii")
        path = parsed.path or "/"
        if parsed.query:
            path = f"{path}?{parsed.query}"
        host_header = host if parsed.port is None else f"{host}:{port}"
        request = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: {host_header}\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n"
            "\r\n"
        )
        raw_socket.sendall(request.encode("ascii"))
        response = self._read_http_response(raw_socket)
        if b" 101 " not in response.split(b"\r\n", 1)[0]:
            raise RuntimeError(response.decode("utf-8", errors="replace"))
        self._socket = raw_socket

    def send_command(self, method: str, params: dict[str, Any] | None = None, *, session_id: str = "") -> int:
        self._next_id += 1
        payload: dict[str, Any] = {
            "id": self._next_id,
            "method": method,
            "params": params or {},
        }
        if session_id:
            payload["sessionId"] = session_id
        self.send_json(payload)
        return self._next_id

    def send_json(self, payload: dict[str, Any]) -> None:
        data = json.dumps(payload, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
        self._send_frame(data, opcode=0x1)

    def recv_json(self, *, timeout: float = 0.25) -> dict[str, Any] | None:
        data = self._recv_message(timeout=timeout)
        if data is None:
            return None
        try:
            return json.loads(data.decode("utf-8", errors="replace"))
        except ValueError:
            return None

    def close(self) -> None:
        if self._socket is None:
            return
        try:
            self._send_frame(b"", opcode=0x8)
        except Exception:
            pass
        try:
            self._socket.close()
        finally:
            self._socket = None

    def _send_frame(self, data: bytes, *, opcode: int) -> None:
        if self._socket is None:
            raise RuntimeError("websocket is not connected")
        length = len(data)
        header = bytearray([0x80 | opcode])
        if length < 126:
            header.append(0x80 | length)
        elif length < (1 << 16):
            header.append(0x80 | 126)
            header.extend(struct.pack("!H", length))
        else:
            header.append(0x80 | 127)
            header.extend(struct.pack("!Q", length))
        mask = os.urandom(4)
        masked = bytes(byte ^ mask[index % 4] for index, byte in enumerate(data))
        self._socket.sendall(bytes(header) + mask + masked)

    def _recv_message(self, *, timeout: float) -> bytes | None:
        chunks: list[bytes] = []
        while True:
            frame = self._recv_frame(timeout=timeout)
            if frame is None:
                return None
            fin, opcode, payload = frame
            if opcode == 0x8:
                raise ConnectionError("websocket closed")
            if opcode == 0x9:
                self._send_frame(payload, opcode=0xA)
                continue
            if opcode == 0xA:
                continue
            if opcode in {0x1, 0x2, 0x0}:
                chunks.append(payload)
                if fin:
                    return b"".join(chunks)

    def _recv_frame(self, *, timeout: float) -> tuple[bool, int, bytes] | None:
        if self._socket is None:
            return None
        readable, _, _ = select.select([self._socket], [], [], timeout)
        if not readable:
            return None
        first = self._read_exact(2)
        if not first:
            return None
        byte1, byte2 = first
        fin = bool(byte1 & 0x80)
        opcode = byte1 & 0x0F
        masked = bool(byte2 & 0x80)
        length = byte2 & 0x7F
        if length == 126:
            length = struct.unpack("!H", self._read_exact(2))[0]
        elif length == 127:
            length = struct.unpack("!Q", self._read_exact(8))[0]
        mask = self._read_exact(4) if masked else b""
        payload = self._read_exact(length) if length else b""
        if masked and mask:
            payload = bytes(byte ^ mask[index % 4] for index, byte in enumerate(payload))
        return fin, opcode, payload

    def _read_exact(self, size: int) -> bytes:
        if self._socket is None:
            return b""
        chunks = bytearray()
        while len(chunks) < size:
            chunk = self._socket.recv(size - len(chunks))
            if not chunk:
                raise ConnectionError("websocket closed")
            chunks.extend(chunk)
        return bytes(chunks)

    @staticmethod
    def _read_http_response(sock: socket.socket | ssl.SSLSocket) -> bytes:
        chunks = bytearray()
        while b"\r\n\r\n" not in chunks:
            chunk = sock.recv(4096)
            if not chunk:
                break
            chunks.extend(chunk)
        return bytes(chunks)
