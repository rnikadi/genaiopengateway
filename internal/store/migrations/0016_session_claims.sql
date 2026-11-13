-- The identity provider's view of an SSO session's user, so roles are
-- resolved on each request and changes to sso.roles apply at once.
-- NULL for admin token sessions.
ALTER TABLE admin_sessions ADD COLUMN claims TEXT;
