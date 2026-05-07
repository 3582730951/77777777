"""
HTTP 流量记录代理 - 记录所有浏览器请求/响应/Cookie
监听 8888 端口，浏览器需设置代理
"""
import sys, json, datetime, threading, gzip, io
from http.server import HTTPServer, BaseHTTPRequestHandler
from urllib.request import Request, urlopen
from urllib.parse import urlparse

LOG_FILE = "/tmp/browser_traffic.jsonl"
lock = threading.Lock()

def ts():
    return datetime.datetime.now().isoformat(timespec="milliseconds")

def log_event(data):
    line = json.dumps(data, ensure_ascii=False)
    with lock:
        with open(LOG_FILE, "a") as f:
            f.write(line + "\n")
    print(line[:200], flush=True)

class ProxyHandler(BaseHTTPRequestHandler):
    def do_CONNECT(self):
        # HTTPS tunnel - can't inspect
        host, port = self.path.split(":")
        log_event({"ts": ts(), "type": "connect", "host": host, "port": port})
        self.send_response(200, "Connection Established")
        self.end_headers()

    def log_message(self, format, *args):
        pass

if __name__ == "__main__":
    with open(LOG_FILE, "w") as f:
        f.write("")
    print(f"[记录器] 流量日志: {LOG_FILE}", flush=True)
    print("[记录器] 注意: HTTPS 流量只能看到域名，无法解密内容", flush=True)
    print("[记录器] 建议直接在浏览器中操作，我会通过截图+页面监控记录", flush=True)
