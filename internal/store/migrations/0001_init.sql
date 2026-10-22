-- Portable across SQLite and Postgres: BIGINT for times (unix milliseconds),
-- TEXT for ids and JSON, DOUBLE PRECISION for money.

CREATE TABLE orgs (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    policy     TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);

CREATE TABLE teams (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES orgs (id),
    name       TEXT NOT NULL,
    policy     TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    updated_at BIGINT NOT NULL
);
CREATE INDEX teams_org ON teams (org_id);

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    org_id       TEXT NOT NULL REFERENCES orgs (id),
    team_id      TEXT NOT NULL REFERENCES teams (id),
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,
    secret_hash  TEXT NOT NULL UNIQUE,
    policy       TEXT NOT NULL,
    expires_at   BIGINT,
    revoked_at   BIGINT,
    last_used_at BIGINT,
    created_at   BIGINT NOT NULL,
    updated_at   BIGINT NOT NULL
);
CREATE INDEX api_keys_team ON api_keys (team_id);

CREATE TABLE usage (
    request_id          TEXT NOT NULL,
    ts                  BIGINT NOT NULL,
    org_id              TEXT NOT NULL,
    team_id             TEXT NOT NULL,
    key_id              TEXT NOT NULL,
    endpoint            TEXT NOT NULL,
    model               TEXT NOT NULL,
    provider            TEXT NOT NULL,
    upstream_model      TEXT NOT NULL,
    status              INTEGER NOT NULL,
    latency_ms          BIGINT NOT NULL,
    input_tokens        BIGINT NOT NULL,
    output_tokens       BIGINT NOT NULL,
    cached_input_tokens BIGINT NOT NULL,
    reasoning_tokens    BIGINT NOT NULL,
    cost_usd            DOUBLE PRECISION NOT NULL
);
CREATE INDEX usage_ts ON usage (ts);
CREATE INDEX usage_org_ts ON usage (org_id, ts);

CREATE TABLE audit_log (
    id            TEXT PRIMARY KEY,
    ts            BIGINT NOT NULL,
    actor         TEXT NOT NULL,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    org_id        TEXT NOT NULL,
    details       TEXT NOT NULL
);
CREATE INDEX audit_log_ts ON audit_log (ts);
