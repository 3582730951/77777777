"""
ChatGPT Plus 自动升级模块
流程: accessToken → payurl.779.chat → Stripe → PayPal → 3DS → 完成
"""

import json
import logging
import re
import time
from typing import Optional, Callable
from dataclasses import dataclass

logger = logging.getLogger(__name__)


class _CurlCffiRequestsProxy:
    """Lazy curl_cffi requests proxy for parser-only tests."""

    def get(self, *args, **kwargs):
        from curl_cffi import requests as _requests

        return _requests.get(*args, **kwargs)

    def post(self, *args, **kwargs):
        from curl_cffi import requests as _requests

        return _requests.post(*args, **kwargs)

    def __getattr__(self, name: str):
        from curl_cffi import requests as _requests

        return getattr(_requests, name)


cffi_requests = _CurlCffiRequestsProxy()


@dataclass
class UpgradeConfig:
    """升级配置"""
    # 卡信息
    card_number: str = ""
    card_expiry: str = ""  # MM/YY
    card_cvv: str = ""
    card_name: str = ""
    card_address: str = ""
    # 3DS 验证手机
    card_phone: str = ""
    card_sms_api: str = ""
    # PayPal 创建手机
    paypal_phone: str = ""
    paypal_sms_api: str = ""
    # 支付链接生成
    payurl_base: str = "https://payurl.779.chat"
    plan: str = "plus"  # plus or team


@dataclass
class UpgradeResult:
    success: bool = False
    stripe_url: str = ""
    chatgpt_url: str = ""
    error: str = ""
    logs: list = None


@dataclass
class BillingAddress:
    street: str = ""
    city: str = ""
    state: str = ""
    postal_code: str = ""
    country: str = "US"


def _part(value: str) -> str:
    return str(value or "").strip()


def _split_card_info(card_info: str) -> list[str]:
    parts = [_part(part) for part in str(card_info or "").split("----")]
    if len(parts) > 7:
        parts = parts[:6] + ["----".join(parts[6:])]
    return parts


def normalize_card_expiry(value: str) -> str:
    """Normalize common expiry inputs to MM/YY for checkout forms."""
    raw = _part(value)
    if not raw:
        return ""

    numbers = re.findall(r"\d+", raw)
    if len(numbers) < 2:
        return raw

    first, second = numbers[0], numbers[1]
    year = ""
    month = ""

    if len(first) == 4:
        year = first
        month = second
    elif int(first) > 12 and len(first) >= 2:
        year = first
        month = second
    elif len(second) == 4 or int(second) > 31:
        month = first
        year = second
    else:
        month = first
        year = second

    month_int = int(month or "0")
    if month_int <= 0 or month_int > 12:
        return raw
    month_text = f"{month_int:02d}"
    year_text = str(year)[-2:].zfill(2)
    return f"{month_text}/{year_text}"


def normalize_us_phone_for_form(value: str) -> str:
    """Return the local 10-digit US number for PayPal signup forms."""
    digits = re.sub(r"\D+", "", str(value or ""))
    if len(digits) == 11 and digits.startswith("1"):
        return digits[1:]
    return digits or _part(value)


def parse_paypal_info(paypal_info: str) -> dict:
    """Parse ``phone|sms_api`` PayPal test-account config."""
    phone, sep, sms_api = str(paypal_info or "").partition("|")
    raw_phone = _part(phone)
    return {
        "paypal_phone": normalize_us_phone_for_form(raw_phone),
        "paypal_phone_raw": raw_phone,
        "paypal_sms_api": _part(sms_api) if sep else "",
    }


def parse_card_info(card_info: str) -> dict:
    """Parse ``card----expiry----cvv----3ds phone----3ds sms api----name----address``."""
    parts = _split_card_info(card_info)
    while len(parts) < 7:
        parts.append("")
    return {
        "card_number": parts[0],
        "card_expiry": normalize_card_expiry(parts[1]),
        "card_expiry_raw": parts[1],
        "card_cvv": parts[2],
        "card_phone": parts[3],
        "card_sms_api": parts[4],
        "card_name": parts[5],
        "card_address": parts[6],
    }


def parse_upgrade_config(
    *,
    card_info: str = "",
    paypal_info: str = "",
    payurl_base: str = "",
    plan: str = "",
) -> UpgradeConfig:
    """Build an UpgradeConfig from UI/API text fields."""
    card = parse_card_info(card_info)
    paypal = parse_paypal_info(paypal_info)
    return UpgradeConfig(
        card_number=card["card_number"],
        card_expiry=card["card_expiry"],
        card_cvv=card["card_cvv"],
        card_phone=card["card_phone"],
        card_sms_api=card["card_sms_api"],
        card_name=card["card_name"],
        card_address=card["card_address"],
        paypal_phone=paypal["paypal_phone"],
        paypal_sms_api=paypal["paypal_sms_api"],
        payurl_base=payurl_base or UpgradeConfig.payurl_base,
        plan=plan or UpgradeConfig.plan,
    )


def parse_billing_address(address: str) -> BillingAddress:
    """Parse compact test billing addresses.

    Supported examples:
      - ``Street,CITY ST 12345,US``
      - ``Street,CITY 12345-6789,US``
      - ``Street,City,ST,12345,US``
    """
    raw_parts = [_part(part) for part in str(address or "").split(",")]
    parts = [part for part in raw_parts if part]
    if not parts:
        return BillingAddress()

    street = parts[0]
    country = "US"
    if parts and re.fullmatch(r"[A-Za-z]{2}", parts[-1]):
        country = parts[-1].upper()
        parts = parts[:-1]

    city = ""
    state = ""
    postal_code = ""

    if len(parts) >= 4 and re.fullmatch(r"[A-Za-z]{2}", parts[-2]):
        city = parts[1]
        state = parts[-2].upper()
        postal_code = parts[-1]
    elif len(parts) >= 3 and re.fullmatch(r"[A-Za-z]{2}", parts[-1]):
        city = parts[1]
        state = parts[-1].upper()
    elif len(parts) >= 2:
        city_state_zip = parts[1]
        match = re.match(r"^(.*?)\s+([A-Za-z]{2})\s+(\d{5}(?:-\d{4})?)$", city_state_zip)
        if match:
            city = match.group(1).strip()
            state = match.group(2).upper()
            postal_code = match.group(3)
        else:
            match = re.match(r"^(.*?)\s+(\d{5}(?:-\d{4})?)$", city_state_zip)
            if match:
                city = match.group(1).strip()
                postal_code = match.group(2)
            else:
                city = city_state_zip
        if len(parts) >= 3 and not postal_code:
            postal_code = parts[2]

    return BillingAddress(
        street=street,
        city=city,
        state=state,
        postal_code=postal_code,
        country=country,
    )


def _get_sms_code(sms_api_url: str, timeout: int = 120, log_fn: Callable = print) -> Optional[str]:
    """从 SMS API 获取验证码"""
    start = time.time()
    seen_codes = set()
    while time.time() - start < timeout:
        try:
            resp = cffi_requests.get(sms_api_url, timeout=15)
            text = resp.text.strip()
            # 尝试解析 JSON
            try:
                data = resp.json()
                if isinstance(data, dict):
                    text = data.get("sms", "") or data.get("code", "") or data.get("message", "") or str(data)
                elif isinstance(data, list) and data:
                    text = str(data[-1]) if data else ""
            except Exception:
                pass

            # 提取 6 位数字验证码
            codes = re.findall(r'(?<!\d)(\d{4,6})(?!\d)', text)
            for code in codes:
                if code not in seen_codes:
                    seen_codes.add(code)
                    log_fn(f"SMS 收到验证码: {code}")
                    return code
        except Exception as e:
            log_fn(f"SMS API 请求失败: {e}")
        time.sleep(5)
    log_fn("SMS 验证码等待超时")
    return None


def generate_payment_link(access_token: str, config: UpgradeConfig, proxy: str = None, log_fn: Callable = print) -> dict:
    """调用 payurl.779.chat 生成支付链接"""
    log_fn(f"正在生成支付链接 (plan={config.plan})...")

    is_plus = config.plan == "plus"
    payload = {"token": access_token, "plus": is_plus}

    proxies = {"http": proxy, "https": proxy} if proxy else None
    resp = cffi_requests.post(
        f"{config.payurl_base}/api/request",
        json=payload,
        proxies=proxies,
        timeout=30,
        impersonate="chrome120",
    )

    if resp.status_code != 200:
        raise RuntimeError(f"payurl API 失败: {resp.status_code} {resp.text[:200]}")

    data = resp.json()
    log_fn(f"payurl 响应: {json.dumps(data, ensure_ascii=False)[:300]}")

    result = {}
    # 提取各种支付链接
    for key in ("Stripe_payurl", "stripe_payurl", "url", "link", "payurl", "payment_url"):
        if data.get(key):
            result["stripe_url"] = data[key]
            break
    for key in ("chatgpt_payurl",):
        if data.get(key):
            result["chatgpt_url"] = data[key]
    for key in ("openai_payurl",):
        if data.get(key):
            result["openai_url"] = data[key]

    # 检查 nested data
    if not result.get("stripe_url") and isinstance(data.get("data"), dict):
        nested = data["data"]
        for key in ("Stripe_payurl", "stripe_payurl", "url", "link"):
            if nested.get(key):
                result["stripe_url"] = nested[key]
                break

    if not result.get("stripe_url"):
        raise RuntimeError(f"未获取到支付链接: {data}")

    log_fn(f"Stripe 链接: {result.get('stripe_url', '')[:80]}...")
    return result


def auto_upgrade_with_browser(
    access_token: str,
    config: UpgradeConfig,
    proxy: str = None,
    log_fn: Callable = print,
    headless: bool = True,
) -> UpgradeResult:
    """
    完整自动升级流程（浏览器自动化）

    1. 生成支付链接
    2. 打开 Stripe 链接
    3. 选择 PayPal
    4. 创建 PayPal 账号（SMS 验证）
    5. 填写卡信息
    6. 3DS 验证
    7. 完成
    """
    import os
    os.environ.setdefault("DISPLAY", ":99")

    result = UpgradeResult(logs=[])

    def _log(msg):
        result.logs.append(msg)
        log_fn(msg)

    try:
        # Step 1: 生成支付链接
        links = generate_payment_link(access_token, config, proxy=proxy, log_fn=_log)
        stripe_url = links.get("stripe_url", "")
        result.stripe_url = stripe_url
        result.chatgpt_url = links.get("chatgpt_url", "")

        if not stripe_url:
            result.error = "未获取到 Stripe 支付链接"
            return result

        # Step 2: 打开 Stripe 链接并自动化支付
        _log("启动浏览器打开 Stripe 支付页面...")
        from camoufox.sync_api import Camoufox

        with Camoufox(headless=headless, geoip=False, window=(1920, 1080)) as browser:
            page = browser.new_page()
            page.goto(stripe_url, wait_until="domcontentloaded", timeout=60000)
            _log(f"Stripe 页面已打开: {page.title()}")
            page.screenshot(path="/tmp/upgrade_01_stripe.png")
            time.sleep(3)

            # Step 3: 选择 PayPal
            _log("选择 PayPal 支付方式...")
            _click_paypal_option(page, _log)
            page.screenshot(path="/tmp/upgrade_02_paypal_selected.png")

            # Step 4: 创建 PayPal 账号
            if config.paypal_phone and config.paypal_sms_api:
                _log("创建 PayPal 账号...")
                _create_paypal_account(page, config, _log)
                page.screenshot(path="/tmp/upgrade_03_paypal_created.png")

            # Step 5: 填写卡信息
            if config.card_number:
                _log("填写卡信息...")
                _fill_card_info(page, config, _log)
                page.screenshot(path="/tmp/upgrade_04_card_filled.png")

            # Step 6: 3DS 验证（可选，有些卡不需要）
            if config.card_phone and config.card_sms_api:
                _log("检测是否需要 3DS 验证...")
                needs_3ds = _detect_3ds(page, timeout=10)
                if needs_3ds:
                    _log("检测到 3DS 验证，开始处理...")
                    _handle_3ds_verification(page, config, _log)
                    page.screenshot(path="/tmp/upgrade_05_3ds_done.png")
                else:
                    _log("无需 3DS 验证，跳过")
            else:
                _log("未配置 3DS 信息，跳过 3DS 验证")

            # Step 7: 等待完成
            _log("等待支付完成...")
            _wait_for_completion(page, _log)
            page.screenshot(path="/tmp/upgrade_06_done.png")

            result.success = True
            _log("升级完成！")

    except Exception as e:
        result.error = str(e)
        _log(f"升级失败: {e}")
        logger.exception("auto_upgrade error")

    return result


def _click_paypal_option(page, log_fn):
    """在 Stripe 页面选择 PayPal"""
    selectors = [
        'button[data-testid="paypal-tab"]',
        '[data-testid="PAYPAL-payment-method"]',
        'div[data-testid="paypal"]',
        'text=PayPal',
        'button:has-text("PayPal")',
        'label:has-text("PayPal")',
        'div.PaymentMethodSelector >> text=PayPal',
    ]
    for sel in selectors:
        try:
            el = page.locator(sel).first
            if el.is_visible(timeout=3000):
                el.click()
                log_fn(f"点击 PayPal: {sel}")
                time.sleep(2)
                return
        except Exception:
            continue
    log_fn("未找到 PayPal 选项，尝试截图分析...")
    page.screenshot(path="/tmp/upgrade_paypal_not_found.png")


def _create_paypal_account(page, config: UpgradeConfig, log_fn):
    """在 PayPal iframe/页面中创建账号"""
    time.sleep(3)

    # PayPal 可能在 iframe 中
    frames = page.frames
    paypal_frame = None
    for f in frames:
        if "paypal" in f.url.lower():
            paypal_frame = f
            break

    target = paypal_frame or page

    # 填写手机号
    phone_input = target.locator('input[name="phone"], input[type="tel"], input[id*="phone"]').first
    try:
        if phone_input.is_visible(timeout=5000):
            phone_input.fill(config.paypal_phone)
            log_fn(f"填写 PayPal 手机号: {config.paypal_phone}")
    except Exception:
        log_fn("未找到手机号输入框")

    # 点击发送验证码/继续
    for btn_text in ["Next", "Continue", "Send", "发送", "继续", "下一步"]:
        try:
            btn = target.locator(f'button:has-text("{btn_text}")').first
            if btn.is_visible(timeout=2000):
                btn.click()
                log_fn(f"点击: {btn_text}")
                time.sleep(3)
                break
        except Exception:
            continue

    # 获取 SMS 验证码
    if config.paypal_sms_api:
        code = _get_sms_code(config.paypal_sms_api, timeout=120, log_fn=log_fn)
        if code:
            # 填写验证码
            otp_input = target.locator('input[name="otp"], input[name="code"], input[type="tel"][maxlength="6"], input[id*="code"], input[id*="otp"]').first
            try:
                if otp_input.is_visible(timeout=10000):
                    otp_input.fill(code)
                    log_fn(f"填写 PayPal 验证码: {code}")
                    time.sleep(1)
                    # 点击确认
                    for btn_text in ["Verify", "Confirm", "Submit", "确认", "验证"]:
                        try:
                            btn = target.locator(f'button:has-text("{btn_text}")').first
                            if btn.is_visible(timeout=2000):
                                btn.click()
                                time.sleep(3)
                                break
                        except Exception:
                            continue
            except Exception:
                log_fn("未找到验证码输入框")


def _fill_card_info(page, config: UpgradeConfig, log_fn):
    """填写信用卡信息"""
    time.sleep(2)

    frames = page.frames
    target = page
    for f in frames:
        if "paypal" in f.url.lower() or "stripe" in f.url.lower():
            target = f
            break

    address = parse_billing_address(config.card_address)

    field_map = {
        "card_number": config.card_number,
        "card_name": config.card_name,
        "card_expiry": config.card_expiry,
        "card_cvv": config.card_cvv,
        "street": address.street,
        "city": address.city,
        "state": address.state,
        "zip": address.postal_code,
    }

    # 通用填写逻辑
    input_selectors = {
        "card_number": ['input[name="cardnumber"]', 'input[name="number"]', 'input[id*="cardNumber"]', 'input[autocomplete="cc-number"]'],
        "card_name": ['input[name="ccname"]', 'input[name="name"]', 'input[id*="cardholderName"]', 'input[autocomplete="cc-name"]'],
        "card_expiry": ['input[name="exp-date"]', 'input[name="expiry"]', 'input[id*="expiry"]', 'input[autocomplete="cc-exp"]'],
        "card_cvv": ['input[name="cvc"]', 'input[name="cvv"]', 'input[id*="cvc"]', 'input[autocomplete="cc-csc"]'],
        "street": ['input[name="addressLine1"]', 'input[name="address"]', 'input[id*="address"]', 'input[autocomplete="address-line1"]'],
        "city": ['input[name="city"]', 'input[id*="city"]', 'input[autocomplete="address-level2"]'],
        "state": ['input[name="state"]', 'select[name="state"]', 'input[id*="state"]'],
        "zip": ['input[name="zip"]', 'input[name="postalCode"]', 'input[id*="zip"]', 'input[autocomplete="postal-code"]'],
    }

    for field_name, value in field_map.items():
        if not value:
            continue
        for sel in input_selectors.get(field_name, []):
            try:
                el = target.locator(sel).first
                if el.is_visible(timeout=2000):
                    el.fill(value)
                    log_fn(f"填写 {field_name}: {value[:20]}...")
                    break
            except Exception:
                continue


def _detect_3ds(page, timeout=10) -> bool:
    """检测页面是否出现 3DS 验证 iframe"""
    for _ in range(timeout // 2):
        for f in page.frames:
            if any(kw in f.url.lower() for kw in ("3ds", "authentication", "challenge", "acs", "secure")):
                return True
        # 也检查页面上是否有 3DS 相关元素
        try:
            body = page.text_content("body")[:1000] if page.query_selector("body") else ""
            if any(kw in body.lower() for kw in ("verify your card", "authentication required", "3d secure")):
                return True
        except Exception:
            pass
        time.sleep(2)
    return False


def _handle_3ds_verification(page, config: UpgradeConfig, log_fn):
    """处理 3DS 验证"""
    time.sleep(5)

    # 等待 3DS iframe 出现
    log_fn("等待 3DS 验证页面...")
    for _ in range(30):
        frames = page.frames
        for f in frames:
            if "3ds" in f.url.lower() or "authentication" in f.url.lower() or "challenge" in f.url.lower():
                log_fn(f"检测到 3DS iframe: {f.url[:80]}")
                # 获取验证码
                code = _get_sms_code(config.card_sms_api, timeout=120, log_fn=log_fn)
                if code:
                    # 在 3DS iframe 中填写验证码
                    otp_input = f.locator('input[type="text"], input[type="tel"], input[type="number"], input[name*="code"], input[name*="otp"]').first
                    try:
                        if otp_input.is_visible(timeout=5000):
                            otp_input.fill(code)
                            log_fn(f"填写 3DS 验证码: {code}")
                            # 点击提交
                            for btn_text in ["Submit", "Verify", "Confirm", "确认", "提交"]:
                                try:
                                    btn = f.locator(f'button:has-text("{btn_text}"), input[type="submit"]').first
                                    if btn.is_visible(timeout=2000):
                                        btn.click()
                                        log_fn(f"提交 3DS: {btn_text}")
                                        return
                                except Exception:
                                    continue
                    except Exception:
                        log_fn("3DS 验证码输入框未找到")
                return
        time.sleep(2)
    log_fn("未检测到 3DS 验证页面")


def _wait_for_completion(page, log_fn, timeout=60):
    """等待支付完成"""
    start = time.time()
    while time.time() - start < timeout:
        url = page.url
        title = page.title()
        body = page.text_content("body")[:500] if page.query_selector("body") else ""

        if any(kw in url.lower() for kw in ["success", "complete", "thank"]):
            log_fn(f"支付成功！URL: {url}")
            return
        if any(kw in body.lower() for kw in ["success", "payment complete", "thank you", "subscription active"]):
            log_fn(f"支付成功！页面: {title}")
            return
        if any(kw in body.lower() for kw in ["failed", "declined", "error"]):
            log_fn(f"支付可能失败: {body[:200]}")
            return

        time.sleep(3)
    log_fn("等待支付完成超时")
