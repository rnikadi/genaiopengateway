-- Push notification configurations that A2A callers set through the
-- gateway (docs/a2a.md#push-notifications). The agent is given a URL at
-- the gateway that ends in a secret, and id is that secret's hash. The
-- caller's own URL stays here, with its token and credentials sealed.

CREATE TABLE a2a_push (
    id         TEXT PRIMARY KEY,
    agent      TEXT NOT NULL,
    task_id    TEXT NOT NULL,
    config_id  TEXT NOT NULL,
    org_id     TEXT NOT NULL,
    team_id    TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    url        TEXT NOT NULL,
    secret     TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL
);

CREATE INDEX a2a_push_task ON a2a_push (agent, task_id);
CREATE INDEX a2a_push_expires ON a2a_push (expires_at);
