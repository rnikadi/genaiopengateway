# Team provider credentials

A team can store its own credential for a provider, such as its own OpenAI API key or Bedrock API key. Requests from the team's virtual keys then reach that provider with the team's credential instead of the shared one. Applications still hold only their virtual key, and routing, limits, budgets, guardrails, caching, usage and audit work as before. The decision was agreed with Ram on 2026-10-26, as the third part of the virtual keys plan ([virtual-keys.md](virtual-keys.md)).

## Who it is for

- Business units that pay for their own provider account.
- Teams with their own quota or a provider account in a region they must stay in.
- Platform teams whose internal customers bring their own key.

## Model

A team credential names a provider defined in the gateway (in the file or the UI) and holds the secret that replaces the provider's `api_key`:

| Provider type | What the team stores |
| --- | --- |
| `openai`, `anthropic`, `gemini`, `cohere`, `elevenlabs`, `deepgram`, `assemblyai`, `stability`, `fal` | An API key |
| `bedrock` | A Bedrock API key |
| `vertex` | A service account key (the JSON file Google Cloud gives you), and optionally the project to call. See [Vertex AI](#vertex-ai) |

Everything else (base URL, region, headers, timeouts) comes from the provider. The secret is encrypted with `secrets.encryption_key`, never returned, and shown as set or not set.

## Request path

When a request's key belongs to a team with a credential for a target's provider, the gateway calls that target with an adapter built from the provider and the team's secret. Adapters are built when credentials change, on every replica within `store.refresh_interval`, not per request.

- **No silent fallback to the shared credential.** If the team's credential fails, that target fails, and fallback moves to the model's next target as usual. The shared credential of the same provider is never used for that team's request, so spend does not move between accounts unnoticed.
- **Jobs** (batches, video) keep the team's credential: the background poller uses the credential of the job's team.
- **Usage** is recorded under the team as before. Costs are still computed from the model's prices, so budgets and reports keep working; every call of the team to that provider used the team's credential while it was set.

## Admin API and UI

| Method and path | Purpose |
| --- | --- |
| `GET /admin/v1/teams/{team_id}/credentials` | The team's credentials: `provider`, `kind` (`api_key` or `service_account`), `set`, `updated_at`, `updated_by` |
| `PUT /admin/v1/teams/{team_id}/credentials/{provider}` | Set or replace, with `{"api_key": "..."}`, or for `vertex` with `{"service_account": {...}, "project": "..."}` |
| `DELETE /admin/v1/teams/{team_id}/credentials/{provider}` | Remove; the team uses the shared credential again |

Owners and the team's organization admins can change them; viewers see whether each is set. Changes are audited as `team.credential.set` and `team.credential.delete`, without the secret. The team page gets a **Provider credentials** panel listing the gateway's providers with Set, Replace and Remove.

## Vertex AI

Vertex AI has no API keys: a caller is a Google identity, and a request is billed to the project in its URL. So a team's credential for a `vertex` provider is a service account key, and the team's requests go to the team's project.

```sh
curl -X PUT http://localhost:8080/admin/v1/teams/$TEAM/credentials/vertex \
  -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  -d "{\"service_account\": $(cat team-key.json)}"
```

- `service_account` is the key file's content, as a JSON object or as text. In the UI, paste the file.
- `project` is optional and defaults to the key's `project_id`. Set it when the service account lives in one project and calls Vertex AI in another.
- The location, timeouts and headers still come from the provider.
- The service account needs the `Vertex AI User` role (`roles/aiplatform.user`) in that project, and the models the gateway routes to must be enabled there.

Only keys of type `service_account` are accepted. Workload identity federation files (`external_account`) and user credentials are refused, because they make the gateway fetch tokens from addresses or files the file itself names. For the same reason a key's `token_uri` must be Google's (`https://oauth2.googleapis.com/token`), and the gateway always uses that address whatever the key says.

The gateway signs a token request with the key, caches the access token and renews it before it expires, as it does for the shared credential. The list shows the credential as set, with kind `service_account`; the key is never returned, and the audit entry carries the project and not the key or the account's email.

## Not in this design

- AWS access keys or roles per team.
- Credentials per key or per organization. A team is the unit that owns a provider account; an organization-wide credential can be added later if asked for.
- Testing a credential before saving it. The first request shows whether it works, and the provider's errors are returned as usual.

## Testing

- Store: CRUD on SQLite and Postgres, encrypted at rest.
- Server: a team key's request carries the team's secret upstream, another team's carries the shared one; no fallback to the shared credential of the same provider; refused for unknown providers; a Vertex AI service account key sends the team's requests to its project with its own token, and other credential types, other token addresses and broken keys are refused; needs the encryption key; scope per role; audit without the secret; batch polling uses the team's credential.
- Browser: an owner sets a team's credential on the team page, sees it as set, and removes it.
