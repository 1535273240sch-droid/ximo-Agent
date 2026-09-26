#!/usr/bin/env python3
"""假上游：OpenAI 兼容，用于在中转站部署后做端到端验证。

两个要点：
1. **usage 分片在 finish_reason 之后单独发**（真实上游的常见形态），顺带验证网关
   侧「尾随 usage 被丢弃」的修复。
2. **必须正确终止响应**：SSE 分支显式 `Content-Length` 不可用，所以写完 `[DONE]`
   后设置 `close_connection = True` 让连接关闭，客户端才知道流结束。少了这一步，
   Go 的 http 客户端会一直等到自己的超时（本文件早期版本就踩了这个坑，表现为
   非流式请求耗时 30s）。
"""
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

PORT = 8799
MODEL = "fake-large"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):  # 静音
        pass

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = False

    def _sse_start(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")  # 明确：流结束即关连接
        self.end_headers()
        self.close_connection = True

    def do_GET(self):
        if self.path.rstrip("/").endswith("/models"):
            self._json(200, {"object": "list", "data": [{"id": MODEL, "object": "model", "owned_by": "fake"}]})
        else:
            self._json(404, {"error": {"message": "not found"}})

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            req = json.loads(raw or b"{}")
        except Exception:
            req = {}
        if not self.path.rstrip("/").endswith("/chat/completions"):
            self._json(404, {"error": {"message": "not found"}})
            return
        if not (self.headers.get("Authorization") or "").startswith("Bearer "):
            self._json(401, {"error": {"message": "missing api key"}})
            return

        msgs = req.get("messages") or [{}]
        last = msgs[-1].get("content") or ""
        text = "假上游回复：" + str(last)[:40]
        usage = {"prompt_tokens": 11, "completion_tokens": 5, "total_tokens": 16}

        if req.get("stream"):
            self._sse_start()

            def chunk(obj):
                self.wfile.write(("data: " + json.dumps(obj) + "\n\n").encode())
                self.wfile.flush()

            chunk({"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": MODEL,
                   "choices": [{"index": 0, "delta": {"role": "assistant", "content": text}, "finish_reason": None}]})
            chunk({"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": MODEL,
                   "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
            chunk({"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": MODEL,
                   "choices": [], "usage": usage})
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return

        self._json(200, {
            "id": "chatcmpl-fake", "object": "chat.completion", "model": MODEL,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": text},
                         "finish_reason": "stop"}],
            "usage": usage,
        })


if __name__ == "__main__":
    HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
