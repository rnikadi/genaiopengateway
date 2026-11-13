-- Per-user upstream MCP credentials (docs/mcp-user-credentials.md).
-- Tokens are encrypted by the gateway before they are written.

CREATE TABLE mcp_connections (
    server        TEXT NOT NULL,
    subject       TEXT NOT NULL,
    access_token  TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    scopes        TEXT NOT NULL,
    expires_at    BIGINT,
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL,
    last_used_at  BIGINT NOT NULL,
    PRIMARY KEY (server, subject)
);
CREATE INDEX mcp_connections_last_used ON mcp_connections (server, last_used_at);

CREATE TABLE mcp_connect_requests (
    id_hash        TEXT PRIMARY KEY,
    state_hash     TEXT NOT NULL,
    server         TEXT NOT NULL,
    subject        TEXT NOT NULL,
    virtual_server TEXT NOT NULL,
    verifier       TEXT NOT NULL,
    expires_at     BIGINT NOT NULL
);
CREATE INDEX mcp_connect_requests_state ON mcp_connect_requests (state_hash);
