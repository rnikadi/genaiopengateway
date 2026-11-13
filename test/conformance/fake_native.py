"""A fake for providers with their own APIs (ElevenLabs, Deepgram,
AssemblyAI, Stability AI, fal, Cohere, and Veo on the Gemini API) in the SDK conformance run. It answers their real paths and
checks each provider's own authentication header."""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

TRANSCRIPTS = {}
VEO = {"polls": 0}


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

    def authorized(self):
        path = self.path
        if path.startswith("/v1/text-to-speech/") or path.startswith("/v1/speech-to-text"):
            return self.headers.get("xi-api-key") == "native-key"
        if path.startswith("/v1/speak") or path.startswith("/v1/listen"):
            return self.headers.get("Authorization") == "Token native-key"
        if path.startswith("/v2beta/") or path.startswith("/compatibility/") or path.startswith("/v2/rerank"):
            return self.headers.get("Authorization") == "Bearer native-key"
        if path.startswith("/v1beta/"):
            return self.headers.get("X-Goog-Api-Key") == "native-key"
        if path.startswith("/fal-ai/"):
            return self.headers.get("Authorization") == "Key native-key"
        return self.headers.get("Authorization") == "native-key"

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if not self.authorized():
            return self.reply(401, {"detail": {"message": "bad key"}})
        path, _, query = self.path.partition("?")
        if path.startswith("/v1/text-to-speech/"):
            if json.loads(raw).get("model_id") != "eleven_flash_v2_5" or "output_format=mp3_44100_128" not in query:
                return self.reply(400, {"detail": {"message": f"unexpected {raw!r} {query}"}})
            return self.reply(200, b"ID3eleven", "audio/mpeg")
        if path == "/v1/speech-to-text":
            if b"scribe_v1" not in raw or b"RIFF" not in raw:
                return self.reply(400, {"detail": {"message": "model or file missing"}})
            return self.reply(200, {"language_code": "eng", "text": "hello world", "words": [
                {"text": "hello", "start": 0, "end": 1, "type": "word"},
                {"text": "world", "start": 1, "end": 120, "type": "word"}]})
        if path == "/v1/speak":
            if "model=aura-2-thalia-en" not in query or json.loads(raw) != {"text": "hello world"}:
                return self.reply(400, {"err_msg": f"unexpected {raw!r} {query}"})
            return self.reply(200, b"ID3deepgram", "audio/mpeg")
        if path == "/v1/listen":
            if "model=nova-3" not in query or not raw.startswith(b"RIFF"):
                return self.reply(400, {"err_msg": "model or audio missing"})
            return self.reply(200, {"metadata": {"duration": 30.0}, "results": {"channels": [
                {"alternatives": [{"transcript": "hello world", "words": []}]}]}})
        if path == "/v2beta/stable-image/generate/core":
            if b'name="prompt"' not in raw or self.headers.get("Accept") != "application/json":
                return self.reply(400, {"errors": ["prompt missing"]})
            return self.reply(200, {"image": "aQ==", "finish_reason": "SUCCESS", "seed": 1})
        if path == "/fal-ai/flux/dev":
            body = json.loads(raw)
            n = body.get("num_images", 1)
            return self.reply(200, {"images": [{"url": f"https://fal.example/{i}.png"} for i in range(n)], "has_nsfw_concepts": [False] * n})
        if path == "/compatibility/v1/chat/completions":
            return self.reply(200, {"id": "c", "object": "chat.completion", "created": 1, "model": json.loads(raw)["model"],
                                    "choices": [{"index": 0, "message": {"role": "assistant", "content": "cohere hi"}, "finish_reason": "stop"}],
                                    "usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}})
        if path == "/compatibility/v1/embeddings":
            return self.reply(200, {"object": "list", "model": "embed-v4.0", "data": [{"object": "embedding", "index": 0, "embedding": [0.5]}],
                                    "usage": {"prompt_tokens": 2, "total_tokens": 2}})
        if path == "/v2/rerank":
            return self.reply(200, {"id": "r", "results": [{"index": 1, "relevance_score": 0.8}], "meta": {"billed_units": {"search_units": 1}}})
        if path == "/v1beta/models/veo-3.0-fast-generate-001:predictLongRunning":
            body = json.loads(raw)
            if body.get("parameters", {}).get("durationSeconds") != 6:
                return self.reply(400, {"error": {"message": f"unexpected {body}", "status": "INVALID_ARGUMENT"}})
            VEO["polls"] = 0
            return self.reply(200, {"name": "models/veo-3.0-fast-generate-001/operations/op1"})
        if path == "/v2/upload":
            return self.reply(200, {"upload_url": "https://cdn.example/u1"})
        if path == "/v2/transcript":
            tid = f"t{len(TRANSCRIPTS) + 1}"
            TRANSCRIPTS[tid] = 0
            return self.reply(200, {"id": tid, "status": "queued"})
        return self.reply(404, {"error": "unknown path " + path})

    def do_GET(self):
        if not self.authorized():
            return self.reply(401, {"error": "bad key"})
        if self.path == "/v1beta/models/veo-3.0-fast-generate-001/operations/op1":
            VEO["polls"] += 1
            if VEO["polls"] < 2:
                return self.reply(200, {"name": "op1"})
            host = self.headers.get("Host")
            return self.reply(200, {"name": "op1", "done": True, "response": {"generateVideoResponse": {
                "generatedSamples": [{"video": {"uri": f"http://{host}/v1beta/files/v1:download?alt=media"}}]}}})
        if self.path.startswith("/v1beta/files/v1:download"):
            return self.reply(200, b"veo-mp4", "video/mp4")
        tid = self.path.rsplit("/", 1)[-1]
        if tid not in TRANSCRIPTS:
            return self.reply(404, {"error": "no transcript"})
        TRANSCRIPTS[tid] += 1
        if TRANSCRIPTS[tid] < 2:
            return self.reply(200, {"id": tid, "status": "processing"})
        return self.reply(200, {"id": tid, "status": "completed", "text": "hello world", "audio_duration": 60,
                                "language_code": "en", "words": []})

    def do_DELETE(self):
        tid = self.path.rsplit("/", 1)[-1]
        TRANSCRIPTS.pop(tid, None)
        return self.reply(200, {"id": tid, "status": "completed"})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
