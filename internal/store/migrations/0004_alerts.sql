-- Alerts raised (docs/alerts.md). The unique key makes each alert fire
-- once across replicas.

CREATE TABLE alerts_sent (
    id         TEXT PRIMARY KEY,
    ts         BIGINT NOT NULL,
    event      TEXT NOT NULL,
    dedupe_key TEXT NOT NULL UNIQUE,
    org_id     TEXT NOT NULL,
    data       TEXT NOT NULL
);
CREATE INDEX alerts_sent_org ON alerts_sent (org_id, id);
