-- Providers, models, guardrails, MCP servers and alert webhooks created
-- through the admin API (ADR 0023). spec is the config file's entry as
-- JSON without its secrets, which secrets holds encrypted.

CREATE TABLE config_objects (
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    spec       TEXT NOT NULL,
    secrets    TEXT NOT NULL,
    updated_at BIGINT NOT NULL,
    updated_by TEXT NOT NULL,
    PRIMARY KEY (kind, name)
);

-- One row whose rev moves with every write, so replicas notice changes
-- with one cheap read.
CREATE TABLE config_revision (
    id  INTEGER PRIMARY KEY,
    rev BIGINT NOT NULL
);
INSERT INTO config_revision (id, rev) VALUES (1, 0);
