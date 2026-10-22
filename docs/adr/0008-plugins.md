# 0008. Go interfaces for built-ins, WebAssembly for third-party plugins

- Status: accepted (implementation in v0.5)
- Date: 2026-10-22

## Decision

Adapters, stores, limiters and built-in guardrails are Go interfaces compiled into the binary. Third-party guardrails and transforms run as WebAssembly modules through wazero (pure Go, no CGO), with a small host API for reading and rewriting request and response JSON.

## Consequences

Plugins are sandboxed and language-neutral; there is no dynamic Go plugin loading.
