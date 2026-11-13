-- Daily usage rollups (docs/analytics.md): one row per UTC day, key,
-- model, target and endpoint. They outlive the usage rows they sum, so
-- reports reach back past store.usage_retention. day is days since
-- 1970-01-01.
CREATE TABLE usage_daily (
    day                 BIGINT NOT NULL,
    org_id              TEXT NOT NULL,
    team_id             TEXT NOT NULL,
    key_id              TEXT NOT NULL,
    endpoint            TEXT NOT NULL,
    model               TEXT NOT NULL,
    provider            TEXT NOT NULL,
    upstream_model      TEXT NOT NULL,
    requests            BIGINT NOT NULL,
    errors              BIGINT NOT NULL,
    input_tokens        BIGINT NOT NULL,
    output_tokens       BIGINT NOT NULL,
    cached_input_tokens BIGINT NOT NULL,
    reasoning_tokens    BIGINT NOT NULL,
    images              BIGINT NOT NULL,
    characters          BIGINT NOT NULL,
    audio_seconds       DOUBLE PRECISION NOT NULL,
    video_seconds       DOUBLE PRECISION NOT NULL,
    cost_usd            DOUBLE PRECISION NOT NULL,
    cache_hits          BIGINT NOT NULL,
    saved_usd           DOUBLE PRECISION NOT NULL,
    latency_ms_sum      BIGINT NOT NULL,
    PRIMARY KEY (day, org_id, team_id, key_id, endpoint, model, provider, upstream_model)
);
CREATE INDEX usage_daily_org_day ON usage_daily (org_id, day);
