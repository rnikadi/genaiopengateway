-- Units media endpoints are billed by (docs/modalities.md). Earlier rows
-- read as zero.
ALTER TABLE usage ADD COLUMN images BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage ADD COLUMN characters BIGINT NOT NULL DEFAULT 0;
ALTER TABLE usage ADD COLUMN audio_seconds DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE usage ADD COLUMN video_seconds DOUBLE PRECISION NOT NULL DEFAULT 0;
