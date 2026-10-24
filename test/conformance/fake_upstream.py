"""A fake OpenAI-compatible upstream for the SDK conformance run.

It answers every endpoint the gateway passes to `openai` providers, checks
that the gateway rewrote the model name, and reports usage the way OpenAI
does.
"""
import email.parser
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

UPSTREAM_MODELS = {"up-chat", "up-embed", "gpt-image-1", "tts-1", "whisper-1", "omni-moderation-latest", "rerank-v3.5"}


def multipart_fields(content_type, body):
    msg = email.parser.BytesParser().parsebytes(b"Content-Type: " + content_type.encode() + b"\r\n\r\n" + body)
    fields, files = {}, {}
    for part in msg.get_payload():
        name = part.get_param("name", header="content-disposition")
        if part.get_filename():
            files[name] = part.get_payload(decode=True)
        else:
            fields[name] = part.get_payload(decode=True).decode()
    return fields, files


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def reply(self, status, body, content_type="application/json"):
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        ctype = self.headers.get("Content-Type", "")
        if ctype.startswith("multipart/"):
            body, files = multipart_fields(ctype, raw)
        else:
            body, files = json.loads(raw), {}
        if self.headers.get("Authorization") != "Bearer upstream-key":
            return self.reply(401, {"error": {"message": "bad upstream key"}})
        if body.get("model") not in UPSTREAM_MODELS:
            return self.reply(400, {"error": {"message": f"unexpected model {body.get('model')!r}"}})
        path = self.path
        if path == "/v1/chat/completions":
            return self.reply(200, {"id": "c", "object": "chat.completion", "created": 1, "model": body["model"],
                                    "choices": [{"index": 0, "message": {"role": "assistant", "content": "hi"}, "finish_reason": "stop"}],
                                    "usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}})
        if path == "/v1/embeddings":
            return self.reply(200, {"object": "list", "model": body["model"], "data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2]}],
                                    "usage": {"prompt_tokens": 3, "total_tokens": 3}})
        if path in ("/v1/images/generations", "/v1/images/edits"):
            if path.endswith("edits") and "image" not in files and "image[]" not in files:
                return self.reply(400, {"error": {"message": "image file missing"}})
            n = int(body.get("n", 1))
            return self.reply(200, {"created": 1, "data": [{"b64_json": "aQ=="}] * n,
                                    "usage": {"input_tokens": 10, "output_tokens": 50, "total_tokens": 60}})
        if path == "/v1/audio/speech":
            return self.reply(200, b"ID3fake-mp3", "audio/mpeg")
        if path in ("/v1/audio/transcriptions", "/v1/audio/translations"):
            if files.get("file", b"")[:4] != b"RIFF":
                return self.reply(400, {"error": {"message": "audio file missing"}})
            if body.get("response_format") == "text":
                return self.reply(200, b"hello world\n", "text/plain; charset=utf-8")
            return self.reply(200, {"text": "hello world", "usage": {"type": "duration", "seconds": 90}})
        if path == "/v1/moderations":
            return self.reply(200, {"id": "m", "model": body["model"], "results": [{"flagged": False, "categories": {}, "category_scores": {}}]})
        if path == "/v1/rerank":
            return self.reply(200, {"id": "r", "results": [{"index": 0, "relevance_score": 0.9}], "meta": {"billed_units": {"search_units": 1}}})
        return self.reply(404, {"error": {"message": "unknown path " + path}})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
