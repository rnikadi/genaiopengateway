# 0033: A2A agents behind the gateway, with tasks owned by their caller

- Status: accepted
- Date: 2026-11-11

## Context

Teams publish agents that speak the Agent2Agent (A2A) protocol, and other teams' agents call them. Platform teams want the same control over these calls as over model and MCP traffic: one place that holds the agents' credentials, decides who may call which agent, applies guardrails and rate limits, and records what happened.

A2A agents are called over JSON-RPC: `message/send` and `message/stream` start or continue a task, and `tasks/get`, `tasks/cancel` and `tasks/resubscribe` act on a task by id. An agent advertises itself with an agent card that names its URL and how to authenticate.

The gateway calls each agent with one set of credentials. That makes it a confused deputy unless it checks ownership itself: any caller who learned a task or context id could read or continue another team's task.

## Decision

- Agents are configured under `a2a.agents`, in the file or as stored objects (ADR 0023), with the agent's JSON-RPC URL, optional card URL, headers, timeout and guardrails. Each is served at `/a2a/{name}`, with its card at `/a2a/{name}/.well-known/agent-card.json`.
- The gateway serves the agent's card rewritten: its URL is the gateway's, its only transport is JSON-RPC, and its only security scheme is a bearer token, which is a gateway key. Push notifications and the authenticated extended card are removed. Cards need a key too, since they describe internal agents.
- Callers authenticate with gateway keys. Org, team and key policies gain `allowed_agents`; an empty list allows all, as for models and MCP servers. Each call counts against requests per minute.
- The gateway relays `message/send`, `message/stream`, `tasks/get`, `tasks/cancel` and `tasks/resubscribe`, and refuses every other method, including push notification configuration, which would have the agent call URLs the caller chooses.
- Task and context ids an agent returns are recorded in the store with the organization, team and key that caused them. A request naming a task or context recorded for another caller gets the A2A `TaskNotFound` error, as does one naming a task the gateway never saw. Without a database, calls that name a task are refused.
- An agent's `input` guardrails check the text parts of each message sent to it and may block or redact them; `output` guardrails check the text parts of results. An agent with output guardrails does not stream, since a stream would pass before it could be checked.

## Consequences

- Teams share agents without sharing credentials, and a leaked task id is useless to another team.
- Clients must send a key when they fetch the card. A2A SDKs let callers set headers on the card resolver.
- Agents with output guardrails lose streaming; their cards say so.
- Ownership costs one store write per new task or context and one read per call that names one.
