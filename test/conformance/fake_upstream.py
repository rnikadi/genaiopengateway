"""A fake OpenAI-compatible upstream for the SDK conformance run.

It answers every endpoint the gateway passes to `openai` providers, checks
that the gateway rewrote the model name, and reports usage the way OpenAI
does.
"""
import email.parser
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from websockets.sync.server import serve

UPSTREAM_MODELS = {"up-chat", "up-embed", "gpt-image-1", "tts-1", "whisper-1", "omni-moderation-latest", "rerank-v3.5", "sora-2"}


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
        if self.path.startswith("/v1/videos/") and self.path.endswith("/remix"):
            # A remix takes only a prompt, as OpenAI's does.
            if set(body) != {"prompt"}:
                return self.reply(400, {"error": {"message": f"unexpected remix fields {sorted(body)}"}})
            vid = self.path.split("/")[3]
            return self.reply(200, video(vid + "-remix", "4", remixed_from_video_id=vid))
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
        if path == "/v1/videos":
            return self.reply(200, video("video_1", body.get("seconds", "4")))
        if path == "/v1/rerank":
            return self.reply(200, {"id": "r", "results": [{"index": 0, "relevance_score": 0.9}], "meta": {"billed_units": {"search_units": 1}}})
        return self.reply(404, {"error": {"message": "unknown path " + path}})

    def do_GET(self):
        if self.headers.get("Authorization") != "Bearer upstream-key":
            return self.reply(401, {"error": {"message": "bad upstream key"}})
        path, _, query = self.path.partition("?")
        parts = path.split("/")
        if len(parts) == 5 and parts[4] == "content":
            return self.reply(200, b"fake-" + query.encode(), "video/mp4")
        if len(parts) == 4 and parts[2] == "videos":
            return self.reply(200, video(parts[3], "8", status="completed", progress=100))
        return self.reply(404, {"error": {"message": "unknown path " + path}})

    def do_DELETE(self):
        parts = self.path.split("/")
        return self.reply(200, {"id": parts[3], "object": "video.deleted", "deleted": True})


def video(id, seconds, status="queued", progress=0, **extra):
    return {"id": id, "object": "video", "model": "sora-2", "status": status, "progress": progress,
            "created_at": 1, "size": "1280x720", "seconds": seconds, **extra}


def realtime(ws):
    """OpenAI's realtime WebSocket: session events and one usage report per
    response."""
    req = ws.request
    if req.headers.get("Authorization") != "Bearer upstream-key" or req.path != "/v1/realtime?model=gpt-realtime":
        ws.send(json.dumps({"type": "error", "error": {"type": "invalid_request_error", "message": "bad session " + req.path}}))
        return
    ws.send(json.dumps({"type": "session.created", "event_id": "e0", "session": {"type": "realtime", "model": "gpt-realtime"}}))
    for raw in ws:
        ev = json.loads(raw)
        if ev["type"] == "session.update":
            ws.send(json.dumps({"type": "session.updated", "event_id": "e1", "session": ev["session"]}))
        elif ev["type"] == "response.create":
            ws.send(json.dumps({"type": "response.done", "event_id": "e2", "response": {
                "id": "resp_1", "object": "realtime.response", "status": "completed", "output": [],
                "usage": {"total_tokens": 1500, "input_tokens": 1000, "output_tokens": 500,
                          "input_token_details": {"cached_tokens": 0, "text_tokens": 200, "audio_tokens": 800},
                          "output_token_details": {"text_tokens": 100, "audio_tokens": 400}}}}))


if __name__ == "__main__":
    port = int(sys.argv[1])
    ws = serve(realtime, "127.0.0.1", port + 1)
    threading.Thread(target=ws.serve_forever, daemon=True).start()
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
