from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

import requests

from core.browser_flow_recorder import build_protocol_plan


@dataclass
class ProtocolReplayConfig:
    recording_path: str
    output_path: str = ""
    live: bool = False
    resource_types: set[str] = field(default_factory=lambda: {"document", "xhr", "fetch"})
    timeout: int = 30
    max_steps: int = 0


def replay_recording(config: ProtocolReplayConfig) -> dict[str, Any]:
    recording = json.loads(Path(config.recording_path).read_text(encoding="utf-8"))
    plan = build_protocol_plan(recording, resource_types=config.resource_types)
    result = {
        "schema": "autoreg.protocol_replay_result.v1",
        "recording_path": config.recording_path,
        "live": config.live,
        "started_at": _ts(),
        "plan": plan,
        "responses": [],
    }
    if not config.live:
        result["finished_at"] = _ts()
        _write_result(config, result)
        return result

    session = requests.Session()
    _seed_cookies(session, plan.get("cookie_jar") or [])
    steps = list(plan.get("steps") or [])
    if config.max_steps > 0:
        steps = steps[: config.max_steps]

    for index, step in enumerate(steps, start=1):
        started = time.time()
        try:
            response = session.request(
                method=str(step.get("method") or "GET"),
                url=str(step.get("url") or ""),
                headers=dict(step.get("headers") or {}),
                data=step.get("post_data"),
                timeout=config.timeout,
                allow_redirects=False,
            )
            body_preview = response.text[:500] if response.text else ""
            result["responses"].append(
                {
                    "step": index,
                    "method": step.get("method"),
                    "url": step.get("url"),
                    "status": response.status_code,
                    "elapsed_ms": int((time.time() - started) * 1000),
                    "content_type": response.headers.get("content-type", ""),
                    "body_preview": body_preview,
                }
            )
        except Exception as exc:
            result["responses"].append(
                {
                    "step": index,
                    "method": step.get("method"),
                    "url": step.get("url"),
                    "error": str(exc),
                    "elapsed_ms": int((time.time() - started) * 1000),
                }
            )
    result["finished_at"] = _ts()
    _write_result(config, result)
    return result


def _seed_cookies(session: requests.Session, cookies: list[dict[str, Any]]) -> None:
    for cookie in cookies:
        name = str(cookie.get("name") or "")
        value = str(cookie.get("value") or "")
        domain = str(cookie.get("domain") or "")
        path = str(cookie.get("path") or "/")
        if not name or not value:
            continue
        kwargs: dict[str, Any] = {"path": path}
        if domain:
            kwargs["domain"] = domain.lstrip(".")
        session.cookies.set(name, value, **kwargs)


def _write_result(config: ProtocolReplayConfig, result: dict[str, Any]) -> None:
    output_path = config.output_path
    if not output_path:
        source = Path(config.recording_path)
        output_path = str(source.with_name(f"{source.stem}_protocol_replay.json"))
    Path(output_path).write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    result["output_path"] = output_path


def _ts() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime())
