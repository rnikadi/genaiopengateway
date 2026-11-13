# A2A agents

The gateway puts agents that speak the [Agent2Agent (A2A) protocol](https://a2a-protocol.org) behind the same keys, policies, rate limits and telemetry as models and MCP servers. A team publishes its agent once; other teams call it through the gateway with their own gateway keys and never see the agent's credentials. Decisions are recorded in [ADR 0033](adr/0033-a2a-agents.md).

## Configuration

```yaml
a2a:
  public_url: https://gateway.example.com   # named in agent cards; default: mcp.public_url, or the request's URL
  # push: {enabled: true}                   # deliver push notifications; see below
  agents:
    planner:
      url: https://planner.internal.example.com/a2a     # the agent's JSON-RPC endpoint
      # rest_url: https://planner.internal.example.com/rest   # its HTTP+JSON binding, if it has one; url, rest_url or both
      # card_url: https://planner.internal.example.com/.well-known/agent-card.json   # default: at the url's host
      headers: {Authorization: "Bearer ${PLANNER_TOKEN}"}  # sent to the agent, never to callers
      timeout: 5m                                          # default 5m; see below
      guardrails: [no-pii]                                 # input: messages sent; output: results
      description: Plans team offsites                     # replaces the card's description
```

Agents can also be added and changed in the admin UI on the **Agents** page, or through the [config objects API](api.md) as kind `a2a_agents`. Header values are encrypted like other secrets.

Agent names use lowercase letters, digits, `-` and `_`.

## Calling an agent

Each agent is served at `/a2a/{name}`, with its card at `/a2a/{name}/.well-known/agent-card.json` (and the older `agent.json`). The gateway relays A2A 0.3 and A2A 1.0, over the bindings the agent offers:

| Binding | Needs | Requests |
| --- | --- | --- |
| JSON-RPC | `url` | `POST /a2a/{name}` with the methods of 0.3 (`message/send`, `tasks/get`, …) or of 1.0 (`SendMessage`, `GetTask`, …) |
| HTTP+JSON (REST) | `rest_url` | 1.0: `POST /a2a/{name}/message:send`, `POST …/message:stream`, `GET …/tasks/{id}`, `POST …/tasks/{id}:cancel`, `POST` or `GET …/tasks/{id}:subscribe`. 0.3: the same paths after `/a2a/{name}/v1` |

A request goes to the agent over the binding it arrived on, in the protocol version the caller used; the gateway does not translate between bindings or versions. The `A2A-Version` and `A2A-Extensions` headers are passed on. A caller that uses a binding the agent is not configured for gets `UnsupportedOperation`.

Point an A2A client at the card and give it a gateway key as a bearer token:

```python
import httpx
from a2a.client import A2ACardResolver, ClientFactory, ClientConfig

http = httpx.AsyncClient(headers={"Authorization": f"Bearer {GATEWAY_KEY}"})
card = await A2ACardResolver(http, "https://gateway.example.com/a2a/planner").get_agent_card()
client = ClientFactory(ClientConfig(httpx_client=http)).create(card)
```

The card a caller sees is the agent's own, with these changes:

- The interfaces are the gateway's, one for each binding the agent is configured for. A 1.0 card lists them in `supportedInterfaces`, at the protocol version the agent declares for that binding; a 0.3 card has the gateway's `url`, and HTTP+JSON in `additionalInterfaces` when both are configured. Bindings the gateway does not relay, such as gRPC, are removed.
- The only security scheme is `gateway`, an HTTP bearer token, which is a gateway key.
- `capabilities.pushNotifications` is true only when the agent sends them and the gateway is set to [deliver them](#push-notifications). The extended card is off (`supportsAuthenticatedExtendedCard`, or `capabilities.extendedAgentCard` in 1.0). `capabilities.streaming` is false when the agent has output guardrails.
- `signatures` are removed, since they cover the card as the agent wrote it.

Cards are cached for five minutes. Reading a card needs a key that may call the agent, since cards describe internal agents.

## What the gateway checks

1. **Authenticate** the gateway key (`Authorization: Bearer`, or `X-Api-Key`). Failure returns `401`.
2. **Authorize** the agent against `allowed_agents` at organization, team and key level, as `allowed_models` works. An empty list allows every agent. Failure returns `403` with `agent_not_allowed`.
3. **Rate limit**: each call counts against requests per minute at every level, and `429` comes back when one is spent.
4. **Operation**: sending a message (with or without a stream), getting a task, cancelling it and subscribing to it are relayed. Push notification configurations are relayed when the gateway is set to [deliver push notifications](#push-notifications), and refused otherwise. The extended card and every other operation are refused. Listing tasks is refused too: the agent knows every caller as the gateway, so its list would show other teams' tasks. Batch requests are refused.
5. **Ownership**: a task id in `tasks/*` or in a message's `taskId` must be one the gateway saw the agent return to the same organization and team (and, for static keys, the same key). A `contextId` may be new, but not another caller's. Anything else gets A2A's `TaskNotFound` error (`-32001`), so a leaked id tells another team nothing.
6. **Guardrails**: the agent's `input` guardrails check the text parts of the message, and may block it or redact it before it is sent. Its `output` guardrails check the text parts of the result; an agent with output guardrails does not stream, and `message/stream` and `tasks/resubscribe` get `UnsupportedOperation` (`-32004`).

Requests refused before the protocol (authentication, policy, rate limits, an unknown agent) get an HTTP error in the [gateway's error shape](api.md#errors). Everything after that is in the binding's shape: over JSON-RPC an error with HTTP 200, and over HTTP+JSON an HTTP status with a `google.rpc.Status` body whose `details` name the A2A error, such as `TASK_NOT_FOUND`. Over HTTP+JSON the agent's own errors pass through with their status, except `401` and `403`, which are about the gateway's credentials and become `500`.

The gateway records the owner of every task and context id an agent returns, from results and from each event of a stream, in the store's `a2a_objects` table. Without a database, calls that name a task or context are refused with `-32004`, and new messages still work.

## Push notifications

An agent that sends push notifications posts task updates to a URL its caller chose. Passed through a gateway as is, that would let any caller make the agent call whatever the agent can reach. So the gateway stands in between, and it is off until you turn it on:

```yaml
a2a:
  public_url: https://gateway.example.com   # where agents reach the gateway; required
  push:
    enabled: true
    # allowed_hosts: ["hooks.example.com", "*.corp.example.com"]
```

1. A caller sets a configuration, with a message or for a task it owns, in any binding and version: `tasks/pushNotificationConfig/set` or `CreateTaskPushNotificationConfig` over JSON-RPC, `POST …/tasks/{id}/pushNotificationConfigs` over HTTP+JSON.
2. The gateway keeps the caller's URL, token and credentials, the last two encrypted with `secrets.encryption_key`, and gives the agent a URL of its own instead: `{public_url}/a2a/{name}/push/{secret}`. The agent never sees the caller's URL or secrets.
3. The agent posts a notification to that URL. The secret in it is the agent's credential, so the agent's push sender must be able to reach `public_url`. The gateway accepts the notification only for the task the configuration belongs to; a configuration sent with a message belongs to the task of the first notification.
4. The gateway runs the agent's `output` guardrails on the text in the notification, then posts it to the caller's URL with `X-A2A-Notification-Token` and `Authorization` as the caller set them. The caller's `2xx` becomes the agent's `204`; anything else becomes `502`, so an agent that retries does so. A notification a guardrail blocks is dropped, and the agent gets `202`.

Getting, listing and deleting configurations are relayed to the agent, and the answers show the caller its own URL again. Deleting ends the gateway's URL. A configuration is honored for 30 days.

Where the gateway delivers to:

- With no `allowed_hosts`, a webhook must use HTTPS and be on the public internet. Internal names are refused when the configuration is set, and the connection is refused if the name resolves to a loopback, private, link-local or shared address, so DNS tricks do not help. Redirects are not followed.
- With `allowed_hosts`, webhooks must be on one of those hosts, which may be internal and may use HTTP.

Each delivery is logged as `a2a push notification` with the agent, task, owner and the webhook's status, and counted in `gog_a2a_requests_total` with the method `push/notify`.

## Timeouts and streams

`timeout` bounds a call that does not stream. For `message/stream` and `tasks/resubscribe` it bounds only the wait for the stream to start; the stream then runs as long as the agent keeps it open, and each event is passed on as it arrives.

## Telemetry

Every call is logged as `a2a request` with the request id, agent, method (named as in A2A 0.3 whatever the binding and version, such as `message/send`), status, organization, team, key and duration, and counted in `gog_a2a_requests_total` and `gog_a2a_request_duration_seconds` ([observability](observability.md)). Message contents are never logged. Guardrail decisions are recorded as for models.

## Not in this version

- The gRPC binding. Agents must offer JSON-RPC or HTTP+JSON.
- Translating between bindings or protocol versions: a caller uses a binding and version the agent offers.
- Listing tasks.
- The authenticated extended card.
- OAuth access tokens for callers: A2A callers use gateway keys.
- Usage and cost: an agent's own model calls are metered where it makes them, which can be through the gateway.
