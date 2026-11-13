# 0011. MCP gateway scope

- Status: accepted, amended 2026-10-23 (implementation moved to v0.2)
- Date: 2026-10-22

## Decision

v0.5 ships: a proxy for remote MCP servers over streamable HTTP, a registry of approved servers per organization, virtual servers that combine and filter tools, OAuth 2.1 resource-server support, per-user upstream credentials held by the gateway, tool-level allow lists, tool-description pinning, and audit of every tool call. Hosting stdio servers in sandboxes comes later.

## Amendment (2026-10-23)

The MCP gateway moved forward to v0.2. Per-user upstream credentials need user identities, so they move to v0.3 with SSO; v0.2 holds static credentials per upstream. Every server is defined in config, as providers are, and organizations are granted access through `allowed_mcp_servers`. ADR 0013 covers how the protocol is handled and ADR 0014 covers authentication. The design is in [docs/mcp.md](../mcp.md).

## Consequences

MCP traffic shares keys, tenancy, limits and audit with model traffic.
