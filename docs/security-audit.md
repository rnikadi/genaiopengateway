# Third-party security audit

The [roadmap](roadmap.md) puts an independent security audit in the 1.0 phase. This page says what it covers and where it stands, and will link the report when there is one.

## Status

**Not yet performed.** Version 1.0.0 was released without a third-party audit. The project has no audit funding yet, and an audit needs an independent firm, so it cannot be done by the maintainers. The aim is an audit through the [OpenSSF](https://openssf.org) or [OSTIF](https://ostif.org) programs, which fund audits of open source projects, or by an adopter who commissions one.

Until then, the security work behind 1.0 is the project's own:

- A STRIDE [threat model](security.md), reviewed at each minor release.
- `govulncheck`, `staticcheck` and race-enabled tests in CI on every change.
- Signed releases with SBOMs ([releasing.md](releasing.md#verifying-a-release)).
- A [disclosure policy](https://github.com/rnikadi/genaiopengateway/blob/main/SECURITY.md) with response times.
- Continuous [fuzzing](#fuzzing) of the code that parses what clients and upstreams send.
- An [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/rnikadi/genaiopengateway) report, refreshed weekly and on every merge.

## Fuzzing

Go fuzz targets cover the code that reads untrusted input: what clients send and what upstream providers return.

| Target | Package | Checks |
| --- | --- | --- |
| `FuzzTranslateRequest`, `FuzzTranslateResponse`, `FuzzTranslateStream` | `internal/provider/anthropic`, `gemini`, `bedrock` | The OpenAI-shape translation of requests, responses and streams never panics or hangs, and its output always encodes. |
| `FuzzRealtimeSessionModel` | `internal/server` | No OpenAI Realtime client message can move a session to another model, whatever its escapes or duplicate keys. |
| `FuzzLiveSetup`, `FuzzLiveClientMessage` | `internal/server` | A Gemini Live session's setup names a model, the gateway's rewrite always names the upstream model, and no later message can carry a second setup. |
| `FuzzTap`, `FuzzDrop` | `internal/sse` | Watching a stream for usage never changes a byte of it, however it is split. |
| `FuzzBodyStaysJSON` | `internal/redact` | Redacting a JSON body leaves valid JSON. |

Each target's seed corpus runs with `go test` on every change. [ClusterFuzzLite](https://google.github.io/clusterfuzzlite/) fuzzes the targets a pull request changes for five minutes, and every target for an hour each night, keeping the corpus on the `fuzz-corpus` branch (`.github/workflows/fuzz.yml`). The build files in `.clusterfuzzlite/` are the ones an [OSS-Fuzz](https://google.github.io/oss-fuzz/) project needs, for when the project is accepted there; `scripts/check-fuzz-targets.sh` fails CI when a new target is missing from them.

To run one locally:

```sh
go test ./internal/server -run '^$' -fuzz '^FuzzRealtimeSessionModel$' -fuzztime 1m
```

Fuzzing found two ways for a realtime client to change a session's model before 1.1: a `session.update` written with `\u` escapes, and one with a repeated `type` key that the gateway and the upstream would read differently. Gemini Live had the same escape gap for a second `setup`. All three were fixed in 1.1 and are kept as regression seeds.

## Scope

The scope offered to auditors, in priority order:

1. **Authentication and tenancy:** gateway keys, the admin token, OIDC and SAML sign-in, SCIM, roles and Cedar policies, and the scoping of every admin query to its organization.
2. **Secrets:** encryption of stored credentials and bodies, KMS and Vault, and redaction in logs and errors.
3. **Request handling:** the OpenAI and Anthropic translation, streaming, request size limits, and the WebSocket relays for realtime, Gemini Live and Nova Sonic.
4. **MCP:** OAuth 2.1 for clients, per-user upstream credentials, tool pinning, and hosted stdio servers.
5. **A2A:** task and context ownership, and the card rewriting.
6. **Control and data planes, and the operator:** the snapshot protocol, the cache file, and the operator's access to Secrets.
7. **Supply chain:** the release workflow, signatures and dependencies.

## Applying for an audit

The project applies to [OSTIF](https://ostif.org/), which arranges and funds audits of open-source projects, and to the [OpenSSF](https://openssf.org/) through [Alpha-Omega](https://alpha-omega.dev/). A maintainer submits the application; the facts it needs are on this page: the scope above, the security work so far, and the fuzzing and Scorecard results. An adopter that wants to commission an audit can use the same scope, and the report will be published here either way.

## Findings

Findings will be published here with their fixes, after the 90-day disclosure window in SECURITY.md. Fixes for findings rated high or critical are backported to every supported release ([support policy](releasing.md#support)).
