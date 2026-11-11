# MCP catalog and access requests

The [MCP gateway](mcp.md) serves virtual servers, and a policy's `allowed_mcp_servers` decides which of them each organization, team and key may use. The catalog lets teams find the servers that exist and ask for the ones they are not allowed yet, without someone editing the team's policy by hand.

## The catalog

The **Catalog** panel on the admin UI's MCP page lists every virtual server: its description, endpoint (`/mcp/<name>`), upstream servers and guardrails. Pick a team to see its access to each:

| Access | Meaning |
|---|---|
| Allowed | The team's and the organization's policies allow it. Keys of the team can connect, unless a key's own policy leaves it out |
| Requested | A request is waiting for a decision |
| Not allowed | The team's policy lists other servers. The team can request it |
| Not allowed for the organization | The organization's policy leaves it out. The team can request it, but an owner has to allow it for the organization before it can be approved |

Anyone who can read a team (`teams.read`, including a [team grant](roles.md)) sees its catalog and can request access for it, with a reason.

## Deciding

Requests show in the **Access requests** panel to everyone who can read the team. Operators with `teams.write` for the whole organization (owners and organization admins, or a custom role with that permission) approve or deny them, with an optional note the requester sees. A team grant cannot decide, since approving changes the team's policy, which team grants may not do ([ADR 0032](adr/0032-roles-are-permission-sets.md)).

Approving adds the server to the team's `allowed_mcp_servers`, and the change applies at once. A team that already allows every server (an empty list) needs nothing added. Approval is refused while the organization's policy leaves the server out: an owner adds it to the organization first, which opens it to all of the organization's teams that do not list their own servers.

Every request, approval and denial is in the audit log as `mcp_request.create`, `mcp_request.approve` or `mcp_request.deny`. The team's policy change is logged as `team.update`, which names the request.

## API

| Method and path | Who | What |
|---|---|---|
| `GET /admin/v1/mcp/catalog?team_id=` | any operator; `teams.read` for the team | Virtual servers, with `access` and `request_id` for the team |
| `POST /admin/v1/mcp/requests` | `teams.read` for the team | `{"team_id", "server", "reason"}`. One pending request per team and server |
| `GET /admin/v1/mcp/requests?status=&org_id=` | `teams.read` | Requests of the teams the operator can read, newest first, with `team_name` and `can_decide` |
| `POST /admin/v1/mcp/requests/{id}/approve` | `teams.write` for the organization | `{"note"}` is optional |
| `POST /admin/v1/mcp/requests/{id}/deny` | `teams.write` for the organization | `{"note"}` is optional |

A request for a server the team may already use is refused with 409, as are a second pending request and a decision on a request that was already decided. Deleting a team deletes its requests.
