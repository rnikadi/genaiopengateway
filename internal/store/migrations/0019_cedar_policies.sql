-- Cedar policies per organization (docs/cedar.md). Every save is a new
-- version, so the history is kept: the highest version is in force, and
-- an empty text means the organization has no policies.
CREATE TABLE cedar_policies (
    org_id     TEXT NOT NULL,
    version    BIGINT NOT NULL,
    text       TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    created_by TEXT NOT NULL,
    PRIMARY KEY (org_id, version)
);
