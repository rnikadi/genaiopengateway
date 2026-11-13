-- Video jobs created through the gateway (docs/modalities.md), so later
-- calls for a job reach the target that created it and only its owner.

CREATE TABLE video_jobs (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    team_id        TEXT NOT NULL,
    key_id         TEXT NOT NULL,
    model          TEXT NOT NULL,
    provider       TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    object         TEXT NOT NULL,
    created_at     BIGINT NOT NULL,
    updated_at     BIGINT NOT NULL
);
CREATE INDEX video_jobs_owner ON video_jobs (org_id, team_id, created_at);
