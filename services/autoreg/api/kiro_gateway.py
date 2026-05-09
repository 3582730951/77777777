"""Kiro Gateway Token 管理 API。

管理 kiro-gateway 的 credentials.json，支持添加/删除/列出 refreshToken。
"""
from __future__ import annotations

import json
import os
import signal
import subprocess
import tempfile
from typing import Optional

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

router = APIRouter(prefix="/kiro-gateway", tags=["kiro-gateway"])

KIRO_GATEWAY_DIR = os.environ.get(
    "KIRO_GATEWAY_DIR",
    os.path.join(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))), "kiro-gateway"),
)
CREDENTIALS_FILE = os.path.join(KIRO_GATEWAY_DIR, "credentials.json")
KIRO_GATEWAY_PORT = int(os.environ.get("KIRO_GATEWAY_PORT", "18923"))


def _read_credentials() -> list[dict]:
    if not os.path.exists(CREDENTIALS_FILE):
        return []
    try:
        with open(CREDENTIALS_FILE, "r") as f:
            data = json.load(f)
        return data if isinstance(data, list) else []
    except Exception:
        return []


def _write_credentials(creds: list[dict]) -> None:
    directory = os.path.dirname(CREDENTIALS_FILE)
    os.makedirs(directory, exist_ok=True)
    fd, tmp_path = tempfile.mkstemp(prefix=".credentials-", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w") as f:
            fd = -1
            json.dump(creds, f, indent=2, ensure_ascii=False)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp_path, 0o600)
        os.replace(tmp_path, CREDENTIALS_FILE)
        os.chmod(CREDENTIALS_FILE, 0o600)
    except Exception:
        if fd >= 0:
            try:
                os.close(fd)
            except OSError:
                pass
        try:
            os.unlink(tmp_path)
        except OSError:
            pass
        raise


def _mask_token(token: str) -> str:
    if not token:
        return ""
    if len(token) <= 16:
        return "***"
    return f"{token[:8]}...{token[-6:]}"


def _find_kiro_gateway_pid() -> Optional[int]:
    try:
        result = subprocess.run(
            ["pgrep", "-f", f"kiro.*gateway.*{KIRO_GATEWAY_PORT}"],
            capture_output=True, text=True, timeout=5,
        )
        pids = result.stdout.strip().split("\n")
        return int(pids[0]) if pids and pids[0] else None
    except Exception:
        return None


class AddTokenRequest(BaseModel):
    refresh_token: str = Field(..., description="Kiro refreshToken")
    label: str = Field("", description="账号标签（如邮箱）")
    region: str = Field("us-east-1", description="AWS 区域")
    profile_arn: str = Field("", description="profileArn（可选，自动检测）")
    auth_type: str = Field("desktop", description="认证类型: desktop 或 sso")


class UpdateTokenRequest(BaseModel):
    refresh_token: str = Field(..., description="新的 refreshToken")


@router.get("/tokens")
def list_tokens():
    """列出所有已配置的 Kiro token。"""
    creds = _read_credentials()
    items = []
    for i, c in enumerate(creds):
        token = c.get("refresh_token", "")
        items.append({
            "index": i,
            "type": c.get("type", "refresh_token"),
            "label": c.get("label", c.get("comment", "")),
            "region": c.get("region", "us-east-1"),
            "profile_arn": c.get("profile_arn", ""),
            "enabled": c.get("enabled", True),
            "token_preview": _mask_token(token),
            "has_token": bool(token),
        })
    gateway_pid = _find_kiro_gateway_pid()
    return {
        "items": items,
        "total": len(items),
        "gateway_running": gateway_pid is not None,
        "gateway_pid": gateway_pid,
        "gateway_port": KIRO_GATEWAY_PORT,
        "credentials_file": CREDENTIALS_FILE,
    }


@router.post("/tokens")
def add_token(req: AddTokenRequest):
    """添加一个 Kiro refreshToken。"""
    if not req.refresh_token.strip():
        raise HTTPException(400, "refreshToken 不能为空")

    creds = _read_credentials()
    entry = {
        "type": "refresh_token",
        "refresh_token": req.refresh_token.strip(),
        "region": req.region or "us-east-1",
    }
    if req.label:
        entry["label"] = req.label
        entry["comment"] = req.label
    if req.profile_arn:
        entry["profile_arn"] = req.profile_arn
    entry["enabled"] = True

    creds.append(entry)
    _write_credentials(creds)

    return {
        "ok": True,
        "message": f"已添加 token（共 {len(creds)} 个）",
        "index": len(creds) - 1,
        "token_preview": _mask_token(req.refresh_token),
        "hint": "请重启 kiro-gateway 使配置生效",
    }


@router.put("/tokens/{index}")
def update_token(index: int, req: UpdateTokenRequest):
    """更新指定索引的 token。"""
    creds = _read_credentials()
    if index < 0 or index >= len(creds):
        raise HTTPException(404, f"索引 {index} 不存在")
    creds[index]["refresh_token"] = req.refresh_token.strip()
    _write_credentials(creds)
    return {"ok": True, "message": "已更新", "token_preview": _mask_token(req.refresh_token)}


@router.delete("/tokens/{index}")
def delete_token(index: int):
    """删除指定索引的 token。"""
    creds = _read_credentials()
    if index < 0 or index >= len(creds):
        raise HTTPException(404, f"索引 {index} 不存在")
    removed = creds.pop(index)
    _write_credentials(creds)
    return {"ok": True, "message": "已删除", "removed_label": removed.get("label", "")}


@router.post("/tokens/{index}/toggle")
def toggle_token(index: int):
    """启用/禁用指定索引的 token。"""
    creds = _read_credentials()
    if index < 0 or index >= len(creds):
        raise HTTPException(404, f"索引 {index} 不存在")
    current = creds[index].get("enabled", True)
    creds[index]["enabled"] = not current
    _write_credentials(creds)
    return {"ok": True, "enabled": not current}


@router.post("/restart")
def restart_gateway():
    """重启 kiro-gateway 服务。"""
    pid = _find_kiro_gateway_pid()
    if pid:
        try:
            os.kill(pid, signal.SIGTERM)
        except Exception:
            pass

    creds = _read_credentials()
    if not creds:
        return {"ok": False, "message": "credentials.json 为空，无法启动 kiro-gateway"}

    venv_python = os.path.join(KIRO_GATEWAY_DIR, ".venv", "bin", "python")
    main_py = os.path.join(KIRO_GATEWAY_DIR, "main.py")
    if not os.path.exists(venv_python):
        venv_python = "python3"
    if not os.path.exists(main_py):
        return {"ok": False, "message": f"kiro-gateway 未找到: {main_py}"}

    try:
        proc = subprocess.Popen(
            [venv_python, main_py, "--port", str(KIRO_GATEWAY_PORT), "--host", "127.0.0.1"],
            cwd=KIRO_GATEWAY_DIR,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        return {
            "ok": True,
            "message": f"kiro-gateway 已启动 (PID={proc.pid})",
            "pid": proc.pid,
            "port": KIRO_GATEWAY_PORT,
        }
    except Exception as e:
        return {"ok": False, "message": f"启动失败: {e}"}


@router.get("/status")
def gateway_status():
    """检查 kiro-gateway 运行状态。"""
    import requests
    pid = _find_kiro_gateway_pid()
    healthy = False
    models = []
    if pid:
        try:
            r = requests.get(
                f"http://127.0.0.1:{KIRO_GATEWAY_PORT}/health",
                timeout=3,
            )
            healthy = r.status_code == 200
        except Exception:
            pass
        try:
            r = requests.get(
                f"http://127.0.0.1:{KIRO_GATEWAY_PORT}/v1/models",
                headers={"Authorization": f"Bearer {os.environ.get('KIRO_GATEWAY_KEY', 'sk-kiro-gateway-internal')}"},
                timeout=3,
            )
            if r.status_code == 200:
                data = r.json()
                models = [m.get("id", "") for m in data.get("data", [])]
        except Exception:
            pass

    creds = _read_credentials()
    return {
        "running": pid is not None,
        "pid": pid,
        "healthy": healthy,
        "port": KIRO_GATEWAY_PORT,
        "accounts": len(creds),
        "enabled_accounts": sum(1 for c in creds if c.get("enabled", True)),
        "models": models,
    }
