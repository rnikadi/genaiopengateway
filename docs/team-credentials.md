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
| `openai`, `anthropic`, `gemini` | An API key |
| `bedrock` | A Bedrock API key |
| `vertex` | Not supported yet: Vertex AI authenticates with a service account, which needs its own design |

Everything else (base URL, region, headers, timeouts) comes from the provider. The secret is encrypted with `secrets.encryption_key`, never returned, and shown as set or not set.

## Request path

When a request's key belongs to a team with a credential for a target's provider, the gateway calls that target with an adapter built from the provider and the team's secret. Adapters are built when credentials change, on every replica within `store.refresh_interval`, not per request.

- **No silent fallback to the shared credential.** If the team's credential fails, that target fails, and fallback moves to the model's next target as usual. The shared credential of the same provider is never used for that team's request, so spend does not move between accounts unnoticed.
- **Jobs** (batches, video) keep the team's credential: the background poller uses the credential of the job's team.
- **Usage** is recorded under the team as before. Costs are still computed from the model's prices, so budgets and reports keep working; every call of the team to that provider used the team's credential while it was set.

## Admin API and UI

| Method and path | Purpose |
| --- | --- |
| `GET /admin/v1/teams/{team_id}/credentials` | The team's credentials: `provider`, `set`, `updated_at`, `updated_by` |
| `PUT /admin/v1/teams/{team_id}/credentials/{provider}` | Set or replace, with `{"api_key": "..."}` |
| `DELETE /admin/v1/teams/{team_id}/credentials/{provider}` | Remove; the team uses the shared credential again |

Owners and the team's organization admins can change them; viewers see whether each is set. Changes are audited as `team.credential.set` and `team.credential.delete`, without the secret. The team page gets a **Provider credentials** panel listing the gateway's providers with Set, Replace and Remove.

## Not in this design

- Vertex AI service accounts, and AWS access keys or roles per team.
- Credentials per key or per organization. A team is the unit that owns a provider account; an organization-wide credential can be added later if asked for.
- Testing a credential before saving it. The first request shows whether it works, and the provider's errors are returned as usual.

## Testing

- Store: CRUD on SQLite and Postgres, encrypted at rest.
- Server: a team key's request carries the team's secret upstream, another team's carries the shared one; no fallback to the shared credential of the same provider; refused for `vertex` and unknown providers; needs the encryption key; scope per role; audit without the secret; batch polling uses the team's credential.
- Browser: an owner sets a team's credential on the team page, sees it as set, and removes it.
