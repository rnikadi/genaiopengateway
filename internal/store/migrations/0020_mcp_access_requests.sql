-- Requests for a team's access to an MCP virtual server from the catalog
-- (docs/mcp-catalog.md), and their decisions.
CREATE TABLE mcp_access_requests (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL,
    team_id      TEXT NOT NULL,
    server       TEXT NOT NULL,
    reason       TEXT NOT NULL,
    requested_by TEXT NOT NULL,
    created_at   BIGINT NOT NULL,
    status       TEXT NOT NULL,
    decided_by   TEXT NOT NULL,
    decided_at   BIGINT NOT NULL,
    note         TEXT NOT NULL
);
CREATE INDEX mcp_access_requests_org ON mcp_access_requests (org_id, status, created_at);
