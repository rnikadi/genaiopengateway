-- Guardrail decisions (docs/guardrails.md): which guardrail acted on a
-- request and how, never the text it matched.

CREATE TABLE guardrail_events (
    id         TEXT PRIMARY KEY,
    request_id TEXT NOT NULL,
    ts         BIGINT NOT NULL,
    org_id     TEXT NOT NULL,
    team_id    TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    guardrail  TEXT NOT NULL,
    stage      TEXT NOT NULL,
    action     TEXT NOT NULL,
    failed     INTEGER NOT NULL,
    reason     TEXT NOT NULL,
    counts     TEXT NOT NULL,
    latency_ms BIGINT NOT NULL
);
CREATE INDEX guardrail_events_request ON guardrail_events (request_id);
CREATE INDEX guardrail_events_ts ON guardrail_events (ts);
CREATE INDEX guardrail_events_org_ts ON guardrail_events (org_id, ts);
