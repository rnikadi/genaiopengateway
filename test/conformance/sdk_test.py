"""Calls every OpenAI-shaped endpoint through the gateway with the official
OpenAI Python SDK, then checks what the gateway metered (run.sh)."""
import json
import os
import time
import urllib.request

import httpx
from openai import OpenAI

BASE = os.environ.get("GATEWAY_URL", "http://127.0.0.1:18190")
client = OpenAI(base_url=BASE + "/v1", api_key="conformance-key-0000000000")
WAV = b"RIFF" + b"\0" * 64
PNG = b"\x89PNG\r\n\x1a\n" + b"\0" * 32


def check(name, ok):
    print(("ok   " if ok else "FAIL ") + name)
    if not ok:
        raise SystemExit(1)


r = client.chat.completions.create(model="chat", messages=[{"role": "user", "content": "hi"}])
check("chat completions", r.choices[0].message.content == "hi" and r.usage.total_tokens == 7)

r = client.embeddings.create(model="embed", input="hi")
check("embeddings", len(r.data[0].embedding) == 2)

r = client.images.generate(model="image", prompt="a falcon", n=2)
check("images.generate", len(r.data) == 2 and r.data[0].b64_json == "aQ==")

r = client.images.edit(model="image", image=("a.png", PNG, "image/png"), prompt="make it blue")
check("images.edit (multipart)", len(r.data) == 1)

r = client.audio.speech.create(model="speech", voice="alloy", input="hello world")
check("audio.speech", r.content == b"ID3fake-mp3")

r = client.audio.transcriptions.create(model="transcribe", file=("a.wav", WAV, "audio/wav"))
check("audio.transcriptions (multipart)", r.text == "hello world")

r = client.audio.transcriptions.create(model="transcribe", file=("a.wav", WAV, "audio/wav"), response_format="text")
check("audio.transcriptions as text", r.strip() == "hello world")

r = client.audio.translations.create(model="transcribe", file=("a.wav", WAV, "audio/wav"))
check("audio.translations (multipart)", r.text == "hello world")

r = client.moderations.create(model="moderate", input="hi")
check("moderations", r.results[0].flagged is False)

r = httpx.post(BASE + "/v1/rerank", headers={"Authorization": "Bearer conformance-key-0000000000"},
               json={"model": "rerank", "query": "q", "documents": ["a", "b"]})
check("rerank", r.status_code == 200 and r.json()["results"][0]["index"] == 0)

try:
    client.audio.speech.create(model="chat-missing", voice="alloy", input="x")
    check("unknown model is refused", False)
except Exception as e:  # noqa: BLE001
    check("unknown model is refused", getattr(e, "status_code", 0) == 404)


def usage():
    req = urllib.request.Request(BASE + "/admin/v1/usage?group_by=endpoint",
                                 headers={"Authorization": "Bearer conformance-admin-token-0000000000"})
    return {row["group"]: row for row in json.load(urllib.request.urlopen(req))["data"]}


# Usage records are written in batches.
for _ in range(50):
    rows = usage()
    if len(rows) >= 9:
        break
    time.sleep(0.2)
check("images metered", rows["images.generations"]["images"] == 2 and rows["images.edits"]["images"] == 1)
check("speech characters metered", rows["audio.speech"]["characters"] == len("hello world"))
check("audio seconds metered", rows["audio.transcriptions"]["audio_seconds"] == 90 and rows["audio.translations"]["audio_seconds"] == 90)
check("image cost", abs(rows["images.generations"]["cost_usd"] - 0.08) < 1e-9)
check("transcription cost", abs(rows["audio.transcriptions"]["cost_usd"] - 0.009) < 1e-9)
check("rerank cost", abs(rows["rerank"]["cost_usd"] - 0.002) < 1e-9)
print("conformance: all checks passed")
