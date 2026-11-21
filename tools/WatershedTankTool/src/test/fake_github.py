import http.server, json, os
REL = "/home/claude/e2e/rel"
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path == "/repos/test/watershed/releases/latest":
            assets = [{"name": n, "url": f"http://127.0.0.1:8099/assets/{n}"} for n in sorted(os.listdir(REL))]
            body = json.dumps({"tag_name": "v0.4.0", "published_at": "2026-11-22T00:00:00Z", "assets": assets}).encode()
            self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers(); self.wfile.write(body); return
        if self.path.startswith("/assets/"):
            n = self.path[8:]
            if self.headers.get("Accept") != "application/octet-stream":
                self.send_response(415); self.end_headers(); return
            d = open(os.path.join(REL, n), "rb").read()
            self.send_response(200); self.send_header("Content-Length", str(len(d))); self.end_headers(); self.wfile.write(d); return
        self.send_response(404); self.end_headers()
http.server.ThreadingHTTPServer(("127.0.0.1", 8099), H).serve_forever()
