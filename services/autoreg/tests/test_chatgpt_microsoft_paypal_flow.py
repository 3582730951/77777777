from __future__ import annotations

from platforms.chatgpt.microsoft_paypal_flow import (
    ChatGPTMicrosoftPaypalFlowConfig,
    GeneratedMicrosoftEmailProvider,
    MicrosoftEmailAccount,
    StaticMicrosoftEmailProvider,
    run_chatgpt_microsoft_paypal_flow,
)


def test_dry_run_flow_captures_session_and_payment_without_network():
    config = ChatGPTMicrosoftPaypalFlowConfig(
        dry_run=True,
        persist=False,
        card_info=(
            "4242424242424242----2030/7----123----+18585550123----"
            "https://sms.example.test/3ds----TEST USER----1 Test St,Seattle WA 98101,US"
        ),
        paypal_info="+13502234731|https://sms.example.test/paypal",
    )

    result = run_chatgpt_microsoft_paypal_flow(
        config,
        microsoft_provider=StaticMicrosoftEmailProvider("qa.ms@example.test", "example-password"),
    )

    assert result.ok
    assert result.email == "qa.ms@example.test"
    assert result.session["access_token"] == "dry-run-access-token"
    assert result.session["session_token"] == "dry-run-session-token"
    assert result.payment["stripe_url"] == "https://checkout.stripe.com/c/pay/cs_test_dry_run"


def test_dry_run_flow_accepts_custom_provider():
    class Provider:
        def acquire(self):
            return MicrosoftEmailAccount("custom.ms@example.test", "secret", metadata={"batch": "qa"})

    config = ChatGPTMicrosoftPaypalFlowConfig(dry_run=True, persist=False)
    result = run_chatgpt_microsoft_paypal_flow(config, microsoft_provider=Provider())

    assert result.ok
    assert result.email == "custom.ms@example.test"


def test_generated_microsoft_provider_uses_random_prefix_domain():
    provider = GeneratedMicrosoftEmailProvider(domain="outlook.com", prefix_length=8)

    first = provider.acquire()
    second = provider.acquire()

    assert first.email.endswith("@outlook.com")
    assert second.email.endswith("@outlook.com")
    assert first.email != second.email
    assert first.email.split("@", 1)[0].isalpha()


def test_payment_failures_retry_with_new_generated_emails_and_return_stripe_links():
    class FailingRunner:
        def run(self, access_token, config, proxy, log_fn, headless):
            return {
                "success": False,
                "stripe_url": f"https://checkout.stripe.com/c/pay/{access_token}",
                "error": "declined",
            }

    config = ChatGPTMicrosoftPaypalFlowConfig(
        dry_run=True,
        persist=False,
        max_attempts=5,
        microsoft_email_domain="outlook.com",
        microsoft_email_prefix_length=8,
    )
    result = run_chatgpt_microsoft_paypal_flow(config, payment_runner=FailingRunner())

    assert not result.ok
    assert len(result.attempts) == 5
    assert len({attempt["email"] for attempt in result.attempts}) == 5
    assert all(attempt["payment"]["stripe_url"].startswith("https://checkout.stripe.com/") for attempt in result.attempts)
    assert result.error == "declined"
