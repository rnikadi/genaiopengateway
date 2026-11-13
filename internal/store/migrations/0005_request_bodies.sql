-- Opt-in request and response bodies (docs/body-logging.md), redacted
-- and encrypted before they are written.

CREATE TABLE request_bodies (
    id         TEXT PRIMARY KEY,
    request_id TEXT NOT NULL,
    ts         BIGINT NOT NULL,
    org_id     TEXT NOT NULL,
    team_id    TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    kind       TEXT NOT NULL,
    endpoint   TEXT NOT NULL,
    request    TEXT NOT NULL,
    response   TEXT NOT NULL,
    truncated  INTEGER NOT NULL,
    redactions TEXT NOT NULL
);
CREATE INDEX request_bodies_request ON request_bodies (request_id);
CREATE INDEX request_bodies_ts ON request_bodies (ts);
CREATE INDEX request_bodies_org_ts ON request_bodies (org_id, ts);
CREATE INDEX usage_request ON usage (request_id);
