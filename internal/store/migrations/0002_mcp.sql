-- MCP tool pins and the audit trail of MCP calls (docs/mcp.md).

CREATE TABLE mcp_tool_pins (
    server        TEXT NOT NULL,
    tool          TEXT NOT NULL,
    hash          TEXT NOT NULL,
    definition    TEXT NOT NULL,
    approved_hash TEXT NOT NULL,
    updated_at    BIGINT NOT NULL,
    PRIMARY KEY (server, tool)
);

CREATE TABLE mcp_calls (
    id             TEXT PRIMARY KEY,
    ts             BIGINT NOT NULL,
    org_id         TEXT NOT NULL,
    team_id        TEXT NOT NULL,
    key_id         TEXT NOT NULL,
    subject        TEXT NOT NULL,
    virtual_server TEXT NOT NULL,
    server         TEXT NOT NULL,
    method         TEXT NOT NULL,
    name           TEXT NOT NULL,
    status         TEXT NOT NULL,
    error_code     BIGINT NOT NULL,
    latency_ms     BIGINT NOT NULL,
    request_id     TEXT NOT NULL
);
CREATE INDEX mcp_calls_ts ON mcp_calls (ts);
CREATE INDEX mcp_calls_org_ts ON mcp_calls (org_id, ts);
