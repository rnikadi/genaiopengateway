# Cedar policies

An organization's [policy](virtual-keys.md) says which models its keys may use, how fast and for how much. Some rules need more than that: a team may not use a model, a tool may only be called in office hours, keys named `internal-*` only work from the office network. Owners write such rules as [Cedar](https://www.cedarpolicy.com) policies for each organization ([ADR 0009](adr/0009-policy.md)).

The gateway evaluates them in-process with [cedar-go](https://github.com/cedar-policy/cedar-go), after the key, team and organization policies, for:

- every model request, batch and realtime session, and
- every MCP tool call.

An organization without Cedar policies is unaffected. One with policies follows Cedar's rule: **a request is allowed only when a policy permits it and none forbids it.** So most policy sets start with a policy that permits everything, then forbid the exceptions.

## Writing policies

```cedar
@id("allow-by-default")
permit (principal, action, resource);

// Interns may not use the large models.
@id("no-large-models-for-interns")
forbid (principal in Team::"team_01J...", action in Action::"model", resource)
when { resource.name like "*-large*" };

// Tools on the github server only in office hours (UTC).
@id("github-office-hours")
forbid (principal, action == Action::"call_tool", resource in MCPServer::"github")
unless { context.hour >= 8 && context.hour < 18 && context.weekday != "saturday" && context.weekday != "sunday" };

// Keys named internal-* only from the office network.
@id("internal-keys-on-site")
forbid (principal, action, resource)
when { principal.name like "internal-*" && context has ip && !context.ip.isInRange(ip("10.0.0.0/8")) };
```

Give each policy an `@id`: a denial names the policies that forbade the request, and without an id they are called `policy0`, `policy1` and so on.

### What a request looks like

| | Entity | Attributes | Parents |
|---|---|---|---|
| Principal | `Key::"<key id>"` | `name` | `Team::"<team id>"` |
| | `Team::"<team id>"` | `name` | `Org::"<org id>"` |
| Action | `Action::"<endpoint>"`, such as `chat.completions`, `embeddings`, `responses`, `messages`, `generate_content`, `images.generations`, `audio.speech`, `videos`, `realtime` or `batches` | | `Action::"model"` |
| | `Action::"call_tool"` | | `Action::"mcp"` |
| Resource | `Model::"<virtual model>"` | `name` | |
| | `Tool::"<server>/<tool>"` | `name`, `server` | `MCPServer::"<server>"` |

The context has:

| Field | Type | Value |
|---|---|---|
| `hour` | Long | Hour of the day, 0 to 23, in UTC |
| `weekday` | String | `monday` to `sunday`, in UTC |
| `stream` | Bool | Whether the response is streamed; realtime sessions are |
| `user` | String | The end user: the `user` field of an OpenAI-style request, or the OAuth subject of an MCP caller; empty otherwise |
| `ip` | ipaddr | The address the request came from, when it parses. Behind a load balancer this is the balancer's address. Test with `context has ip` first |

## Managing policies

On an organization's page in the admin UI, the **Cedar policies** panel shows the policies in force and their history. Owners add, edit and remove them; anyone who can read the organization can **Check a request** against them.

| Method and path | Who | What |
|---|---|---|
| `GET /admin/v1/orgs/{org_id}/cedar` | `org.read` | The policies in force, or a version with `?version=` |
| `PUT /admin/v1/orgs/{org_id}/cedar` | owner | Save `{"text": "..."}` as a new version. Text that does not parse is refused with 400 |
| `DELETE /admin/v1/orgs/{org_id}/cedar` | owner | Remove the policies, as a new empty version |
| `GET /admin/v1/orgs/{org_id}/cedar/versions` | `org.read` | Versions, newest first |
| `POST /admin/v1/orgs/{org_id}/cedar/check` | `org.read` | Decide a request without sending it |

Every save is a new version, kept in the database, and recorded in the audit log as `cedar.update` or `cedar.delete`. To roll back, read an old version and save its text again.

A check names a key (`key_id`) or a team (`team_id`, by id or name), and either a `model` with an `action` (default `chat.completions`) or a `server` and `tool`. It may add `user`, `ip`, `stream` and `time`, and `text` to try a draft instead of the policies in force:

```sh
curl -s -X POST localhost:8080/admin/v1/orgs/$ORG/cedar/check \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -d '{"team_id": "interns", "model": "gpt-4.1"}'
# {"allowed":false,"errors":[],"policies":["no-large-models-for-interns"],"reason":"forbidden by the Cedar policy no-large-models-for-interns"}
```

## When a request is denied

A model request gets `403` with the code `policy_denied` and a message naming the policies:

```json
{"error": {"message": "forbidden by the Cedar policy no-large-models-for-interns", "type": "policy_denied", "code": "policy_denied"}}
```

A tool call returns a tool error with the same reason, so the agent sees why. Tools are still listed; only calls are refused.

A policy that fails to evaluate (for example, reading an attribute that is not there) is skipped, as Cedar specifies, and logged as a warning. If an organization's stored policies stop parsing, which can only happen after a cedar-go upgrade changes the language, the gateway denies that organization's requests and logs an error rather than allowing them.

## Limits

- Policies are checked against the attributes above. There is no schema validation yet, so a misspelled attribute shows up as a skipped policy in the logs, not as an error on save. Use **Check a request** before saving.
- A document may be at most 256 KiB.
- Policies apply per organization. Team and key policies stay in their [policies](virtual-keys.md).
