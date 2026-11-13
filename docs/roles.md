# Roles and permissions

Operators of the admin UI and API get their access from role mappings in `sso.roles`, matched at sign-in against the identity provider's groups or the user's verified email ([admin-ui.md](admin-ui.md#roles)), or against their provisioned groups with [SCIM](scim.md). A role is a set of permissions, granted for:

- every organization (no `org`);
- one organization (`org`, by name or id);
- one team of that organization (`org` and `team`, by name or id).

There are three built-in roles, and owners can define more as custom roles.

## Built-in roles

| Role | Permissions |
| --- | --- |
| `owner` | Every permission, in every organization, and the gateway itself: organizations and their policies, configuration, settings, MCP tool approvals, alerts, sessions. Always gateway-wide. |
| `admin` | Every permission below. Needs an `org`. |
| `viewer` | `org.read`, `teams.read`, `keys.read`, `usage.read`, `audit.read`, `alerts.read`. |
| `developer` | `keys.self`, `playground.use`. Needs an `org`. Sees only **My keys and access** and the playground ([developer-portal.md](developer-portal.md)). |

The admin token acts as `owner`.

## Permissions

| Permission | Allows |
| --- | --- |
| `org.read` | Reading the organization and its policy |
| `teams.read` | Reading teams, their policies and which providers have team credentials |
| `teams.write` | Creating, changing and deleting teams. Organization-wide only: a team grant cannot change its own team's policy, which is the ceiling for its keys (the same rule as ADR 0030 for organizations). |
| `keys.read` | Listing and reading virtual keys |
| `keys.write` | Creating, changing, rotating and revoking virtual keys |
| `credentials.write` | Setting and removing a team's own provider credentials ([team-credentials.md](team-credentials.md)) |
| `usage.read` | Usage and cost, MCP calls, guardrail events, logged request metadata, batches and video jobs |
| `audit.read` | The audit log. Organization-wide only. |
| `alerts.read` | Alerts sent for budgets and expiring keys. Organization-wide only. |
| `bodies.read` | Logged request and response bodies ([body-logging.md](body-logging.md)), which is audited |
| `batches.write` | Cancelling batches |
| `cache.clear` | Clearing cached answers ([cache.md](cache.md)) |
| `playground.use` | Calling models from the playground as a team ([playground.md](playground.md)) |
| `keys.self` | Creating, rotating and revoking one's own keys in the developer portal, within `sso.portal` limits ([developer-portal.md](developer-portal.md)) |

Anything not tied to an organization, such as the configuration, settings and the audit log across all organizations, needs `owner`, or for reading, a gateway-wide grant with the permission.

## Custom roles

```yaml
sso:
  custom_roles:
    billing:
      description: Finance reads spend
      permissions: [org.read, usage.read, alerts.read]
    key-manager:
      permissions: [teams.read, keys.read, keys.write]
  roles:
    - {group: finance, role: billing}                                   # every organization
    - {group: acme-ops, role: key-manager, org: acme}                   # one organization
    - {group: search-leads, role: admin, org: acme, team: search}       # one team
```

A custom role's name is lowercase letters, digits, `-` and `_`, and cannot be `owner`, `admin`, `viewer` or `developer`. Startup, and saving in the UI, fails on an unknown permission or a mapping to a role that does not exist. Owners edit custom roles and mappings on the **Settings** page, under **Single sign-on**, and changes apply to signed-in users at once.

## Team grants

A grant for one team covers that team, its keys and their data, and nothing else in the organization:

- The user sees the organization's name, and in its team list only their teams.
- Usage, MCP calls and other records need a `team_id` or `key_id` filter for their team. Without one, the API answers `403`.
- The virtual keys page lists only their teams' keys.
- Team grants never cover organization-wide permissions: `teams.write`, `audit.read`, `alerts.read`, and clearing the whole organization's cache.

A mapping whose `team` does not exist is skipped, with a warning in the log, just like a mapping to an organization that does not exist.

## In the API

`GET /admin/v1/me` lists each grant with its `permissions`, and its `team_name` for team grants, so clients can hide what the API would refuse. The API checks every request on its own.
