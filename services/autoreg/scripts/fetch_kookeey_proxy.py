#!/usr/bin/env python3
from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from providers.proxy.kookeey import (  # noqa: E402
    KookeeyProxyProvider,
    parse_kookeey_proxy_response,
)


def _self_test() -> None:
    sample = "1.2.3.4:1080\\r\\nuser:pass@5.6.7.8:2080"
    proxies = parse_kookeey_proxy_response(sample, protocol="socks5")
    assert proxies == [
        "socks5://1.2.3.4:1080",
        "socks5://user:pass@5.6.7.8:2080",
    ]

    sample_json = '{"data":["9.9.9.9:9000:u:p"]}'
    assert parse_kookeey_proxy_response(sample_json, protocol="http") == [
        "http://u:p@9.9.9.9:9000"
    ]
    print("All tests passed")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Fetch and normalize Kookeey dynamic proxies.")
    parser.add_argument("api_url", nargs="?", default="", help="Kookeey pickdynamicips URL")
    parser.add_argument("--url", default="", help="Kookeey pickdynamicips URL; overrides positional URL")
    parser.add_argument("--protocol", default=os.getenv("KOOKEEY_PROXY_PROTOCOL", "auto"), help="auto/http/https/socks4/socks5")
    parser.add_argument("--username", default=os.getenv("KOOKEEY_PROXY_USERNAME", ""), help="Optional proxy username")
    parser.add_argument("--password", default=os.getenv("KOOKEEY_PROXY_PASSWORD", ""), help="Optional proxy password")
    parser.add_argument("--timeout", type=int, default=int(os.getenv("KOOKEEY_PROXY_TIMEOUT", "10")), help="Request timeout seconds")
    parser.add_argument("--count", type=int, default=1, help="Number of proxies to print")
    parser.add_argument("--self-test", action="store_true", help="Run parser self-test and exit")
    args = parser.parse_args(argv)

    if args.self_test:
        _self_test()
        return 0

    api_url = args.url or args.api_url or os.getenv("KOOKEEY_PROXY_API_URL", "")
    if not api_url:
        parser.error("URL is required as a positional argument, --url, or KOOKEEY_PROXY_API_URL")

    provider = KookeeyProxyProvider(
        api_url=api_url,
        protocol=args.protocol,
        username=args.username,
        password=args.password,
        timeout=args.timeout,
    )
    wanted = max(int(args.count or 1), 1)
    emitted = 0
    for _ in range(wanted):
        proxy = provider.get_proxy()
        if not proxy:
            break
        print(proxy)
        emitted += 1

    return 0 if emitted else 2


if __name__ == "__main__":
    raise SystemExit(main())
