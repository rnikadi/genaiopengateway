-- Virtual keys (docs/virtual-keys.md): who a key belongs to, and the
-- secret it replaced in a rotation, which works until previous_expires_at.
ALTER TABLE api_keys ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN previous_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE api_keys ADD COLUMN previous_expires_at BIGINT;
