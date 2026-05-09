import json
import stat

from api import kiro_gateway
from platforms.kiro.switch import _atomic_write


def test_kiro_gateway_token_preview_never_returns_short_secret():
    assert kiro_gateway._mask_token("") == ""
    assert kiro_gateway._mask_token("short-token") == "***"
    assert kiro_gateway._mask_token("1234567890abcdefXYZ") == "12345678...defXYZ"


def test_kiro_gateway_credentials_are_written_atomically_with_private_mode(tmp_path, monkeypatch):
    credentials_file = tmp_path / "kiro-gateway" / "credentials.json"
    monkeypatch.setattr(kiro_gateway, "CREDENTIALS_FILE", str(credentials_file))

    creds = [
        {
            "type": "refresh_token",
            "refresh_token": "rt-secret",
            "profile_arn": "profile-arn-1",
            "enabled": True,
        }
    ]
    kiro_gateway._write_credentials(creds)

    assert json.loads(credentials_file.read_text()) == creds
    assert stat.S_IMODE(credentials_file.stat().st_mode) == 0o600
    assert kiro_gateway._read_credentials() == creds
    assert list(credentials_file.parent.glob(".credentials-*.tmp")) == []


def test_kiro_desktop_atomic_write_uses_private_mode(tmp_path):
    token_file = tmp_path / "kiro-auth-token.json"

    _atomic_write(str(token_file), '{"refreshToken":"rt"}')

    assert token_file.read_text() == '{"refreshToken":"rt"}'
    assert stat.S_IMODE(token_file.stat().st_mode) == 0o600
    assert list(tmp_path.glob("*.tmp")) == []
