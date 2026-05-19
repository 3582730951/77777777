#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import socket
import sys
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from typing import Iterable


DEFAULT_PORTS = (
    "9222-9235,"
    "10000-10080,"
    "20000-20100,"
    "30000-30100,"
    "35000-65535"
)


@dataclass(frozen=True)
class CdpEndpoint:
    host: str
    port: int
    http_url: str
    websocket_url: str
    browser: str
    protocol_version: str


def parse_port_ranges(value: str) -> list[int]:
    ports: set[int] = set()
    for raw_part in str(value or "").split(","):
        part = raw_part.strip()
        if not part:
            continue
        if "-" in part:
            start_text, end_text = part.split("-", 1)
            start = int(start_text.strip())
            end = int(end_text.strip())
            if start > end:
                start, end = end, start
            for port in range(start, end + 1):
                if 1 <= port <= 65535:
                    ports.add(port)
            continue
        port = int(part)
        if 1 <= port <= 65535:
            ports.add(port)
    return sorted(ports)


def discover_cdp_endpoints(
    *,
    host: str = "127.0.0.1",
    ports: Iterable[int],
    timeout: float = 0.25,
    workers: int = 256,
) -> list[CdpEndpoint]:
    port_list = list(ports)
    if not port_list:
        return []
    worker_count = min(max(int(workers or 1), 1), len(port_list))
    endpoints: list[CdpEndpoint] = []
    with ThreadPoolExecutor(max_workers=worker_count) as executor:
        futures = [executor.submit(probe_cdp_endpoint, host, port, timeout) for port in port_list]
        for future in as_completed(futures):
            endpoint = future.result()
            if endpoint is not None:
                endpoints.append(endpoint)
    return sorted(endpoints, key=lambda item: item.port)


def probe_cdp_endpoint(host: str, port: int, timeout: float = 0.25) -> CdpEndpoint | None:
    if not _tcp_connects(host, port, timeout):
        return None
    http_url = f"http://{host}:{port}"
    try:
        request = urllib.request.Request(
            f"{http_url}/json/version",
            headers={"Accept": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = response.read().decode("utf-8", errors="replace")
    except (OSError, urllib.error.URLError, TimeoutError):
        return None
    try:
        data = json.loads(body)
    except ValueError:
        return None
    browser = str(data.get("Browser") or "")
    protocol_version = str(data.get("Protocol-Version") or "")
    websocket_url = str(data.get("webSocketDebuggerUrl") or "")
    if not browser and not websocket_url:
        return None
    return CdpEndpoint(
        host=host,
        port=port,
        http_url=http_url,
        websocket_url=websocket_url,
        browser=browser,
        protocol_version=protocol_version,
    )


def _tcp_connects(host: str, port: int, timeout: float) -> bool:
    try:
        with socket.create_connection((host, port), timeout=timeout):
            return True
    except OSError:
        return False


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Find local Chrome/BitBrowser CDP endpoints.")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--ports", default=DEFAULT_PORTS, help="Comma-separated ports/ranges, for example 9222,30000-65535")
    parser.add_argument("--timeout", type=float, default=0.25)
    parser.add_argument("--workers", type=int, default=256)
    parser.add_argument("--json", action="store_true", help="Print JSON instead of copyable commands")
    args = parser.parse_args(argv)

    ports = parse_port_ranges(args.ports)
    endpoints = discover_cdp_endpoints(
        host=args.host,
        ports=ports,
        timeout=args.timeout,
        workers=args.workers,
    )
    if args.json:
        print(json.dumps([endpoint.__dict__ for endpoint in endpoints], ensure_ascii=False, indent=2))
    else:
        if not endpoints:
            print("No CDP endpoint found.")
            return 2
        for endpoint in endpoints:
            print(f"{endpoint.http_url}  {endpoint.browser}".rstrip())
            print(
                "record command: "
                f"python scripts\\record_external_flow.py --cdp-url \"{endpoint.http_url}\" "
                "--no-navigate --output-dir data\\recordings\\bit_real"
            )
            if endpoint.websocket_url:
                print(f"websocket: {endpoint.websocket_url}")
            print()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
