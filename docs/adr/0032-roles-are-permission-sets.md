# 0032: Roles are sets of permissions, granted for every organization, one organization or one team

- Status: accepted
- Date: 2026-11-11

## Context

ADR 0016 gave operators three fixed roles (owner, admin, viewer), granted gateway-wide or for one organization. Two requests kept coming:

- Roles narrower than viewer or admin, such as finance reading spend only, or an operations group managing keys without seeing request bodies.
- Roles for one team, so a team lead can manage their team's keys without access to the rest of the organization.

ADR 0009 plans Cedar for attribute-based rules, but neither request needs attributes. Both fit a role-based model with finer roles and a finer scope.

## Decision

- A role is a set of named permissions, such as `keys.write` or `usage.read`. `admin` holds them all. `viewer` holds the read permissions except `bodies.read`. `owner` holds every permission, and also the gateway-level actions, which are not permissions.
- `sso.custom_roles` defines more roles from the same fixed list. There are no wildcards and no denies.
- A mapping may name a `team` of its `org`. A team grant matches only checks for that team. Checks that are organization-wide (`teams.write`, `audit.read`, `alerts.read`, and a cache clear for the whole organization) ask with no team, so a team grant never satisfies them. This keeps ADR 0030's rule that a scope cannot raise its own ceiling.
- Every handler checks one permission with one scope (organization, team) through a single function. `GET /admin/v1/me` returns each grant's permissions, so the UI mirrors the check rather than knowing the roles.

## Consequences

- Existing mappings keep their meaning. The checks they pass are the same, apart from `bodies.read`, which viewers never had.
- A permission added later must be added to `admin` explicitly, and to the docs table. A test compares the two lists.
- Custom roles cannot express conditions, such as "keys whose owner is me". That is still left to Cedar (ADR 0009).
