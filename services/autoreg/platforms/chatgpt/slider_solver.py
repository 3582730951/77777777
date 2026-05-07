"""
人类模拟滑块求解器
模拟真实人手拖拽行为：
- 缓慢按下
- 不均匀速度前进
- 自然手抖
- 中途微停顿
- 末尾减速微调
- 偶尔轻微回退修正
"""
import time
import random
import math


def _human_delay(base=0.01, jitter=0.02):
    """随机延迟"""
    time.sleep(base + random.random() * jitter)


def _generate_human_track(distance):
    """
    生成模拟人手的滑动轨迹点列表 [(dx, dy, delay), ...]
    dx: 相对于起点的 x 偏移
    dy: 相对于起点的 y 偏移
    delay: 到达该点后等待的时间
    """
    track = []
    current_x = 0
    current_y = 0
    remaining = distance

    # 阶段1: 缓慢启动 (约 15% 距离)
    slow_start = distance * random.uniform(0.10, 0.18)
    # 阶段2: 加速冲刺 (约 60% 距离)
    fast_mid = distance * random.uniform(0.55, 0.65)
    # 阶段3: 减速到位 (剩余距离)

    t = 0
    while current_x < distance - 2:
        if current_x < slow_start:
            # 阶段1: 慢速，每次移动 2-6px
            move = random.uniform(2, 6)
            delay = random.uniform(0.025, 0.06)
        elif current_x < slow_start + fast_mid:
            # 阶段2: 快速，每次移动 8-18px
            move = random.uniform(8, 18)
            delay = random.uniform(0.008, 0.022)
        else:
            # 阶段3: 减速，每次移动 1-4px
            move = random.uniform(1, 4)
            delay = random.uniform(0.03, 0.08)

        # 不要超过目标
        if current_x + move > distance:
            move = distance - current_x

        current_x += move

        # Y 轴自然抖动：人手不可能完全水平
        y_drift = random.gauss(0, 1.5)
        current_y += y_drift
        # 限制 Y 偏移范围
        current_y = max(-6, min(6, current_y))

        # 偶尔中途停顿 (5% 概率)
        if random.random() < 0.05:
            delay += random.uniform(0.08, 0.25)

        # 偶尔轻微回退 (3% 概率，模拟手抖)
        if random.random() < 0.03 and current_x > slow_start:
            current_x -= random.uniform(0.5, 2)
            delay += random.uniform(0.02, 0.05)

        track.append((current_x, current_y, delay))
        t += 1

    # 末尾微调：轻微超过再回来
    if random.random() < 0.4:
        overshoot = random.uniform(2, 5)
        track.append((distance + overshoot, current_y + random.uniform(-1, 1), random.uniform(0.03, 0.06)))
        track.append((distance + overshoot - random.uniform(1, 3), current_y, random.uniform(0.05, 0.1)))

    # 最终位置
    track.append((distance, current_y * 0.3, random.uniform(0.05, 0.15)))

    return track


def solve_slider(page, max_retries=6):
    """
    解决 PayPal 滑块验证

    Returns: True if solved, False otherwise
    """
    for attempt in range(max_retries):
        time.sleep(2 + random.random() * 2)

        # 点击 Retry 按钮（如果有）
        retry = page.query_selector('button:has-text("Retry")')
        if retry:
            try:
                if retry.is_visible():
                    time.sleep(random.uniform(0.5, 1.5))
                    retry.click()
                    time.sleep(3 + random.random() * 2)
            except:
                pass

        # 检查是否已经通过（页面显示了正常内容）
        body = page.text_content("body") or ""
        if _page_has_form(body):
            return True

        # 检查 "Try the challenge" 文字（滑块失败状态）
        if "try the challenge" in body.lower():
            retry = page.query_selector('button:has-text("Retry")')
            if retry:
                try:
                    retry.click()
                    time.sleep(4 + random.random() * 2)
                except:
                    pass

        # 寻找滑块
        slider = _find_slider(page)
        if not slider:
            # 再等一下看滑块是否出现
            time.sleep(3)
            slider = _find_slider(page)
        if not slider:
            # 没有滑块也没有表单 — 可能还在加载
            if "paypal.com" in page.url:
                time.sleep(5)
                body = page.text_content("body") or ""
                if _page_has_form(body):
                    return True
                # 再找一次
                slider = _find_slider(page)
            if not slider:
                print(f"    [slider] 未找到滑块 (attempt {attempt+1})", flush=True)
                time.sleep(3)
                continue

        print(f"    [slider] 尝试 {attempt+1}/{max_retries}...", flush=True)

        box = slider.bounding_box()
        if not box:
            continue

        sx = box["x"] + box["width"] / 2
        sy = box["y"] + box["height"] / 2

        # 先随机移动鼠标到附近区域（人不会精确移到滑块上）
        page.mouse.move(
            sx + random.uniform(-30, 30),
            sy + random.uniform(20, 60)
        )
        time.sleep(random.uniform(0.3, 0.7))

        # 移到滑块附近
        page.mouse.move(sx + random.uniform(-5, 5), sy + random.uniform(-3, 3))
        time.sleep(random.uniform(0.15, 0.35))

        # 精确移到滑块中心
        page.mouse.move(sx, sy)
        time.sleep(random.uniform(0.1, 0.25))

        # 按下鼠标
        page.mouse.down()
        time.sleep(random.uniform(0.08, 0.2))

        # 生成人类轨迹并执行
        track = _generate_human_track(280 + random.randint(-10, 25))
        for dx, dy, delay in track:
            page.mouse.move(sx + dx, sy + dy)
            time.sleep(delay)

        # 松手前的最后停顿
        time.sleep(random.uniform(0.1, 0.3))
        page.mouse.up()

        # 等待结果
        time.sleep(4 + random.random() * 2)

        # 检查结果
        body = page.text_content("body") or ""
        if _page_has_form(body):
            print(f"    [slider] 通过!", flush=True)
            return True

        if "Try the challenge" in body:
            print(f"    [slider] 失败，重试...", flush=True)
            continue

        # 可能成功了但页面还在加载
        time.sleep(3)
        body = page.text_content("body") or ""
        if _page_has_form(body):
            print(f"    [slider] 通过!", flush=True)
            return True

    return False


def _find_slider(page):
    """找到滑块按钮元素"""
    for el in page.query_selector_all('button, [role="slider"], div[class*="slider"]'):
        try:
            box = el.bounding_box()
            if box and 30 < box["width"] < 80 and 25 < box["height"] < 65:
                return el
        except:
            continue
    return None


def _page_has_form(body):
    """检查页面是否有正常的表单内容（非 captcha）"""
    if not body:
        return False
    lower = body.lower()
    form_indicators = ["email", "password", "phone", "create a paypal", "log in", "sign up"]
    captcha_indicators = ["confirm you're human", "try the challenge", "move the slider"]
    has_form = any(ind in lower for ind in form_indicators)
    has_captcha = any(ind in lower for ind in captcha_indicators)
    return has_form and not has_captcha
