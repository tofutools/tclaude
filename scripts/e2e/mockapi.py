#!/usr/bin/env python3
"""Minimal mock of the Anthropic Messages API for credential-free E2E runs.

Every /v1/messages call streams back one short text reply naming the node, so
real Claude Code TUIs render normally without real credentials or token spend.
Usage: mockapi.py <port> <node-label>
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT, LABEL = int(sys.argv[1]), sys.argv[2]


def sse(event, data):
    return f"event: {event}\ndata: {json.dumps(data)}\n\n".encode()


class H(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # quiet
        sys.stderr.write("mockapi " + (fmt % args) + "\n")

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._json(200, {"data": [], "has_more": False})

    def do_HEAD(self):
        self.send_response(200)
        self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("content-length", 0) or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            req = json.loads(raw or b"{}")
        except json.JSONDecodeError:
            req = {}
        if "count_tokens" in self.path:
            return self._json(200, {"input_tokens": 42})
        if not self.path.startswith("/v1/messages"):
            return self._json(200, {})
        last = ""
        for m in reversed(req.get("messages", [])):
            if m.get("role") == "user":
                c = m.get("content")
                parts = [c] if isinstance(c, str) else [p.get("text", "") for p in c if isinstance(p, dict) and p.get("type") == "text"]
                # Skip harness-injected <system-reminder> blocks; echo the operator's own words.
                parts = [t for t in parts if t.strip() and not t.lstrip().startswith("<system-reminder>")]
                last = parts[-1] if parts else ""
                break
        text = f"[mock model @ {LABEL}] Received: {last.strip()[:160] or '(empty)'}"
        model = req.get("model", "claude-mock")
        if not req.get("stream"):
            return self._json(200, {
                "id": "msg_mock", "type": "message", "role": "assistant", "model": model,
                "content": [{"type": "text", "text": text}], "stop_reason": "end_turn",
                "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 10}})
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("cache-control", "no-cache")
        self.end_headers()
        w = self.wfile.write
        w(sse("message_start", {"type": "message_start", "message": {
            "id": "msg_mock", "type": "message", "role": "assistant", "model": model, "content": [],
            "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 1}}}))
        w(sse("content_block_start", {"type": "content_block_start", "index": 0,
                                      "content_block": {"type": "text", "text": ""}}))
        w(sse("content_block_delta", {"type": "content_block_delta", "index": 0,
                                      "delta": {"type": "text_delta", "text": text}}))
        w(sse("content_block_stop", {"type": "content_block_stop", "index": 0}))
        w(sse("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                                "usage": {"output_tokens": 12}}))
        w(sse("message_stop", {"type": "message_stop"}))
        self.wfile.flush()


ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
