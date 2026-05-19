#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from platforms.chatgpt.microsoft_paypal_flow import (  # noqa: E402
    ChatGPTMicrosoftPaypalFlowConfig,
    run_chatgpt_microsoft_paypal_flow,
)


def _redact(value):
    if isinstance(value, dict):
        redacted = {}
        for key, item in value.items():
            lower = str(key).lower()
            if any(token in lower for token in ("token", "cookie", "password", "cvv", "sms_api")):
                text = str(item or "")
                redacted[key] = f"{text[:6]}...{text[-4:]}" if len(text) > 12 else "***" if text else ""
            else:
                redacted[key] = _redact(item)
        return redacted
    if isinstance(value, list):
        return [_redact(item) for item in value]
    return value


def _env_bool(name: str, default: bool) -> bool:
    raw = os.getenv(name, "")
    if raw == "":
        return default
    return raw.strip().lower() not in {"0", "false", "no", "off"}


def _config_from_args(args) -> ChatGPTMicrosoftPaypalFlowConfig:
    return ChatGPTMicrosoftPaypalFlowConfig(
        dry_run=not args.live,
        persist=not args.no_save,
        headless=not args.headed,
        max_attempts=args.attempts,
        proxy=args.proxy or os.getenv("CHATGPT_FLOW_PROXY", ""),
        kookeey_api_url=args.kookeey_url or os.getenv("KOOKEEY_PROXY_API_URL", ""),
        proxy_protocol=args.proxy_protocol or os.getenv("KOOKEEY_PROXY_PROTOCOL", "auto"),
        proxy_timeout=int(os.getenv("KOOKEEY_PROXY_TIMEOUT", "10")),
        microsoft_email=args.microsoft_email or os.getenv("MICROSOFT_EMAIL", ""),
        microsoft_password=args.microsoft_password or os.getenv("MICROSOFT_PASSWORD", ""),
        microsoft_email_domain=args.microsoft_domain or os.getenv("MICROSOFT_EMAIL_DOMAIN", "outlook.com"),
        microsoft_email_prefix_length=args.microsoft_prefix_length,
        microsoft_account_api_url=args.microsoft_api_url or os.getenv("MICROSOFT_ACCOUNT_API_URL", ""),
        microsoft_account_api_token=os.getenv("MICROSOFT_ACCOUNT_API_TOKEN", ""),
        chatgpt_password=args.chatgpt_password or os.getenv("CHATGPT_PASSWORD", ""),
        payurl_base=args.payurl_base or os.getenv("PAYURL_BASE_URL", "https://payurl.779.chat"),
        plan=args.plan or os.getenv("CHATGPT_PAYMENT_PLAN", "plus"),
        card_info=args.card_info or os.getenv("CHATGPT_CARD_INFO", ""),
        paypal_info=args.paypal_info or os.getenv("CHATGPT_PAYPAL_INFO", ""),
        registration_timeout=int(os.getenv("CHATGPT_REGISTRATION_TIMEOUT", "300")),
    )


def _self_test() -> None:
    config = ChatGPTMicrosoftPaypalFlowConfig(
        dry_run=True,
        persist=False,
        microsoft_email="qa.ms@example.test",
        microsoft_password="example-password",
        card_info=(
            "4242424242424242----2030/7----123----+18585550123----"
            "https://sms.example.test/3ds----TEST USER----1 Test St,Seattle WA 98101,US"
        ),
        paypal_info="+13502234731|https://sms.example.test/paypal",
    )
    result = run_chatgpt_microsoft_paypal_flow(config)
    assert result.ok, result.error
    assert result.session["access_token"] == "dry-run-access-token"
    assert result.payment["stripe_url"].startswith("https://checkout.stripe.com/")
    print("All tests passed")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run ChatGPT Microsoft OAuth + PayPal checkout E2E flow.")
    parser.add_argument("--live", action="store_true", help="Run real browser/network flow. Default is dry-run.")
    parser.add_argument("--headed", action="store_true", help="Use headed browser for live mode.")
    parser.add_argument("--no-save", action="store_true", default=not _env_bool("CHATGPT_FLOW_SAVE", True))
    parser.add_argument("--attempts", type=int, default=int(os.getenv("CHATGPT_FLOW_ATTEMPTS", "5")), help="Max payment retry attempts")
    parser.add_argument("--proxy", default="")
    parser.add_argument("--kookeey-url", default="")
    parser.add_argument("--proxy-protocol", default="")
    parser.add_argument("--microsoft-email", default="")
    parser.add_argument("--microsoft-password", default="")
    parser.add_argument("--microsoft-domain", default="")
    parser.add_argument("--microsoft-prefix-length", type=int, default=int(os.getenv("MICROSOFT_EMAIL_PREFIX_LENGTH", "12")))
    parser.add_argument("--microsoft-api-url", default="")
    parser.add_argument("--chatgpt-password", default="")
    parser.add_argument("--payurl-base", default="")
    parser.add_argument("--plan", default="plus")
    parser.add_argument("--card-info", default="")
    parser.add_argument("--paypal-info", default="")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args(argv)

    if args.self_test:
        _self_test()
        return 0

    config = _config_from_args(args)
    result = run_chatgpt_microsoft_paypal_flow(config, log_fn=print)
    print(json.dumps(_redact(result.__dict__), ensure_ascii=False, indent=2))
    return 0 if result.ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
