#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.request
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from scripts.record_bitbrowser_flow import normalize_bitbrowser_api_base  # noqa: E402


def list_bitbrowser_profiles(
    *,
    api_base: str = "http://127.0.0.1:54345",
    page: int = 0,
    page_size: int = 100,
    name: str = "",
    remark: str = "",
    group_id: str = "",
    timeout: int = 30,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "page": int(page),
        "pageSize": int(page_size),
    }
    if name:
        payload["name"] = name
    if remark:
        payload["remark"] = remark
    if group_id:
        payload["groupId"] = group_id
    return _post_json(f"{normalize_bitbrowser_api_base(api_base)}/browser/list", payload, timeout=timeout)


def extract_profiles(payload: dict[str, Any]) -> list[dict[str, Any]]:
    candidates = [
        _nested_get(payload, ("data", "list")),
        _nested_get(payload, ("data", "items")),
        _nested_get(payload, ("data", "rows")),
        _nested_get(payload, ("data", "data")),
        payload.get("data") if isinstance(payload, dict) else None,
        payload.get("list") if isinstance(payload, dict) else None,
    ]
    for candidate in candidates:
        if isinstance(candidate, list):
            return [item for item in candidate if isinstance(item, dict)]
    return []


def profile_summary(profile: dict[str, Any]) -> dict[str, str]:
    return {
        "id": str(profile.get("id") or profile.get("browserId") or profile.get("browser_id") or ""),
        "seq": str(profile.get("seq") or profile.get("serialNum") or profile.get("serial") or ""),
        "name": str(profile.get("name") or profile.get("browserName") or ""),
        "remark": str(profile.get("remark") or profile.get("remarks") or ""),
        "groupId": str(profile.get("groupId") or profile.get("group_id") or ""),
        "status": str(profile.get("status") or profile.get("state") or ""),
    }


def _nested_get(value: Any, keys: tuple[str, ...]) -> Any:
    current = value
    for key in keys:
        if not isinstance(current, dict):
            return None
        current = current.get(key)
    return current


def _post_json(url: str, payload: dict[str, Any], *, timeout: int = 30) -> dict[str, Any]:
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
    parser = argparse.ArgumentParser(description="List BitBrowser profiles and print their ids.")
    parser.add_argument("--api", default=os.getenv("BITBROWSER_API_BASE", "http://127.0.0.1:54345"))
    parser.add_argument("--page", type=int, default=0)
    parser.add_argument("--page-size", type=int, default=100)
    parser.add_argument("--name", default="")
    parser.add_argument("--remark", default="")
    parser.add_argument("--group-id", default="")
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args(argv)

    payload = list_bitbrowser_profiles(
        api_base=args.api,
        page=args.page,
        page_size=args.page_size,
        name=args.name,
        remark=args.remark,
        group_id=args.group_id,
    )
    profiles = [profile_summary(profile) for profile in extract_profiles(payload)]
    if args.json:
        print(json.dumps({"profiles": profiles, "raw": payload}, ensure_ascii=False, indent=2))
        return 0
    if not profiles:
        print("No BitBrowser profiles returned.")
        print(f"API: {normalize_bitbrowser_api_base(args.api)}/browser/list")
        return 2
    for profile in profiles:
        label = " | ".join(
            part
            for part in (
                f"id={profile['id']}" if profile["id"] else "",
                f"seq={profile['seq']}" if profile["seq"] else "",
                f"name={profile['name']}" if profile["name"] else "",
                f"remark={profile['remark']}" if profile["remark"] else "",
                f"groupId={profile['groupId']}" if profile["groupId"] else "",
                f"status={profile['status']}" if profile["status"] else "",
            )
            if part
        )
        print(label)
        if profile["id"]:
            print(
                "record command: "
                f"python scripts\\record_bitbrowser_flow.py --id \"{profile['id']}\" "
                "--url \"https://chatgpt.com/\" --output-dir data\\recordings\\bit_real"
            )
        print()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
