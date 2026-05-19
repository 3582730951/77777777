from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from scripts.find_cdp_endpoint import discover_cdp_endpoints, parse_port_ranges, probe_cdp_endpoint


class _CdpHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/json/version":
            self.send_response(404)
            self.end_headers()
            return
        body = json.dumps(
            {
                "Browser": "Chrome/123.0.0.0",
                "Protocol-Version": "1.3",
                "webSocketDebuggerUrl": f"ws://127.0.0.1:{self.server.server_port}/devtools/browser/test",
            }
        ).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, format, *args):
        return


def test_parse_port_ranges():
    assert parse_port_ranges("9222,9225-9223,0,65536") == [9222, 9223, 9224, 9225]


def test_probe_cdp_endpoint_finds_json_version():
    server = ThreadingHTTPServer(("127.0.0.1", 0), _CdpHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        endpoint = probe_cdp_endpoint("127.0.0.1", server.server_port, timeout=1)
    finally:
        server.shutdown()
        thread.join(timeout=2)
        server.server_close()

    assert endpoint is not None
    assert endpoint.http_url == f"http://127.0.0.1:{server.server_port}"
    assert endpoint.browser == "Chrome/123.0.0.0"
    assert endpoint.websocket_url.endswith("/devtools/browser/test")


def test_discover_cdp_endpoints_returns_sorted_matches():
    server = ThreadingHTTPServer(("127.0.0.1", 0), _CdpHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        endpoints = discover_cdp_endpoints(host="127.0.0.1", ports=[server.server_port], timeout=1, workers=1)
    finally:
        server.shutdown()
        thread.join(timeout=2)
        server.server_close()

    assert len(endpoints) == 1
    assert endpoints[0].port == server.server_port
