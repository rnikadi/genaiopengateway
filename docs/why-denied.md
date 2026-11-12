# Why was this denied?

When a request is refused, the error says what refused it, such as `model_not_allowed` or `budget_exceeded`. The explainer goes further: for a key and a model or MCP tool, it runs every check a request goes through, says which one says no, at which level (organization, team or key), and what would change the answer.

## In the admin UI

- **Monitor, Why denied?** checks a key against a model and endpoint, or an MCP server and tool. Pick the key, the model or server, and select **Check**.
- **Requests, Refused** lists requests the gateway refused, newest first, with the reason given to the client. **Why?** opens the explainer for that request.
- **Keys** and **My keys and access** have a **Why denied?** link on each key.

Developers can explain their own keys from the developer portal ([developer-portal.md](developer-portal.md)). Anyone with `keys.read` on a key's team can explain it.

## The checks

| Check | Says no when |
| --- | --- |
| The key works | The key is revoked or expired. It warns when the key expires within 7 days. |
| The model exists | No model has that name. |
| Allowed models | The organization's, team's or key's `allowed_models` leave the model out. It names the level. |
| Cedar policies | The organization's Cedar policies forbid the request, with the policy's reason ([cedar.md](cedar.md)). Without a user, policies about users are checked without one. |
| Data residency | No target of the model is in a residency every level allows, or no target serves the endpoint. |
| Rate limits | A level's requests or tokens per minute are used up now. It shows each level's use this minute. |
| Budgets | A level's budget is spent. It shows each level's spend, and warns at 90%. |
| Guardrails | For a recorded request, the guardrail that blocked it and why. Otherwise it lists the guardrails that apply, which check content, so they can still refuse a request. |

For MCP, it checks the key, that the virtual server exists, the levels' `allowed_mcp_servers`, that the virtual server exposes the tool and whether it waits for approval ([mcp.md](mcp.md)), and Cedar.

The checks show how things stand when you ask. For a recorded request, the reason it was refused at the time is shown above them, since limits reset and policies change.

## What is recorded

The gateway records a model request it refuses for a policy, limit, Cedar rule, residency or guardrail, with its time, organization, team, key, model, status, error code and the message the client got. It does not record the request's content. Requests refused before the key is known (`invalid_api_key`) and malformed requests are not recorded.

Records are kept for `store.usage_retention`, like usage, and can be streamed as the `denials` record type ([streams.md](streams.md)).

## API

`POST /admin/v1/explain` takes one of:

```json
{"request_id": "req_01..."}
{"key_id": "key_01...", "model": "chat-default", "action": "chat.completions", "user": "ada@example.com"}
{"key_id": "key_01...", "server": "docs", "tool": "wiki_search"}
```

and returns `allowed`, a one-line `summary`, the key, the recorded `denial` if any, and `checks`, each with `id`, `label`, `status` (`pass`, `fail`, `warn` or `info`), `detail`, and for a failure a `fix` and a `link` to the admin UI page with the setting.

`GET /admin/v1/denials?org_id=&team_id=&key_id=&code=&from=&to=` lists recorded refusals, newest first, for operators with `usage.read`.
