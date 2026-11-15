"""Stub OpenAI-compatible upstream and LiteLLM price table for the e2e smoke test."""

import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

COMPLETION = {
    "id": "chatcmpl-e2e",
    "object": "chat.completion",
    "created": 1800000000,
    "model": "stub-model-2026-11-01",
    "choices": [{"index": 0, "message": {"role": "assistant", "content": "ok"}, "finish_reason": "stop"}],
    "usage": {
        "prompt_tokens": 1200,
        "completion_tokens": 300,
        "total_tokens": 1500,
        "prompt_tokens_details": {"cached_tokens": 1000},
        "completion_tokens_details": {"reasoning_tokens": 100},
    },
}

PRICES = {
    "stub-model": {
        "litellm_provider": "openai",
        "input_cost_per_token": 1e-6,
        "output_cost_per_token": 4e-6,
        "cache_read_input_token_cost": 1e-7,
    }
}


class Handler(BaseHTTPRequestHandler):
    def _send(self, body):
        raw = json.dumps(body).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == "/prices.json":
            self._send(PRICES)
        else:
            self._send({"object": "list", "data": [{"id": "stub-model", "object": "model"}]})

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self._send(COMPLETION)

    def log_message(self, fmt, *args):
        sys.stderr.write("stub: " + fmt % args + "\n")


ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
