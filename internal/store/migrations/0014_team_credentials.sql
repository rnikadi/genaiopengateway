-- A team's own provider credentials (docs/team-credentials.md): secret
-- holds the API key, sealed by the server with secrets.encryption_key.
CREATE TABLE team_credentials (
    team_id    TEXT NOT NULL,
    provider   TEXT NOT NULL,
    secret     TEXT NOT NULL,
    updated_at BIGINT NOT NULL,
    updated_by TEXT NOT NULL,
    PRIMARY KEY (team_id, provider)
);
