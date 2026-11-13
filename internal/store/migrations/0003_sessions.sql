-- Admin UI sessions (docs/admin-ui.md). Only a hash of the cookie value is kept.

CREATE TABLE admin_sessions (
    id_hash    TEXT PRIMARY KEY,
    subject    TEXT NOT NULL,
    email      TEXT NOT NULL,
    name       TEXT NOT NULL,
    roles      TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL
);
CREATE INDEX admin_sessions_expires ON admin_sessions (expires_at);
