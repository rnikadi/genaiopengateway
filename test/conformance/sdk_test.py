"""Calls every OpenAI-shaped endpoint through the gateway with the official
OpenAI Python SDK, then checks what the gateway metered (run.sh)."""
import json
import os
import time
import urllib.request

import httpx
from openai import OpenAI

BASE = os.environ.get("GATEWAY_URL", "http://127.0.0.1:18190")
# The SDK assumes wss for realtime; the local gateway is plain ws.
client = OpenAI(base_url=BASE + "/v1", api_key="conformance-key-0000000000",
                websocket_base_url=BASE.replace("http", "ws", 1) + "/v1")
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

v = client.videos.create(model="video", prompt="a falcon", seconds="8")
check("videos.create", v.id == "video_1" and v.status == "queued")
v = client.videos.create(model="video", prompt="from a still", seconds="8", input_reference=("still.png", PNG, "image/png"))
check("videos.create with input_reference (multipart)", v.id == "video_1")
v = client.videos.retrieve("video_1")
check("videos.retrieve", v.status == "completed" and v.progress == 100)
check("videos.download_content", client.videos.download_content("video_1", variant="thumbnail").content == b"fake-variant=thumbnail")
page = client.videos.list()
check("videos.list", [x.id for x in page.data] == ["video_1"] and page.data[0].status == "completed")
r = client.videos.remix("video_1", prompt="at night")
check("videos.remix", r.id == "video_1-remix" and r.remixed_from_video_id == "video_1")
check("videos.delete", client.videos.delete("video_1-remix").deleted is True)

with client.realtime.connect(model="voice") as conn:
    first = conn.recv()
    check("realtime session opens", first.type == "session.created")
    conn.session.update(session={"type": "realtime", "model": "gpt-other", "instructions": "be brief"})
    ev = conn.recv()
    check("realtime session.update keeps the virtual model", ev.type == "session.updated" and ev.session.model == "gpt-realtime")
    conn.response.create()
    ev = conn.recv()
    check("realtime response.done", ev.type == "response.done" and ev.response.usage.input_tokens == 1000)

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
    if len(rows) >= 12:
        break
    time.sleep(0.2)
check("images metered", rows["images.generations"]["images"] == 2 and rows["images.edits"]["images"] == 1)
check("speech characters metered", rows["audio.speech"]["characters"] == len("hello world"))
check("audio seconds metered", rows["audio.transcriptions"]["audio_seconds"] == 90 and rows["audio.translations"]["audio_seconds"] == 90)
check("image cost", abs(rows["images.generations"]["cost_usd"] - 0.08) < 1e-9)
check("transcription cost", abs(rows["audio.transcriptions"]["cost_usd"] - 0.009) < 1e-9)
check("video seconds metered", rows["videos"]["video_seconds"] == 16 and rows["videos.remix"]["video_seconds"] == 4)
check("video cost", abs(rows["videos"]["cost_usd"] - 1.6) < 1e-9)
check("realtime audio and text cost", abs(rows["realtime"]["cost_usd"] - 0.0536) < 1e-9 and rows["realtime"]["input_tokens"] == 1000)
check("rerank cost", abs(rows["rerank"]["cost_usd"] - 0.002) < 1e-9)
print("conformance: all checks passed")
