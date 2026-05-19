"""ChatGPT Microsoft OAuth registration + PayPal checkout E2E flow."""
from __future__ import annotations

import json
import random
import string
import time
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Callable, Protocol

from platforms.chatgpt.auto_upgrade import UpgradeConfig, parse_upgrade_config


LogFn = Callable[[str], None]


@dataclass
class MicrosoftEmailAccount:
    email: str
    password: str = ""
    provider: str = "microsoft"
    metadata: dict = field(default_factory=dict)


@dataclass
class ChatGPTMicrosoftPaypalFlowConfig:
    dry_run: bool = True
    persist: bool = True
    headless: bool = True
    max_attempts: int = 5
    proxy: str = ""
    kookeey_api_url: str = ""
    proxy_protocol: str = "auto"
    proxy_timeout: int = 10
    microsoft_email: str = ""
    microsoft_password: str = ""
    microsoft_email_domain: str = "outlook.com"
    microsoft_email_prefix_length: int = 12
    microsoft_account_api_url: str = ""
    microsoft_account_api_token: str = ""
    chatgpt_password: str = ""
    payurl_base: str = "https://payurl.779.chat"
    plan: str = "plus"
    card_info: str = ""
    paypal_info: str = ""
    registration_timeout: int = 300


@dataclass
class ChatGPTSessionSnapshot:
    access_token: str = ""
    refresh_token: str = ""
    id_token: str = ""
    session_token: str = ""
    cookies: str = ""
    workspace_id: str = ""

    def as_dict(self) -> dict:
        return {
            "access_token": self.access_token,
            "refresh_token": self.refresh_token,
            "id_token": self.id_token,
            "session_token": self.session_token,
            "cookies": self.cookies,
            "workspace_id": self.workspace_id,
        }


@dataclass
class ChatGPTMicrosoftPaypalFlowResult:
    ok: bool
    dry_run: bool
    email: str = ""
    account_id: int = 0
    proxy: str = ""
    session: dict = field(default_factory=dict)
    payment: dict = field(default_factory=dict)
    attempts: list[dict] = field(default_factory=list)
    logs: list[str] = field(default_factory=list)
    error: str = ""


@dataclass
class LocalPlatformAccount:
    platform: str
    email: str
    password: str
    user_id: str = ""
    region: str = ""
    token: str = ""
    status: str = "registered"
    trial_end_time: int = 0
    extra: dict = field(default_factory=dict)
    created_at: int = field(default_factory=lambda: int(time.time()))


class MicrosoftEmailProvider(Protocol):
    def acquire(self) -> MicrosoftEmailAccount:
        ...


class StaticMicrosoftEmailProvider:
    def __init__(self, email: str, password: str = ""):
        self.email = email
        self.password = password

    def acquire(self) -> MicrosoftEmailAccount:
        email = self.email.strip()
        if not email:
            raise RuntimeError("缺少 Microsoft 邮箱；请传入 microsoft_email 或配置 microsoft_account_api_url")
        return MicrosoftEmailAccount(email=email, password=self.password, metadata={"source": "static"})


class GeneratedMicrosoftEmailProvider:
    """Generate random Microsoft mailbox material without external signup."""

    def __init__(self, domain: str = "outlook.com", password: str = "", prefix_length: int = 12):
        self.domain = str(domain or "outlook.com").strip().lstrip("@")
        self.password = password
        self.prefix_length = max(int(prefix_length or 12), 6)
        self._generated: set[str] = set()

    def acquire(self) -> MicrosoftEmailAccount:
        alphabet = string.ascii_lowercase
        for _ in range(100):
            prefix = "".join(random.choice(alphabet) for _ in range(self.prefix_length))
            if prefix not in self._generated:
                self._generated.add(prefix)
                return MicrosoftEmailAccount(
                    email=f"{prefix}@{self.domain}",
                    password=self.password,
                    metadata={"source": "generated_prefix"},
                )
        raise RuntimeError("随机 Microsoft 邮箱前缀生成失败")


class HttpMicrosoftEmailProvider:
    """Acquire a test Microsoft mailbox from an internal provider API.

    Expected response shapes:
      - ``{"email": "...", "password": "..."}``
      - ``{"data": {"email": "...", "password": "..."}}``
    """

    def __init__(self, api_url: str, token: str = "", timeout: int = 30):
        self.api_url = api_url.strip()
        self.token = token.strip()
        self.timeout = int(timeout or 30)

    def acquire(self) -> MicrosoftEmailAccount:
        if not self.api_url:
            raise RuntimeError("缺少 Microsoft 邮箱 API URL")
        request = urllib.request.Request(self.api_url)
        if self.token:
            request.add_header("Authorization", f"Bearer {self.token}")
        with urllib.request.urlopen(request, timeout=self.timeout) as response:
            payload = json.loads(response.read().decode("utf-8"))
        data = payload.get("data") if isinstance(payload, dict) else None
        if isinstance(data, dict):
            payload = data
        if not isinstance(payload, dict):
            raise RuntimeError("Microsoft 邮箱 API 响应格式异常")
        email = str(payload.get("email") or payload.get("mail") or "").strip()
        password = str(payload.get("password") or payload.get("pass") or "").strip()
        if not email:
            raise RuntimeError("Microsoft 邮箱 API 未返回 email")
        metadata = {
            key: value
            for key, value in payload.items()
            if key not in {"email", "mail", "password", "pass"}
        }
        metadata["source"] = "http_api"
        return MicrosoftEmailAccount(email=email, password=password, metadata=metadata)


class DryRunPaymentRunner:
    def run(self, access_token: str, config: UpgradeConfig, proxy: str, log_fn: LogFn, headless: bool) -> dict:
        log_fn("dry-run: 跳过 payurl/Stripe/PayPal 实际请求")
        return {
            "success": True,
            "stripe_url": "https://checkout.stripe.com/c/pay/cs_test_dry_run",
            "chatgpt_url": "https://chatgpt.com/checkout/openai_llc/cs_test_dry_run",
            "logs": ["dry-run payment flow completed"],
        }


class LivePaymentRunner:
    def run(self, access_token: str, config: UpgradeConfig, proxy: str, log_fn: LogFn, headless: bool) -> dict:
        from platforms.chatgpt.auto_upgrade import auto_upgrade_with_browser

        result = auto_upgrade_with_browser(
            access_token=access_token,
            config=config,
            proxy=proxy or None,
            log_fn=log_fn,
            headless=headless,
        )
        return {
            "success": bool(result.success),
            "stripe_url": result.stripe_url,
            "chatgpt_url": result.chatgpt_url,
            "logs": list(result.logs or []),
            "error": result.error,
        }


def _append_log(logs: list[str], outer_log: LogFn | None, message: str) -> None:
    logs.append(message)
    if outer_log:
        outer_log(message)


def _resolve_microsoft_provider(config: ChatGPTMicrosoftPaypalFlowConfig) -> MicrosoftEmailProvider:
    if config.microsoft_account_api_url:
        return HttpMicrosoftEmailProvider(
            config.microsoft_account_api_url,
            token=config.microsoft_account_api_token,
            timeout=config.registration_timeout,
        )
    if not config.microsoft_email:
        return GeneratedMicrosoftEmailProvider(
            domain=config.microsoft_email_domain,
            password=config.microsoft_password,
            prefix_length=config.microsoft_email_prefix_length,
        )
    return StaticMicrosoftEmailProvider(config.microsoft_email, config.microsoft_password)


def _resolve_proxy(config: ChatGPTMicrosoftPaypalFlowConfig, log: LogFn, used_proxies: set[str] | None = None) -> str:
    used = used_proxies if used_proxies is not None else set()
    if config.proxy:
        return config.proxy
    if config.kookeey_api_url:
        from providers.proxy.kookeey import KookeeyProxyProvider

        for _ in range(max(config.max_attempts, 1) + 5):
            proxy = KookeeyProxyProvider(
                api_url=config.kookeey_api_url,
                protocol=config.proxy_protocol,
                timeout=config.proxy_timeout,
            ).get_proxy()
            if not proxy:
                break
            if proxy in used:
                log(f"Kookeey 动态代理重复，跳过: {_mask_proxy(proxy)}")
                continue
            used.add(proxy)
            log(f"Kookeey 动态代理已获取: {_mask_proxy(proxy)}")
            return proxy
        raise RuntimeError("未获取到未使用过的 Kookeey 动态代理")
    if config.dry_run:
        return ""
    try:
        from core.proxy_pool import proxy_pool

        for _ in range(max(config.max_attempts, 1) + 5):
            proxy = proxy_pool.get_next()
            if not proxy:
                return ""
            if proxy in used:
                log(f"动态代理重复，跳过: {_mask_proxy(proxy)}")
                continue
            used.add(proxy)
            return proxy or ""
        raise RuntimeError("未获取到未使用过的动态代理")
    except Exception:
        return ""


def _mask_proxy(proxy: str) -> str:
    text = str(proxy or "")
    if "@" not in text:
        return text
    prefix, _, host = text.rpartition("@")
    scheme, _, _auth = prefix.partition("://")
    return f"{scheme}://***@{host}" if scheme else f"***@{host}"


def _attach_microsoft_identity(account: Any, microsoft: MicrosoftEmailAccount) -> Any:
    extra = dict(account.extra or {})
    metadata = dict(microsoft.metadata or {})
    provider_account = {
        "provider_type": "mailbox",
        "provider_name": "microsoft",
        "login_identifier": microsoft.email,
        "display_name": microsoft.email,
        "credentials": {"password": microsoft.password} if microsoft.password else {},
        "metadata": metadata,
    }
    provider_resource = {
        "provider_type": "mailbox",
        "provider_name": "microsoft",
        "resource_type": "mailbox",
        "resource_identifier": microsoft.email,
        "handle": microsoft.email,
        "display_name": microsoft.email,
        "metadata": metadata,
    }
    extra["identity"] = {
        **dict(extra.get("identity") or {}),
        "identity_provider": "oauth_browser",
        "oauth_provider": "microsoft",
        "resolved_email": microsoft.email,
        "metadata": {
            **dict((extra.get("identity") or {}).get("metadata") or {}),
            "microsoft_mailbox": {"email": microsoft.email, **metadata},
        },
    }
    extra["verification_mailbox"] = {
        "provider": "microsoft",
        "email": microsoft.email,
        "account_id": microsoft.email,
    }
    provider_accounts = list(extra.get("provider_accounts") or [])
    provider_accounts.append(provider_account)
    extra["provider_accounts"] = provider_accounts
    provider_resources = list(extra.get("provider_resources") or [])
    provider_resources.append(provider_resource)
    extra["provider_resources"] = provider_resources
    account.extra = extra
    return account


def _session_from_account(account: Any) -> ChatGPTSessionSnapshot:
    extra = dict(account.extra or {})
    return ChatGPTSessionSnapshot(
        access_token=str(extra.get("access_token") or account.token or ""),
        refresh_token=str(extra.get("refresh_token") or ""),
        id_token=str(extra.get("id_token") or ""),
        session_token=str(extra.get("session_token") or ""),
        cookies=str(extra.get("cookies") or ""),
        workspace_id=str(extra.get("workspace_id") or ""),
    )


def _dry_run_account(microsoft: MicrosoftEmailAccount, password: str = "") -> LocalPlatformAccount:
    now = int(time.time())
    return LocalPlatformAccount(
        platform="chatgpt",
        email=microsoft.email,
        password=password or microsoft.password,
        user_id=f"dry-run-{now}",
        token="dry-run-access-token",
        status="registered",
        extra={
            "access_token": "dry-run-access-token",
            "refresh_token": "dry-run-refresh-token",
            "id_token": "dry-run-id-token",
            "session_token": "dry-run-session-token",
            "cookies": "__Secure-next-auth.session-token=dry-run-session-token",
            "workspace_id": "dry-run-workspace",
            "account_overview": {
                "flow": "chatgpt_microsoft_paypal",
                "dry_run": True,
                "valid": True,
            },
        },
    )


def _register_chatgpt_account(
    microsoft: MicrosoftEmailAccount,
    config: ChatGPTMicrosoftPaypalFlowConfig,
    proxy: str,
    log: LogFn,
) -> Any:
    if config.dry_run:
        log("dry-run: 生成模拟 ChatGPT 注册结果和 /api/auth/session 快照")
        return _attach_microsoft_identity(_dry_run_account(microsoft, config.chatgpt_password), microsoft)

    from core.base_platform import RegisterConfig
    from platforms.chatgpt.plugin import ChatGPTPlatform

    platform_config = RegisterConfig(
        executor_type="headless" if config.headless else "headed",
        proxy=proxy or None,
        extra={
            "identity_provider": "oauth_browser",
            "oauth_provider": "microsoft",
            "oauth_email_hint": microsoft.email,
            "browser_oauth_timeout": config.registration_timeout,
        },
    )
    platform = ChatGPTPlatform(config=platform_config)
    platform.set_logger(log)
    account = platform.register(email=microsoft.email, password=config.chatgpt_password or microsoft.password)
    return _attach_microsoft_identity(account, microsoft)


def _persist_account(account: Any) -> int:
    from core.db import save_account

    model = save_account(account)
    return int(getattr(model, "id", 0) or 0)


def run_chatgpt_microsoft_paypal_flow(
    config: ChatGPTMicrosoftPaypalFlowConfig,
    *,
    microsoft_provider: MicrosoftEmailProvider | None = None,
    payment_runner: DryRunPaymentRunner | LivePaymentRunner | None = None,
    log_fn: LogFn | None = None,
) -> ChatGPTMicrosoftPaypalFlowResult:
    logs: list[str] = []
    attempts: list[dict] = []
    used_proxies: set[str] = set()

    def log(message: str) -> None:
        _append_log(logs, log_fn, message)

    provider = microsoft_provider or _resolve_microsoft_provider(config)
    runner = payment_runner or (DryRunPaymentRunner() if config.dry_run else LivePaymentRunner())
    max_attempts = max(int(config.max_attempts or 1), 1)

    for attempt_no in range(1, max_attempts + 1):
        attempt: dict = {"attempt": attempt_no}
        try:
            log(f"开始第 {attempt_no}/{max_attempts} 次 ChatGPT/PayPal 流程")
            microsoft = provider.acquire()
            attempt["email"] = microsoft.email
            log(f"Microsoft 测试邮箱已就绪: {microsoft.email}")

            proxy = _resolve_proxy(config, log, used_proxies)
            attempt["proxy"] = proxy
            if proxy:
                log(f"注册代理: {_mask_proxy(proxy)}")

            account = _register_chatgpt_account(microsoft, config, proxy, log)
            session = _session_from_account(account)
            if not session.access_token:
                raise RuntimeError("ChatGPT 注册结果缺少 access_token，无法进入 payurl/Stripe 流程")
            log("ChatGPT session 已捕获")

            account_id = 0
            if config.persist:
                account_id = _persist_account(account)
                attempt["account_id"] = account_id
                log(f"账号已保存到本地账号池: {account_id}")

            upgrade_config = parse_upgrade_config(
                card_info=config.card_info,
                paypal_info=config.paypal_info,
                payurl_base=config.payurl_base,
                plan=config.plan,
            )
            payment = runner.run(
                access_token=session.access_token,
                config=upgrade_config,
                proxy=proxy,
                log_fn=log,
                headless=config.headless,
            )
            attempt["payment"] = payment
            if not payment.get("success"):
                stripe_url = str(payment.get("stripe_url") or "")
                if stripe_url:
                    log(f"支付失败，保留 Stripe 链接: {stripe_url}")
                raise RuntimeError(str(payment.get("error") or "payment flow failed"))

            attempt["ok"] = True
            attempts.append(attempt)
            return ChatGPTMicrosoftPaypalFlowResult(
                ok=True,
                dry_run=config.dry_run,
                email=microsoft.email,
                account_id=account_id,
                proxy=proxy,
                session=session.as_dict(),
                payment=payment,
                attempts=attempts,
                logs=logs,
            )
        except Exception as exc:
            attempt["ok"] = False
            attempt["error"] = str(exc)
            attempts.append(attempt)
            log(f"第 {attempt_no}/{max_attempts} 次失败: {exc}")
            if attempt_no < max_attempts:
                log("准备切换代理并使用新的 Microsoft 邮箱前缀重试")
                continue

    error = attempts[-1].get("error", "payment flow failed") if attempts else "payment flow failed"
    return ChatGPTMicrosoftPaypalFlowResult(
        ok=False,
        dry_run=config.dry_run,
        attempts=attempts,
        logs=logs,
        error=str(error),
    )


def run_chatgpt_microsoft_paypal_flow_once(
    config: ChatGPTMicrosoftPaypalFlowConfig,
    *,
    microsoft_provider: MicrosoftEmailProvider | None = None,
    payment_runner: DryRunPaymentRunner | LivePaymentRunner | None = None,
    log_fn: LogFn | None = None,
) -> ChatGPTMicrosoftPaypalFlowResult:
    single_attempt_config = ChatGPTMicrosoftPaypalFlowConfig(**{**config.__dict__, "max_attempts": 1})
    return run_chatgpt_microsoft_paypal_flow(
        single_attempt_config,
        microsoft_provider=microsoft_provider,
        payment_runner=payment_runner,
        log_fn=log_fn,
    )
