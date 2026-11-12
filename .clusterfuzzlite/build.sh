#!/bin/bash -eu
# Builds every fuzz target for ClusterFuzzLite or OSS-Fuzz. Keep this
# list in step with the Fuzz functions (scripts/check-fuzz-targets.sh
# checks it in CI).
cd "$SRC/genaiopengateway"
mod=github.com/rnikadi/genaiopengateway

fuzz() { compile_native_go_fuzzer "$mod/$1" "$2" "$3"; }

fuzz internal/provider/anthropic FuzzTranslateRequest  anthropic_request
fuzz internal/provider/anthropic FuzzTranslateResponse anthropic_response
fuzz internal/provider/anthropic FuzzTranslateStream   anthropic_stream
fuzz internal/provider/gemini    FuzzTranslateRequest  gemini_request
fuzz internal/provider/gemini    FuzzTranslateResponse gemini_response
fuzz internal/provider/gemini    FuzzTranslateStream   gemini_stream
fuzz internal/provider/bedrock   FuzzTranslateRequest  bedrock_request
fuzz internal/provider/bedrock   FuzzTranslateResponse bedrock_response
fuzz internal/provider/bedrock   FuzzTranslateStream   bedrock_stream
fuzz internal/server             FuzzRealtimeSessionModel realtime_session_model
fuzz internal/server             FuzzLiveSetup         live_setup
fuzz internal/server             FuzzLiveClientMessage live_client_message
fuzz internal/sse                FuzzTap               sse_tap
fuzz internal/sse                FuzzDrop              sse_drop
fuzz internal/redact             FuzzBodyStaysJSON     redact_body
