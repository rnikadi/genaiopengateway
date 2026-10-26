# Virtual keys

Applications call the gateway with a virtual key (`sk-gog-…`). The real provider credentials (OpenAI and Anthropic API keys, Vertex AI service accounts, AWS roles) stay in the gateway: they are never sent to clients and can be changed without touching any application. A virtual key belongs to a team, and narrows what its team and organization allow: models, MCP servers, RPM, TPM, budget, guardrails, cache and expiry. The gateway stores only a hash of each key.

All of that exists. This design adds what operators need to manage keys at scale: one page for every key, rotation without downtime, and an owner on each key.

## Keys page

A **Virtual keys** page in the admin UI lists every key the operator can read, across organizations and teams:

- columns: name, prefix, owner, team and organization, status (active, expiring within 7 days, expired, revoked, rotating), last used, created;
- filters: organization, team, status, and text search on name, prefix and owner;
- **Create** (pick the team first), **Edit** (name, owner, access, limits, expiry), **Rotate** and **Revoke**.

The key's detail shows its effective access: each policy field with the value that applies and the level it comes from (organization, team or key). The Organizations page keeps its per-team key list.

API: `GET /admin/v1/keys` with `org_id`, `team_id`, `status`, `q`, `limit` and `after`. Organization admins and viewers see only their organizations' keys, as elsewhere.

## Rotation

`POST /admin/v1/keys/{key_id}/rotate` with `{"grace": "24h"}` returns a new secret, once, like key creation. The key keeps its id, name, policy, usage and budget. The old secret keeps working until the grace period ends (default 24 hours, at most 30 days, `0` to stop it at once), so applications can switch over without downtime. A second rotation ends the previous grace period at once. Rotation is audited as `key.rotate`.

The key row gains `previous_hash` and `previous_expires_at`. The request path loads both hashes for the same key, and the old one gets the earlier of the key's expiry and the grace period's end, so the existing expiry check covers it. Revoking a key stops both secrets.

## Owner

A key gains an optional `owner`: the email of the person responsible or the name of a service, up to 200 characters. It is shown and searched on the Keys page, sent in the `key.expiring` alert, and recorded in the audit log. It does not grant anything: access still comes from the team and roles.

## Not in this design

- Self-service keys created by team members who are not operators. It needs team-level roles, which come with Cedar policies.
- Per-team provider credentials: a team storing its own provider key, used for its requests instead of the shared one, in [team-credentials.md](team-credentials.md).

## Testing

- Store: listing across teams with filters on SQLite and Postgres, rotation columns, the migration.
- Server: rotate returns a new secret; both secrets work during the grace period and only the new one after it; `grace: 0`; a second rotation; revoke stops both; scope of `GET /admin/v1/keys` per role; owner in audit and alerts.
- Browser: an owner creates a key with an owner from the Keys page, filters for it, rotates it and sees the new secret once, and revokes it.
