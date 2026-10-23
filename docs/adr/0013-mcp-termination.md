# 0013. Terminate MCP at the gateway with the official Go SDK

- Status: accepted
- Date: 2026-10-23

## Context

A gateway can relay MCP JSON-RPC messages untouched, or terminate the protocol: act as an MCP server to clients and as an MCP client to upstreams.

Relaying is simple for one upstream, but virtual servers combine several. That means merging list results, renaming tools, routing calls and resource reads, and answering `initialize` and `server/discover` with merged capabilities. Protocol versions also differ on each side. The 2026-07-28 revision removed sessions and the `initialize` handshake, and clients and servers on 2025-06-18 and 2025-11-25 will remain common for at least a year. A relay would have to translate between the session and stateless models itself.

The official Go SDK (`github.com/modelcontextprotocol/go-sdk`, v1.7.0) implements every version from 2024-11-05 to 2026-07-28 on both sides, version negotiation, Streamable HTTP with a stateless mode, and the `Mcp-Method` and `Mcp-Name` header checks. It is maintained by the MCP project with Google, and is Apache-2.0 / MIT licensed.

## Decision

The gateway terminates MCP using the official Go SDK:

- **Client side:** one `mcp.Server` per virtual server, served by `mcp.NewStreamableHTTPHandler` in stateless mode. Tools, prompts and resources are registered from the catalog with raw handlers that forward to upstreams. Catalog changes rebuild the server and swap it in atomically.
- **Upstream side:** one `mcp.ClientSession` per upstream server per replica, over the SDK's Streamable HTTP client transport, with the gateway's HTTP client carrying upstream credentials.
- **Unchanged:** arguments, results and content blocks pass through without reinterpretation. Only names and URIs are mapped.

The SDK is pinned and upgraded deliberately. It is the only new dependency.

## Consequences

- Each side negotiates its own protocol version, so a 2026-07-28 client can use a 2025-06-18 upstream and the reverse.
- Stateless serving means any replica answers any request, with no sticky sessions or shared session store.
- Server-to-client requests (sampling, elicitation, roots) cannot cross a stateless gateway. Most remote tool servers do not use them, and the spec deprecates sampling and roots. Relaying multi round-trip results is possible later without changing this design.
- The gateway follows the SDK's release cadence for new protocol revisions.
