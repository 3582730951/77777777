import time, json, os
os.environ.setdefault("DISPLAY", ":99")

from camoufox.sync_api import Camoufox

LOG = "/tmp/vnc_record.jsonl"
SHOT_DIR = "/tmp/vnc_shots"
os.makedirs(SHOT_DIR, exist_ok=True)
open(LOG, "w").close()

with Camoufox(headless=False, geoip=False, window=(1920, 1080)) as browser:
    page = browser.new_page()
    page.goto("https://chatgpt.com", wait_until="domcontentloaded", timeout=60000)
    page.screenshot(path=f"{SHOT_DIR}/0000_start.png")
    print("READY", flush=True)

    i = 1
    prev_url = ""
    prev_cc = 0
    while True:
        time.sleep(3)
        try:
            url = page.url
            title = page.title()
            cookies = browser.cookies()
            uc = url != prev_url
            cc = len(cookies) != prev_cc

            if uc or cc or i % 10 == 0:
                path = f"{SHOT_DIR}/{i:04d}.png"
                page.screenshot(path=path)
                entry = {"i": i, "ts": time.strftime("%H:%M:%S"), "url": url, "title": title, "cookies": len(cookies)}
                if uc:
                    entry["prev_url"] = prev_url
                if cc:
                    entry["cookie_names"] = [c["name"] for c in cookies]
                    entry["cookies_full"] = cookies
                with open(LOG, "a") as f:
                    f.write(json.dumps(entry, ensure_ascii=False) + "\n")
                prev_url = url
                prev_cc = len(cookies)
            i += 1
        except:
            i += 1
