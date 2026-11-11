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

cached = OpenAI(base_url=BASE + "/v1", api_key="conformance-cached-key-000000")
answers = [cached.chat.completions.with_raw_response.create(model="chat", messages=[{"role": "user", "content": "cache me"}]) for _ in range(2)]
check("response cache: miss then hit", [a.headers.get("x-gog-cache") for a in answers] == ["miss", "hit"]
      and answers[1].parse().choices[0].message.content == answers[0].parse().choices[0].message.content)

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

lines = "".join(json.dumps({"custom_id": f"r{i}", "method": "POST", "url": "/v1/chat/completions",
                            "body": {"model": "chat", "messages": [{"role": "user", "content": "hi"}]}}) + "\n" for i in range(2))
f = client.files.create(file=("in.jsonl", lines.encode(), "application/jsonl"), purpose="batch")
check("files.create (batch input, model rewritten)", f.id == "file_1" and f.purpose == "batch")
check("files.list", [x.id for x in client.files.list().data] == ["file_1"])
check("files.retrieve", client.files.retrieve("file_1").bytes > 0)
b = client.batches.create(input_file_id=f.id, endpoint="/v1/chat/completions", completion_window="24h")
check("batches.create", b.id == "batch_1" and b.status == "validating")
b = client.batches.retrieve(b.id)
check("batches.retrieve", b.status == "completed" and b.output_file_id == "file_out" and b.request_counts.completed == 2)
out = client.files.content(b.output_file_id).text
check("files.content (batch output)", json.loads(out.splitlines()[0])["custom_id"] == "r0")
check("batches.list", [x.id for x in client.batches.list().data] == ["batch_1"])
c = client.batches.create(input_file_id=f.id, endpoint="/v1/chat/completions", completion_window="24h")
check("batches.cancel", client.batches.cancel(c.id).status == "cancelling")
check("files.delete", client.files.delete("file_1").deleted is True)

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
    if len(rows) >= 13:
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
check("batch charged once at the batch discount", rows["batches"]["requests"] == 1 and abs(rows["batches"]["cost_usd"] - 0.001) < 1e-9)
check("cache hit metered at no cost", rows["chat.completions"]["cache_hits"] == 1 and rows["chat.completions"]["saved_usd"] > 0)

# Providers with their own APIs, through the same OpenAI SDK calls.
r = client.audio.speech.create(model="eleven-speech", voice="JBFqnCBsd6RMkjVDRZzb", input="hello world")
check("ElevenLabs speech", r.content == b"ID3eleven")
r = client.audio.transcriptions.create(model="eleven-scribe", file=("a.wav", WAV, "audio/wav"))
check("ElevenLabs Scribe transcription", r.text == "hello world")
r = client.audio.speech.create(model="dg-speech", voice="alloy", input="hello world")
check("Deepgram Aura speech", r.content == b"ID3deepgram")
r = client.audio.transcriptions.create(model="dg-transcribe", file=("a.wav", WAV, "audio/wav"), response_format="verbose_json")
check("Deepgram transcription", r.text == "hello world" and r.duration == 30)
r = client.audio.transcriptions.create(model="aai-transcribe", file=("a.wav", WAV, "audio/wav"), response_format="text")
check("AssemblyAI transcription", r.strip() == "hello world")

r = client.images.generate(model="stab-image", prompt="a falcon", n=2, size="1792x1024")
check("Stability images", len(r.data) == 2 and r.data[0].b64_json == "aQ==")
r = client.images.generate(model="fal-image", prompt="a falcon", n=3)
check("fal images", [d.url for d in r.data] == [f"https://fal.example/{i}.png" for i in range(3)])
r = client.chat.completions.create(model="co-chat", messages=[{"role": "user", "content": "hi"}])
check("Cohere chat", r.choices[0].message.content == "cohere hi" and r.usage.total_tokens == 6)
r = client.embeddings.create(model="co-embed", input="hi")
check("Cohere embeddings", r.data[0].embedding == [0.5])
r = httpx.post(BASE + "/v1/rerank", headers={"Authorization": "Bearer conformance-key-0000000000"},
               json={"model": "co-rerank", "query": "q", "documents": ["a", "b"]})
check("Cohere rerank", r.status_code == 200 and r.json()["results"][0]["index"] == 1)

v = client.videos.create(model="veo", prompt="a falcon", seconds="6", size="1280x720")
check("Veo videos.create", v.id.startswith("veo_") and v.status == "queued")
statuses = [client.videos.retrieve(v.id).status for _ in range(2)]
check("Veo videos.retrieve", statuses == ["in_progress", "completed"])
check("Veo videos.download_content", client.videos.download_content(v.id).content == b"veo-mp4")


def usage_by_model():
    req = urllib.request.Request(BASE + "/admin/v1/usage?group_by=model",
                                 headers={"Authorization": "Bearer conformance-admin-token-0000000000"})
    return {row["group"]: row for row in json.load(urllib.request.urlopen(req))["data"]}


for _ in range(50):
    rows = usage_by_model()
    if all(m in rows for m in ("eleven-speech", "eleven-scribe", "dg-speech", "dg-transcribe", "aai-transcribe",
                               "stab-image", "fal-image", "co-chat", "co-rerank", "veo")):
        break
    time.sleep(0.2)
check("native speech metered", rows["eleven-speech"]["characters"] == 11 and rows["dg-speech"]["characters"] == 11)
check("native speech cost", abs(rows["eleven-speech"]["cost_usd"] - 11 * 50 / 1e6) < 1e-12)
check("native audio seconds metered", rows["eleven-scribe"]["audio_seconds"] == 120 and rows["dg-transcribe"]["audio_seconds"] == 30
      and rows["aai-transcribe"]["audio_seconds"] == 60)
check("native transcription cost", abs(rows["aai-transcribe"]["cost_usd"] - 0.006) < 1e-9)
check("native images metered", rows["stab-image"]["images"] == 2 and rows["fal-image"]["images"] == 3
      and abs(rows["fal-image"]["cost_usd"] - 0.075) < 1e-9)
check("Cohere chat and rerank cost", abs(rows["co-chat"]["cost_usd"] - (4 * 2.5 + 2 * 10) / 1e6) < 1e-12
      and abs(rows["co-rerank"]["cost_usd"] - 0.002) < 1e-9)
check("Veo seconds and cost", rows["veo"]["video_seconds"] == 6 and abs(rows["veo"]["cost_usd"] - 0.9) < 1e-9)
print("conformance: all checks passed")
