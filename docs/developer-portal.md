# Developer portal

Developers get keys without filing a ticket. A person with the `developer` role signs in to the admin UI and lands on **My keys and access**, where they:

- create, rotate and revoke **their own** keys in their teams,
- see which models their team may use, and its budget and rate limits,
- see this month's requests, tokens and cost on their keys, by model,
- see the MCP servers their team may use, and request access to others,
- copy working code (curl, Python, Node.js, or environment variables for tools that read the OpenAI ones).

Developers see nothing else in the admin UI except the playground.

## Giving people the developer role

Map a group to `developer` for an organization, or one team of it:

```yaml
sso:
  roles:
    - {group: search-engineers, role: developer, org: acme, team: search}
    - {group: acme-engineers, role: developer, org: acme}   # every team in acme
  portal:
    max_keys_per_user: 5    # active keys per person, across teams; 1 to 100
    max_key_ttl: 90d        # the longest a key may last; 1h to 366d
```

The developer role always needs an `org`. It holds two permissions: `keys.self` and `playground.use` ([roles.md](roles.md)). A custom role can include `keys.self` too. Owners change the portal limits under **Settings**, **Sign-in and SSO**, **Developer portal**.

## What a portal key is

A portal key is an ordinary virtual key of the team. Its owner is the developer's email, lowercased, and it always expires. It may use what the team may use, within the team's and organization's limits, so the portal never gives a developer more than their team has. Admins see portal keys on the **Keys** page with their owner, and can revoke them like any other.

A developer can only rotate or revoke keys they own. A key that belongs to someone else returns 404, so its existence is not revealed. Rotating keeps the previous secret working for an hour by default, at most 7 days, so running services can switch over.

Each create, rotate and revoke is in the audit log with the developer as the actor and `via: portal`.

## Requesting MCP access

A developer can request an MCP server for their team with a reason. The request lands in the queue on the **MCP** page, where the team's admins approve or reject it, as with requests made through the API. Access applies to the whole team.

## API

The portal uses these endpoints, which need a signed-in person with an email (the admin token gets 403):

| Method and path | Does |
| --- | --- |
| `GET /admin/v1/portal` | Teams, models, limits, MCP servers, own keys and this month's use |
| `POST /admin/v1/portal/teams/{team_id}/keys` | Creates a key: `{"name": "laptop", "expires_in": "30d"}`. 409 `key_limit` when at the limit. |
| `POST /admin/v1/portal/keys/{key_id}/rotate` | Rotates: `{"grace": "1h"}` |
| `POST /admin/v1/portal/keys/{key_id}/revoke` | Revokes |
| `POST /admin/v1/portal/teams/{team_id}/requests` | Requests MCP access: `{"server": "docs", "reason": "…"}` |
