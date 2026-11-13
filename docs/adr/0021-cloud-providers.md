# 0021. Native Gemini, Vertex AI and Bedrock adapters with cloud credentials

- Status: accepted
- Date: 2026-10-23

## Context

v0.5 adds cloud providers ([cloud-providers.md](../cloud-providers.md)). Gemini and Vertex AI offer OpenAI-compatible endpoints, which the `openai` type already reaches, but they miss thinking tokens in usage, do not serve Claude on Vertex, and need static keys. Bedrock has no OpenAI-compatible endpoint for every model, and needs AWS credentials or a Bedrock API key.

## Decision

1. Three adapter types: `gemini`, `vertex` and `bedrock`.
2. Chat completions are **translated** to Gemini `generateContent` and to Bedrock Converse, so usage is exact and one shape covers each cloud's models.
3. Claude on Vertex and Bedrock reuses the `anthropic` adapter, with the URL and version field changed, for both `/v1/messages` passthrough and chat completions.
4. Google's `generateContent` API is served as passthrough for Google SDK clients.
5. Credentials come from each cloud's standard chain: Application Default Credentials through `golang.org/x/oauth2/google`, and the AWS default chain with SigV4 from small parts of `aws-sdk-go-v2`. Generated cloud SDK clients are not used.

## Consequences

- Companies reach their cloud's models with workload identity instead of long-lived keys, and costs for thinking models are right.
- Two translation tables to maintain, Gemini and Converse, each with a full fixture suite.
- New dependencies: `cloud.google.com/go/compute/metadata` (through `x/oauth2/google`) and four `aws-sdk-go-v2` modules. The generated clients, which are much larger, stay out.
- The gateway still never fetches client-supplied URLs; clients send images inline.
