from __future__ import annotations

import json
from unittest.mock import MagicMock, patch

from core.proxy_providers import KookeeyProxyProvider, create_proxy_provider
from providers.proxy.kookeey import normalize_kookeey_proxy, parse_kookeey_proxy_response
from scripts import fetch_kookeey_proxy


def _response(text: str) -> MagicMock:
    resp = MagicMock()
    resp.text = text
    resp.raise_for_status = lambda: None
    return resp


def test_parse_literal_crlf_and_infer_socks5_from_api_url():
    provider = KookeeyProxyProvider(
        api_url="https://www.kookeey.com/pickdynamicips?p=socks5&format=4&dl=\\r\\n",
    )
    with patch("core.proxy_providers.requests.get", return_value=_response("1.2.3.4:1080\\r\\n5.6.7.8:2080")):
        assert provider.get_proxy() == "socks5://1.2.3.4:1080"
        assert provider.get_proxy() == "socks5://5.6.7.8:2080"


def test_parse_plain_json_shapes():
    text = json.dumps({"data": {"list": ["1.2.3.4:1080", "5.6.7.8:2080"]}})
    assert parse_kookeey_proxy_response(text, protocol="socks5") == [
        "socks5://1.2.3.4:1080",
        "socks5://5.6.7.8:2080",
    ]


def test_normalize_auth_variants():
    assert normalize_kookeey_proxy("u:p@1.2.3.4:1080", protocol="socks5") == "socks5://u:p@1.2.3.4:1080"
    assert normalize_kookeey_proxy("1.2.3.4:1080:u:p", protocol="socks5") == "socks5://u:p@1.2.3.4:1080"
    assert normalize_kookeey_proxy("1.2.3.4|1080|u|p", protocol="socks5") == "socks5://u:p@1.2.3.4:1080"


def test_config_username_password_used_when_response_has_no_auth():
    proxies = parse_kookeey_proxy_response(
        "1.2.3.4:1080",
        protocol="socks5",
        username="user name",
        password="p@ss",
    )
    assert proxies == ["socks5://user%20name:p%40ss@1.2.3.4:1080"]


def test_create_proxy_provider_supports_kookeey_dynamic():
    provider = create_proxy_provider(
        "kookeey_dynamic",
        {"kookeey_api_url": "https://www.kookeey.com/pickdynamicips?p=socks5"},
    )
    assert isinstance(provider, KookeeyProxyProvider)


def test_fetch_cli_accepts_positional_api_url(monkeypatch, capsys):
    class DummyProvider:
        def __init__(self, **kwargs):
            assert kwargs["api_url"] == "https://www.kookeey.com/pickdynamicips?p=socks5"
            assert kwargs["protocol"] == "auto"

        def get_proxy(self):
            return "socks5://1.2.3.4:1080"

    monkeypatch.setattr(fetch_kookeey_proxy, "KookeeyProxyProvider", DummyProvider)

    assert fetch_kookeey_proxy.main(["https://www.kookeey.com/pickdynamicips?p=socks5"]) == 0

    assert capsys.readouterr().out.strip() == "socks5://1.2.3.4:1080"
