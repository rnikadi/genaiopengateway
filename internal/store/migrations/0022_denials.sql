-- Requests the gateway refused, and why (docs/why-denied.md).
CREATE TABLE denials (
    request_id TEXT PRIMARY KEY,
    ts         BIGINT NOT NULL,
    org_id     TEXT NOT NULL,
    team_id    TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    endpoint   TEXT NOT NULL,
    model      TEXT NOT NULL,
    status     INTEGER NOT NULL,
    code       TEXT NOT NULL,
    reason     TEXT NOT NULL
);
CREATE INDEX denials_org ON denials (org_id, ts);
CREATE INDEX denials_key ON denials (key_id, ts);
CREATE INDEX denials_ts ON denials (ts);
