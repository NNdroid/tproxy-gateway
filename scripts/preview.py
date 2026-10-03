"""Local WebUI preview with synthetic data. Does not run or configure the gateway."""
import argparse
import json
from datetime import datetime, timedelta
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

ROOT = Path(__file__).resolve().parents[1]


class PreviewHandler(SimpleHTTPRequestHandler):
    mock_mcp_auth = dict(mode="inherit", api_key_header="X-API-Key", username="", token_configured=False,
                         api_key_configured=False, password_configured=False, preview=True)
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(ROOT / "webui"), **kwargs)

    def json(self, data, status=200):
        encoded = json.dumps(data).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self):
        path = urlsplit(self.path).path
        if path == "/api/state":
            self.json(dict(preview=True, uptime="12h36m", dns_queries_total=1284962,
                           dns_cache_records_total=2847, active_tcp_conns=128,
                           active_udp_sessions=64, fakeip_records_total=16384,
                           auth_required=False, fakeip_cidrs=["198.18.0.0/15", "fd00::/8"], ui_mode="both"))
        elif path == "/api/analytics":
            now = datetime.now().replace(minute=0, second=0, microsecond=0)
            self.json(dict(top_domains=[dict(domain=d, count=n) for d, n in
                                       [("api.github.com", 18420), ("cdn.example.org", 14652),
                                        ("dns.example.net", 11035), ("updates.example.com", 8271),
                                        ("assets.example.org", 5940)]],
                           hourly_trend=[dict(timestamp=(now-timedelta(hours=719-i)).strftime("%Y-%m-%d %H:00"),
                                              dns_count=500+(i*317)%2800) for i in range(720)]))
        elif path == "/api/connections":
            self.json(dict(status="success", connections=[dict(id="preview-1", client_ip="192.0.2.10",
                           target="[2001:db8::1]:443", domain="api.github.com", upstream="DIRECT", duration="2m15s")]))
        elif path == "/api/logs":
            self.json(dict(status="success", current_level="INFO", logs=[
                dict(timestamp="12:36:20.124", level="INFO", message="[Preview] Synthetic telemetry; gateway is not running."),
                dict(timestamp="12:36:21.053", level="INFO", message="[DNS] Cache hit for api.github.com"),
                dict(timestamp="12:36:22.018", level="DEBUG", message="[TCP] Connection established via DIRECT")]))
        elif path == "/api/mcp/settings":
            self.json(self.mock_mcp_auth)
        elif path == "/api/config":
            self.json(dict(status="success", path="config.yaml (preview)",
                           content="log:\n  level: info\n\nui:\n  enabled: true\n  mode: both\n\nrouting:\n  default_upstream: DIRECT\n"))
        elif path == "/api/fakeip/lookup":
            self.json(dict(status="success", domain="example.org", fake_ips=["198.18.0.10", "fd00::10"]))
        elif path.startswith("/api/"):
            self.json(dict(status="error", message="Unsupported preview endpoint"), 404)
        else:
            if self.path.startswith("/ui/"):
                self.path = self.path[3:]
            super().do_GET()

    def do_POST(self):
        # Consume requests without writing files, restarting processes or dialing nodes.
        length = int(self.headers.get("Content-Length", "0"))
        if length > 1024 * 1024:
            self.json(dict(status="error", message="Preview body too large"), 413)
            return
        body = self.rfile.read(length)
        if self.path == "/api/proxy/ping":
            self.json(dict(status="success", nodes={"edge.example:1080": dict(status="online", latency="24 ms")}))
        elif self.path == "/api/logs/level":
            self.json(dict(status="success", current_level=json.loads(body).get("level", "info").upper()))
        else:
            self.json(dict(status="success", message="Preview only: no gateway changes applied."))

    def do_PUT(self):
        if self.path != "/api/mcp/settings":
            self.json(dict(status="error", message="Unsupported preview endpoint"), 404)
            return
        length = int(self.headers.get("Content-Length", "0"))
        if length > 16384:
            self.json(dict(status="error", message="Preview settings too large"), 413)
            return
        try:
            settings = json.loads(self.rfile.read(length))
            if settings.get("mode") not in ("inherit", "bearer", "api_key", "basic", "none"):
                raise ValueError("invalid mode")
        except (ValueError, AttributeError):
            self.json(dict(status="error", message="Invalid preview settings"), 400)
            return
        self.mock_mcp_auth["mode"] = settings["mode"]
        for key in ("username", "api_key_header"):
            if key in settings:
                self.mock_mcp_auth[key] = settings[key]
        for key, flag in (("token", "token_configured"), ("api_key", "api_key_configured"), ("password", "password_configured")):
            if settings.get(key):
                self.mock_mcp_auth[flag] = True
        self.json(self.mock_mcp_auth)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=8765)
    args = parser.parse_args()
    print(f"Synthetic WebUI preview: http://127.0.0.1:{args.port}/ui/", flush=True)
    ThreadingHTTPServer(("127.0.0.1", args.port), PreviewHandler).serve_forever()
