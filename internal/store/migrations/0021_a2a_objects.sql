-- Task and context ids that A2A agents returned through the gateway
-- (ADR 0033), so only the caller that caused them can use them again.

CREATE TABLE a2a_objects (
    agent      TEXT NOT NULL,
    kind       TEXT NOT NULL,
    id         TEXT NOT NULL,
    org_id     TEXT NOT NULL,
    team_id    TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (agent, kind, id)
);
