-- SAML assertions already used to sign in (docs/saml.md), so a response
-- cannot be replayed on any replica before it expires.
CREATE TABLE saml_assertions (
    id         TEXT PRIMARY KEY,
    expires_at BIGINT NOT NULL
);
