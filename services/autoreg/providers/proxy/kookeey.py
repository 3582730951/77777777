"""Kookeey dynamic proxy provider.

The Kookeey extraction API can return a plain text body, JSON, or a body
containing literal ``\r\n`` separators.  This provider normalizes those
responses into proxy URLs that Playwright/requests can consume directly.
"""
from __future__ import annotations

import json
import logging
import re
from typing import Any, Iterable, Optional
from urllib.parse import parse_qs, quote, urlparse
import urllib.request

import core.proxy_providers as proxy_providers
from core.proxy_providers import BaseProxyProvider
from providers.registry import register_provider

logger = logging.getLogger(__name__)

_SCHEME_RE = re.compile(r"^(?:https?|socks4|socks5|socks5h)://", re.IGNORECASE)
_VALID_SCHEMES = {"http", "https", "socks4", "socks5", "socks5h"}


def _normalize_protocol(protocol: str) -> str:
    value = str(protocol or "").strip().lower().replace("://", "")
    return value if value in _VALID_SCHEMES else "http"


def _infer_protocol(api_url: str, configured_protocol: str = "") -> str:
    configured = str(configured_protocol or "").strip().lower()
    if configured and configured != "auto":
        return _normalize_protocol(configured)
    try:
        query = parse_qs(urlparse(api_url).query)
    except Exception:
        query = {}
    for key in ("p", "protocol", "proxy_protocol"):
        raw = (query.get(key) or [""])[0]
        if raw:
            return _normalize_protocol(raw)
    return "http"


def _clean_piece(value: Any) -> str:
    return str(value or "").strip().strip("\"'` ,;\t\r\n")


def _host_port(host: str, port: str) -> tuple[str, int] | None:
    host = str(host or "").strip().strip("[]")
    port_text = str(port or "").strip()
    if not host or not port_text.isdigit():
        return None
    port_int = int(port_text)
    if port_int <= 0 or port_int > 65535:
        return None
    if "/" in host or "?" in host or "=" in host:
        return None
    return host, port_int


def _split_host_port(value: str) -> tuple[str, int] | None:
    raw = _clean_piece(value)
    if raw.startswith("[") and "]:" in raw:
        host, _, port = raw[1:].partition("]:")
        return _host_port(host, port)
    if ":" not in raw:
        return None
    host, _, port = raw.rpartition(":")
    return _host_port(host, port)


def _auth_prefix(username: str = "", password: str = "") -> str:
    user = str(username or "")
    pwd = str(password or "")
    if not user and not pwd:
        return ""
    return f"{quote(user, safe='')}:{quote(pwd, safe='')}@"


def normalize_kookeey_proxy(
    raw: Any,
    *,
    protocol: str = "http",
    username: str = "",
    password: str = "",
) -> str:
    """Normalize one raw proxy entry into ``scheme://[auth@]host:port``.

    Supported raw formats:
      - ``host:port``
      - ``user:pass@host:port``
      - ``host:port:user:pass``
      - ``host|port|user|pass``
      - already-normalized URLs such as ``socks5://host:port``
    """
    item = _clean_piece(raw)
    if not item:
        return ""

    if _SCHEME_RE.match(item):
        parsed = urlparse(item)
        if parsed.hostname and parsed.port:
            return item
        return ""

    scheme = _normalize_protocol(protocol)

    if "@" in item:
        auth, _, target = item.rpartition("@")
        parsed = _split_host_port(target)
        if parsed:
            host, port = parsed
            return f"{scheme}://{auth}@{host}:{port}"

    if "|" in item:
        parts = [_clean_piece(part) for part in item.split("|")]
        if len(parts) >= 2:
            parsed = _host_port(parts[0], parts[1])
            if parsed:
                host, port = parsed
                user = parts[2] if len(parts) >= 3 else username
                pwd = parts[3] if len(parts) >= 4 else password
                return f"{scheme}://{_auth_prefix(user, pwd)}{host}:{port}"

    parts = item.split(":")
    if len(parts) >= 4:
        parsed = _host_port(parts[0], parts[1])
        if parsed:
            host, port = parsed
            return f"{scheme}://{_auth_prefix(parts[2], parts[3])}{host}:{port}"
        parsed = _host_port(parts[-2], parts[-1])
        if parsed:
            host, port = parsed
            return f"{scheme}://{_auth_prefix(parts[0], parts[1])}{host}:{port}"

    parsed = _split_host_port(item)
    if parsed:
        host, port = parsed
        return f"{scheme}://{_auth_prefix(username, password)}{host}:{port}"

    return ""


def _json_candidates(value: Any) -> Iterable[Any]:
    if isinstance(value, list):
        for item in value:
            yield from _json_candidates(item)
        return
    if isinstance(value, dict):
        preferred = (
            "data",
            "proxies",
            "proxy_list",
            "list",
            "result",
            "rows",
            "items",
            "proxy",
            "ip",
        )
        yielded = False
        for key in preferred:
            if key in value:
                yielded = True
                yield from _json_candidates(value[key])
        if not yielded:
            for item in value.values():
                yield from _json_candidates(item)
        return
    yield value


def _text_candidates(text: str) -> list[str]:
    normalized = str(text or "")
    for escaped in ("\\r\\n", "\\n", "\\r"):
        normalized = normalized.replace(escaped, "\n")
    normalized = normalized.replace("\r\n", "\n").replace("\r", "\n")
    pieces: list[str] = []
    for line in normalized.split("\n"):
        line = line.strip()
        if not line:
            continue
        # Kookeey text responses are commonly line based.  Commas and
        # semicolons are also accepted for copy/paste and alternate APIs.
        pieces.extend(part for part in re.split(r"[,;]+", line) if part.strip())
    return pieces


def parse_kookeey_proxy_response(
    text: str,
    *,
    protocol: str = "http",
    username: str = "",
    password: str = "",
) -> list[str]:
    """Parse a Kookeey API response into normalized proxy URLs."""
    raw_text = str(text or "").strip()
    if not raw_text:
        return []

    candidates: list[Any] = []
    try:
        candidates.extend(_json_candidates(json.loads(raw_text)))
    except ValueError:
        candidates.extend(_text_candidates(raw_text))

    proxies: list[str] = []
    seen: set[str] = set()
    for candidate in candidates:
        if isinstance(candidate, str):
            pieces = _text_candidates(candidate)
        else:
            pieces = [candidate]
        for piece in pieces:
            proxy = normalize_kookeey_proxy(
                piece,
                protocol=protocol,
                username=username,
                password=password,
            )
            if proxy and proxy not in seen:
                seen.add(proxy)
                proxies.append(proxy)
    return proxies


@register_provider("proxy", "kookeey_dynamic")
class KookeeyProxyProvider(BaseProxyProvider):
    """Kookeey API extraction provider."""

    def __init__(
        self,
        *,
        api_url: str,
        protocol: str = "auto",
        username: str = "",
        password: str = "",
        timeout: int = 10,
    ):
        if not api_url:
            raise RuntimeError("Kookeey 动态代理未配置 API URL")
        self.api_url = api_url
        self.protocol = _infer_protocol(api_url, protocol)
        self.username = username
        self.password = password
        self.timeout = int(timeout or 10)
        self._cache: list[str] = []

    @classmethod
    def from_config(cls, config: dict) -> "KookeeyProxyProvider":
        api_url = config.get("kookeey_api_url") or config.get("proxy_api_url") or ""
        return cls(
            api_url=api_url,
            protocol=config.get("proxy_protocol", "auto"),
            username=config.get("proxy_username", ""),
            password=config.get("proxy_password", ""),
            timeout=int(config.get("proxy_timeout", 10) or 10),
        )

    def fetch_proxies(self) -> list[str]:
        try:
            text = self._fetch_text()
        except Exception as exc:
            logger.warning("[KookeeyProxyProvider] API 请求失败: %s", exc)
            return []

        return parse_kookeey_proxy_response(
            text,
            protocol=self.protocol,
            username=self.username,
            password=self.password,
        )

    def _fetch_text(self) -> str:
        try:
            resp = proxy_providers.requests.get(self.api_url, timeout=self.timeout)
            resp.raise_for_status()
            return str(resp.text or "")
        except ModuleNotFoundError:
            with urllib.request.urlopen(self.api_url, timeout=self.timeout) as response:
                return response.read().decode("utf-8", errors="replace")

    def get_proxy(self) -> Optional[str]:
        if not self._cache:
            self._cache = self.fetch_proxies()
        if self._cache:
            return self._cache.pop(0)
        return None
