-- Users and groups provisioned by the identity provider over SCIM 2.0
-- (docs/scim.md). Group memberships of provisioned users replace the
-- groups claim when their roles are resolved.
CREATE TABLE scim_users (
    id            TEXT PRIMARY KEY,
    user_name     TEXT NOT NULL,
    user_key      TEXT NOT NULL UNIQUE,
    external_id   TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    email         TEXT NOT NULL,
    active        INTEGER NOT NULL,
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL
);
CREATE INDEX scim_users_email ON scim_users (email);
CREATE TABLE scim_groups (
    id            TEXT PRIMARY KEY,
    display_name  TEXT NOT NULL,
    group_key     TEXT NOT NULL UNIQUE,
    external_id   TEXT NOT NULL,
    created_at    BIGINT NOT NULL,
    updated_at    BIGINT NOT NULL
);
CREATE TABLE scim_members (
    group_id TEXT NOT NULL,
    user_id  TEXT NOT NULL,
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX scim_members_user ON scim_members (user_id);
