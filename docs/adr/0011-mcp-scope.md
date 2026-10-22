# 0011. MCP gateway scope

- Status: accepted (implementation in v0.5)
- Date: 2026-10-22

## Decision

v0.5 ships: a proxy for remote MCP servers over streamable HTTP, a registry of approved servers per organization, virtual servers that combine and filter tools, OAuth 2.1 resource-server support, per-user upstream credentials held by the gateway, tool-level allow lists, tool-description pinning, and audit of every tool call. Hosting stdio servers in sandboxes comes later.

## Consequences

MCP traffic shares keys, tenancy, limits and audit with model traffic.
