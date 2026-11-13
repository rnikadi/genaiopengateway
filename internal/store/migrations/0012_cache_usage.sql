-- Response cache hits (docs/cache.md): cache_hit is 1 for an answer
-- served from the cache, and saved_usd the cost of the answer it reused.
-- Earlier rows read as zero.
ALTER TABLE usage ADD COLUMN cache_hit BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage ADD COLUMN saved_usd DOUBLE PRECISION NOT NULL DEFAULT 0;
