"""Minimal LiteLLM stand-in for the installer smoke test.

Serves /key/info and /team/info for the key "sk-smoke" so `doctor` and the
status line can complete a real budget fetch. Usage: python mock_litellm.py [port]
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

KEY = "sk-smoke"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.headers.get("Authorization") != f"Bearer {KEY}":
            return self.reply(401, {"error": {"message": "invalid key", "type": "auth_error"}})
        path = self.path.split("?", 1)[0]
        if path == "/key/info":
            return self.reply(200, {"info": {"team_id": "team-smoke", "user_id": "user-smoke"}})
        if path == "/team/info":
            return self.reply(200, {"team_info": {"spend": 12.5, "max_budget": 50, "budget_duration": "7d"}})
        return self.reply(404, {"error": {"message": "not found"}})

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 4000
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
