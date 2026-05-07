"""
可视化浏览器记录器 - 记录所有用户操作
用法: python3 tools/record_browser.py [起始URL]
"""
import sys, json, time, os, datetime

os.environ.setdefault("DISPLAY", ":99")

from playwright.sync_api import sync_playwright

START_URL = sys.argv[1] if len(sys.argv) > 1 else "https://chatgpt.com"
LOG_FILE = "/tmp/browser_record.jsonl"

def ts():
    return datetime.datetime.now().isoformat(timespec="milliseconds")

def log_event(evt_type, data):
    entry = {"ts": ts(), "type": evt_type, **data}
    line = json.dumps(entry, ensure_ascii=False)
    print(line, flush=True)
    with open(LOG_FILE, "a") as f:
        f.write(line + "\n")

def dump_cookies(context, label=""):
    cookies = context.cookies()
    log_event("cookies", {"label": label, "count": len(cookies), "cookies": cookies})

def main():
    print(f"[记录器] 日志文件: {LOG_FILE}", flush=True)
    print(f"[记录器] 起始URL: {START_URL}", flush=True)

    with open(LOG_FILE, "w") as f:
        f.write("")

    with sync_playwright() as p:
        browser = p.firefox.launch(
            headless=False,
            args=["--width=1280", "--height=900"],
        )
        context = browser.new_context(
            viewport={"width": 1280, "height": 900},
            user_agent="Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:137.0) Gecko/20100101 Firefox/137.0",
        )
        page = context.new_page()

        # 监听 URL 变化
        def on_navigation(frame):
            if frame == page.main_frame:
                url = page.url
                log_event("navigation", {"url": url})
                dump_cookies(context, f"after_nav:{url[:80]}")

        page.on("framenavigated", on_navigation)

        # 监听请求
        def on_request(req):
            if req.resource_type in ("document", "xhr", "fetch"):
                headers = dict(req.headers)
                log_event("request", {
                    "method": req.method,
                    "url": req.url[:200],
                    "resource_type": req.resource_type,
                    "headers_keys": list(headers.keys()),
                })

        def on_response(resp):
            if resp.request.resource_type in ("document", "xhr", "fetch"):
                set_cookies = []
                try:
                    for h in resp.headers_array():
                        if h["name"].lower() == "set-cookie":
                            set_cookies.append(h["value"][:150])
                except:
                    pass
                log_event("response", {
                    "status": resp.status,
                    "url": resp.url[:200],
                    "set_cookies_count": len(set_cookies),
                    "set_cookies": set_cookies[:10],
                })

        page.on("request", on_request)
        page.on("response", on_response)

        # 监听 console
        page.on("console", lambda msg: log_event("console", {"text": msg.text[:300]}) if msg.type in ("error", "warning") else None)

        log_event("start", {"url": START_URL})
        page.goto(START_URL, wait_until="domcontentloaded", timeout=30000)
        dump_cookies(context, "initial_load")

        print("\n[记录器] 浏览器已打开，正在记录你的所有操作。", flush=True)
        print("[记录器] 操作完成后按 Ctrl+C 停止记录。\n", flush=True)

        # 定期记录 cookies 和页面状态
        try:
            while True:
                time.sleep(5)
                # 记录当前 URL
                try:
                    current_url = page.url
                    title = page.title()
                    log_event("heartbeat", {"url": current_url, "title": title})
                except:
                    pass
        except KeyboardInterrupt:
            print("\n[记录器] 停止记录，保存最终状态...", flush=True)
            dump_cookies(context, "final")
            try:
                log_event("final_url", {"url": page.url, "title": page.title()})
                # 保存最终页面截图
                page.screenshot(path="/tmp/browser_final.png")
                print("[记录器] 截图已保存: /tmp/browser_final.png", flush=True)
            except:
                pass
            browser.close()

    print(f"[记录器] 记录完成，共 {sum(1 for _ in open(LOG_FILE))} 条事件", flush=True)
    print(f"[记录器] 日志: {LOG_FILE}", flush=True)

if __name__ == "__main__":
    main()
