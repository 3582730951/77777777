from __future__ import annotations

from platforms.chatgpt.auto_upgrade import (
    normalize_card_expiry,
    normalize_us_phone_for_form,
    parse_billing_address,
    parse_card_info,
    parse_paypal_info,
    parse_upgrade_config,
)


def test_normalize_card_expiry_accepts_year_month():
    assert normalize_card_expiry("2030/7") == "07/30"
    assert normalize_card_expiry("7/2030") == "07/30"
    assert normalize_card_expiry("07/30") == "07/30"


def test_parse_card_info_separates_3ds_from_billing_fields():
    raw = (
        "4242424242424242----2030/7----123----+18585550123----"
        "https://sms.example.test/api/get_sms?key=redacted----TEST USER----"
        "18520 MANORWOOD S,CLINTON TWP 48038-4818,US"
    )
    parsed = parse_card_info(raw)

    assert parsed["card_number"] == "4242424242424242"
    assert parsed["card_expiry"] == "07/30"
    assert parsed["card_cvv"] == "123"
    assert parsed["card_phone"] == "+18585550123"
    assert parsed["card_sms_api"] == "https://sms.example.test/api/get_sms?key=redacted"
    assert parsed["card_name"] == "TEST USER"
    assert parsed["card_address"] == "18520 MANORWOOD S,CLINTON TWP 48038-4818,US"


def test_parse_paypal_info_normalizes_us_phone_without_touching_sms_url():
    parsed = parse_paypal_info("+13502234731|https://sms.example.test/api/get_sms?key=paypal")

    assert parsed["paypal_phone"] == "3502234731"
    assert parsed["paypal_phone_raw"] == "+13502234731"
    assert parsed["paypal_sms_api"] == "https://sms.example.test/api/get_sms?key=paypal"


def test_normalize_us_phone_for_form():
    assert normalize_us_phone_for_form("+1 (350) 223-4731") == "3502234731"
    assert normalize_us_phone_for_form("3502234731") == "3502234731"


def test_parse_billing_address_without_state_extracts_zip():
    parsed = parse_billing_address("18520 MANORWOOD S,CLINTON TWP 48038-4818,US")

    assert parsed.street == "18520 MANORWOOD S"
    assert parsed.city == "CLINTON TWP"
    assert parsed.state == ""
    assert parsed.postal_code == "48038-4818"
    assert parsed.country == "US"


def test_parse_billing_address_with_state_extracts_components():
    parsed = parse_billing_address("1 Test St,Seattle WA 98101,US")

    assert parsed.street == "1 Test St"
    assert parsed.city == "Seattle"
    assert parsed.state == "WA"
    assert parsed.postal_code == "98101"
    assert parsed.country == "US"


def test_parse_upgrade_config_keeps_3ds_and_paypal_phones_distinct():
    config = parse_upgrade_config(
        card_info=(
            "4242424242424242----2030/7----123----+18585550123----"
            "https://sms.example.test/api/3ds----TEST USER----"
            "1 Test St,Seattle WA 98101,US"
        ),
        paypal_info="+13502234731|https://sms.example.test/api/paypal",
        payurl_base="https://payurl.example.test",
        plan="plus",
    )

    assert config.card_phone == "+18585550123"
    assert config.card_sms_api == "https://sms.example.test/api/3ds"
    assert config.paypal_phone == "3502234731"
    assert config.paypal_sms_api == "https://sms.example.test/api/paypal"
    assert config.card_expiry == "07/30"
    assert config.payurl_base == "https://payurl.example.test"
    assert config.plan == "plus"
