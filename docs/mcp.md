# MCP gateway

The gateway puts MCP servers behind the same keys, tenancy, limits, audit and telemetry as models. Clients connect to one gateway URL per **virtual server** and see a curated set of tools from one or more upstream MCP servers. Upstream credentials never reach clients.

This document is the design for v0.2. Decisions are recorded in ADRs [0011](adr/0011-mcp-scope.md), [0013](adr/0013-mcp-termination.md) and [0014](adr/0014-mcp-auth.md).

## Concepts

| Term | Meaning |
| --- | --- |
| **Upstream server** | A remote MCP server the gateway connects to over Streamable HTTP, with credentials held by the gateway. Defined in config under `mcp.servers`. |
| **Virtual server** | What clients connect to, at `/mcp/{name}`. It exposes chosen tools, prompts and resources from one or more upstream servers. Defined under `mcp.virtual_servers`. |
| **Tool pin** | The hash of a tool's definition (name, description, input and output schemas, annotations) as approved. A tool whose definition changes is hidden until approved again. |

## What v0.2 supports

- Clients on MCP protocol versions 2024-11-05 through 2026-07-28 over Streamable HTTP. The gateway runs stateless, which suits load-balanced replicas; legacy clients still work, with a short-lived session per request.
- Upstream servers on any of those versions over Streamable HTTP. The gateway negotiates the highest common version.
- Tools, prompts and resources (including resource templates), with listing, calling, getting and reading.
- Several upstreams per virtual server. Tool and prompt names are prefixed (`github_search_code`) so they cannot collide; the prefix is configurable and can be empty for a single upstream.
- Allow lists of tools per upstream within a virtual server.
- Tool pinning against silent definition changes ("rug pulls").
- Gateway keys and OAuth 2.1 bearer tokens for clients.
- Static upstream credentials (headers, such as a bearer token or API key), set per upstream server.
- Rate limits: each `tools/call`, `prompts/get` and `resources/read` counts against the caller's RPM at org, team and key level. Listing does not.
- An audit record per tool call, Prometheus metrics, and OpenTelemetry spans that continue the client's trace.

## Not in v0.2

| Feature | Why, and when |
| --- | --- |
| stdio servers | Running arbitrary local processes needs sandboxing; planned with hosted servers. Use a stdio-to-HTTP bridge meanwhile. |
| The deprecated HTTP+SSE transport | Deprecated by the spec since 2025-03-26. |
| Per-user upstream credentials (upstream OAuth on behalf of each user) | Needs user identities, which arrive with SSO in v0.3. |
| Server-to-client requests: sampling, elicitation, roots, and the 2026-07-28 multi round-trip results that carry them | Rare in remote servers, and sampling, roots and logging are deprecated by the spec. A tool that needs input gets a clear error. |
| The tasks extension, completions, subscriptions and list-changed notifications to clients | Clients see changes through `ttlMs` on list results and by listing again. |
| Admin API for MCP servers | MCP servers are infrastructure, like providers, so they live in config (ADR 0003). Pins and audit are runtime data and do have admin endpoints. |

## Request flow

```
client ──POST /mcp/{virtual}──▶ auth ─▶ policy ─▶ limits ─▶ route by name ─▶ upstream session ─▶ upstream server
                                 │                                                   │
                                 └──────────── audit record, metrics, span ◀─────────┘
```

1. **Authenticate** the `Authorization: Bearer` token as a gateway key or an OAuth access token (ADR 0014). Failure returns `401` with a `WWW-Authenticate` header that points OAuth clients at the protected resource metadata.
2. **Authorize** the virtual server against `allowed_mcp_servers` at org, team and key level, the same way `allowed_models` works. Failure returns `403`.
3. **Admit** `tools/call`, `prompts/get` and `resources/read` through the limiter. Failure returns a JSON-RPC error with code `-32005` and the wait in `data.retryAfterSeconds`.
4. **Route** the prefixed name to its upstream and original name, or a resource URI to the upstream that listed it.
5. **Call** the upstream over a pooled client session, passing `traceparent` in `_meta`.
6. **Return** the upstream result unchanged, apart from names.
7. **Record** the audit entry, metrics and span after the call.

## Discovery and the catalog

At startup and then every `mcp.refresh_interval` (default 60s), the gateway lists the tools, prompts, resources and resource templates of every upstream server. For each virtual server it builds a catalog: the allowed, approved items with names prefixed. Catalogs swap in atomically, so a request always sees one consistent view.

Until an upstream's first listing succeeds, its items are absent. An upstream that cannot be reached keeps its last good catalog and is retried with backoff (1s, doubling up to the refresh interval). Its calls fail with a JSON-RPC error until it recovers, and `gog_mcp_upstream_up` drops to 0.

## Tool pinning

With `pin_tools: true` on an upstream (the default), the gateway hashes each tool's definition with SHA-256 over canonical JSON:

- **First sight:** the tool is approved automatically and its hash recorded (trust on first use). Set `pin_tools: strict` to require approval for new tools too.
- **Definition changed:** the tool is hidden from every virtual server, an audit entry and a warning are logged, and `gog_mcp_tools_pending_approval` rises. An admin reviews the new definition and approves it, or leaves it hidden.

Pins live in the store, shared by every replica. Without a store they live in memory, so every restart is a new first sight, and `strict` is not allowed. An approval shows the tool at once on the replica that took it, and on the others after their next refresh. Changes and approvals are written to the admin audit log (`mcp_tool.changed` by `gateway`, `mcp_tool.approve` by the admin).

The hash covers the whole definition except `_meta`, which carries nothing for the model and may change on every listing.

## Upstream sessions

The gateway keeps one client session per upstream server per replica, shared by every client, because v0.2 upstream credentials are per server, not per user. Calls on a shared session are independent, which matches stateless servers and the way remote MCP servers are used for tools. When a call was never delivered, because the upstream expired the session (`404`) or the session had closed, the session is re-established and the call retried once. A call that may have reached the upstream is never retried, since tools can have side effects.

## Audit records

One row per `tools/call`, `prompts/get` and `resources/read`, and per request refused with `403`, written asynchronously like usage records (lost records count in `gog_usage_write_errors_total`):

`id`, `ts`, `org_id`, `team_id`, `key_id`, `subject` (the OAuth `sub`, when present), `virtual_server`, `server`, `method`, `name` (tool, prompt or resource URI), `status` (`ok`, `tool_error`, `error`, `denied`, `rate_limited`), `error_code`, `latency_ms`, `request_id`.

Arguments and results are not stored. Opt-in body capture arrives with redaction in v0.3.

## Observability

| Metric | Type | Labels |
| --- | --- | --- |
| `gog_mcp_requests_total` | counter | `virtual_server`, `method`, `status` |
| `gog_mcp_request_duration_seconds` | histogram | `virtual_server`, `method` |
| `gog_mcp_tool_calls_total` | counter | `server`, `tool`, `status` |
| `gog_mcp_upstream_up` | gauge | `server` |
| `gog_mcp_tools_pending_approval` | gauge | `server` |

Spans: `mcp.request` (server) per client request with a `mcp.upstream` (client) child, using the OpenTelemetry MCP conventions: `mcp.method.name`, `gen_ai.tool.name`, `mcp.protocol.version`, plus `gog.virtual_server`, `gog.mcp_server`, `gog.org_id`, `gog.team_id`. A `traceparent` in the client's `_meta` or HTTP headers is continued.

## Configuration

```yaml
mcp:
  public_url: https://gateway.example.com   # used in OAuth metadata; required for OAuth
  refresh_interval: 60s
  servers:
    github:
      url: https://api.githubcopilot.com/mcp/
      headers:
        Authorization: Bearer ${GITHUB_MCP_TOKEN}
      timeout: 60s                          # per call; default 60s
      pin_tools: true                       # true (default) | strict | false
    docs:
      url: https://mcp.internal.example.com/mcp
  virtual_servers:
    engineering:
      description: Code search and internal docs
      servers:
        - server: github
          tools: [search_code, get_file_contents]   # omit for every tool
          prefix: github                            # default: the server name
        - server: docs
          prompts: false                            # omit prompts from this upstream
          resources: false                          # omit resources and templates
  oauth:                                    # optional; see ADR 0014
    issuer: https://login.example.com/
    audience: https://gateway.example.com/mcp
    organization: org_01J...                # tokens map into this organization
    team_claim: groups                      # a claim value naming a team in that organization
    scopes: [mcp]                           # required scopes; advertised in metadata
```

Key, team and org policies gain `allowed_mcp_servers`, a list of virtual server names. An empty list allows all, as with `allowed_models`.

## API additions

| Endpoint | Purpose |
| --- | --- |
| `POST /mcp/{virtual_server}` | MCP Streamable HTTP endpoint |
| `GET /.well-known/oauth-protected-resource/mcp/{virtual_server}` | RFC 9728 protected resource metadata, when OAuth is configured |
| `GET /admin/v1/mcp/servers` | Upstream status: up or down, protocol version, counts, last refresh |
| `GET /admin/v1/mcp/tools?server=&status=` | Tools with pin status: `approved`, `pending`, `changed` |
| `POST /admin/v1/mcp/servers/{server}/tools/{tool}/approve` | Approve the current definition. The body, `{"hash": "…"}`, carries the hash shown, so an admin approves what they saw; a definition that changed since answers `409`. |
| `GET /admin/v1/mcp/calls?org_id=&team_id=&key_id=&server=&from=&to=` | Audit records, newest first, paginated |

## Errors

Protocol-level failures use HTTP status codes, so any client or proxy can see them. Failures inside a valid request use JSON-RPC errors.

| Case | Response |
| --- | --- |
| Missing or invalid credentials | HTTP `401`, `WWW-Authenticate: Bearer resource_metadata="…"` |
| Virtual server not allowed for this key | HTTP `403` |
| Unknown virtual server | HTTP `404` |
| Rate limited | JSON-RPC error `-32005` (in the implementation-defined range) with `data.retryAfterSeconds` |
| Unknown or hidden tool | JSON-RPC error `-32602` (invalid params), as the spec prescribes |
| Upstream unreachable or timed out | JSON-RPC error `-32603` with `data.server` |
| Tool returned an error | Passed through as a result with `isError: true` |

## Security

The threats listed in [security.md](security.md) and their controls in v0.2:

| Threat | Control |
| --- | --- |
| Token passthrough: a client token forwarded to an upstream, or an upstream accepting tokens minted for someone else | Client credentials are never forwarded. Upstreams receive only the credentials configured for them. OAuth tokens are checked for the gateway's own audience. |
| Tool poisoning through a malicious description | Admins choose which servers and tools each virtual server exposes; pinning makes descriptions reviewable. |
| Rug pull: a tool definition changes after approval | Pinning hides changed tools until approved again. |
| Name collision or shadowing across servers | Per-upstream prefixes; a collision within a virtual server fails config validation. |
| Confused deputy | The gateway does not run an OAuth proxy or dynamic client registration; it is only a resource server. |
| DNS rebinding and cross-origin requests to the gateway | Bearer authentication on every request. The gateway sends no CORS headers, so a page on another origin cannot make an authenticated request. |
| SSRF through upstream URLs | Upstream URLs come only from operator config. |
| Prompt injection in tool output | Out of scope for the transport; guardrails on tool output arrive with the guardrail pipeline. Audit makes every call traceable. |
| Denial of service through slow or huge upstream responses | Per-server timeouts and a 4 MiB request body limit. |
