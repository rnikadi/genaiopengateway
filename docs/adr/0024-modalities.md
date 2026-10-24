# 0024: Images, audio, video and realtime through the one model path

- Status: accepted
- Date: 2026-10-24

## Context

Clients use more than chat and embeddings: image generation, text to speech, transcription, moderation, rerank, video and realtime voice. Without the gateway in front of them, these calls skip its keys, policies, budgets, guardrails and usage records, which defeats the point of a gateway. Ram asked for every model type the gateway can support.

## Decision

- Serve each modality at the OpenAI API path for it, through the existing model handler. Multipart requests are read into the same body plus files, so routing, fallback, limits, guardrails and body logging stay one code path.
- Implement them first on the `openai` provider type, which covers OpenAI, Azure OpenAI and the self-hosted servers that copy its API. Native Vertex AI, Bedrock and Gemini translations follow one by one.
- Meter what each modality is billed by: images, characters, audio seconds, video seconds and requests, alongside tokens. Prices on a target gain one field per count.
- Track video jobs in the store so that only the team that created a job can read, download or delete it, and so that it goes back to the provider that created it.
- Relay OpenAI's realtime WebSocket, charging each response as it finishes and closing the session when a budget runs out. WebRTC is left out because its media would not pass through the gateway.

## Consequences

- An OpenAI SDK works unchanged for every endpoint, and operators price media the way providers bill it.
- The usage table gains four columns. Earlier rows read as zero.
- Guardrails see text only. Audio, image and video bytes, and realtime sessions, are not checked. This is documented.
- Native cloud media APIs need their own adapters and are delivered after the OpenAI shape.
