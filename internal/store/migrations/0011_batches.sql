-- Batch input files and batches created through the gateway
-- (docs/batch.md), so later calls reach the target that holds them and
-- only their owner, and each batch is charged once.

CREATE TABLE batch_files (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    team_id        TEXT NOT NULL,
    key_id         TEXT NOT NULL,
    model          TEXT NOT NULL,
    provider       TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    purpose        TEXT NOT NULL,
    object         TEXT NOT NULL,
    created_at     BIGINT NOT NULL
);
CREATE INDEX batch_files_owner ON batch_files (org_id, team_id, created_at);

CREATE TABLE batches (
    id             TEXT PRIMARY KEY,
    org_id         TEXT NOT NULL,
    team_id        TEXT NOT NULL,
    key_id         TEXT NOT NULL,
    model          TEXT NOT NULL,
    provider       TEXT NOT NULL,
    upstream_model TEXT NOT NULL,
    input_file_id  TEXT NOT NULL,
    status         TEXT NOT NULL,
    object         TEXT NOT NULL,
    cost_usd       DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- charged_at is 0 until the batch is charged.
    charged_at     BIGINT NOT NULL DEFAULT 0,
    polled_at      BIGINT NOT NULL,
    created_at     BIGINT NOT NULL,
    updated_at     BIGINT NOT NULL
);
CREATE INDEX batches_owner ON batches (org_id, team_id, created_at);
CREATE INDEX batches_open ON batches (status, polled_at);
