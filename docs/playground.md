# Admin UI: playground and missing actions

Operators need to try a model the way a team gets it, before they hand out a key, and to do from the browser the last two things the admin API offers but the UI does not. This is phase 2 of the plan tracked in issue #31 (issue #33).

## Playground

A page where an operator picks a team and a model, writes messages and sees the answer stream in, with the target that answered, tokens, cost and any guardrail decision.

- **As the team, not as a superuser.** The call runs with the team's policies: its organization's and its own allowed models, rate limits, budgets and guardrails. What works in the playground works for the team's keys, and what is blocked is blocked the same way. Key-level policies do not apply, because there is no key.
- **Who may use it:** owners, and admins of the team's organization, because a call spends the team's budget. Viewers cannot.
- **Metered like any request.** Usage is recorded under the team with key name `playground` and no key id, like OAuth MCP calls (ADR 0014). Each call is also written to the audit log as `playground.chat` with the model and request id, so spend in the playground is attributable to a person.
- **The real path.** The handler checks the operator's role, builds the team's principal and hands the request to the same model handler that serves `/v1/chat/completions`, so routing, fallback, guardrails, body logging, metrics and traces are the ones production uses. There is no second code path to drift.

API: `POST /admin/v1/playground/chat/completions?team_id=…` with an OpenAI chat completions body; the response is that endpoint's response, streamed when `stream` is true. The usual admin CSRF rules apply to cookie requests.

The same holds for the media tabs ([modalities.md](modalities.md)), each audited under its own action:

| Tab | API | Audit action |
| --- | --- | --- |
| Image | `POST /admin/v1/playground/images/generations` | `playground.image` |
| Text to speech | `POST /admin/v1/playground/audio/speech` | `playground.speech` |
| Transcription | `POST /admin/v1/playground/audio/transcriptions` (multipart) | `playground.transcription` |

The transcription upload is multipart, which a cookie request may send when its `Origin` is the gateway's own; browsers do not let other sites forge `Origin`. Every model on an `openai` provider lists every endpoint, so each tab keeps its own model choice. The page shows the generated images, plays the speech, and prices each call at the target's media prices.

The page lists the models the team may use that serve chat completions, keeps the conversation in the page only, and shows `X-Gateway-Target`, `X-Gog-Guardrails`, the usage the upstream reported, and the cost at the target's configured price.

## Other actions

- **Delete logged bodies** (owners): on the Requests page, pick an organization and a date; the UI calls `DELETE /admin/v1/requests?org_id=&before=` after a confirmation and reports how many were deleted. It is audited as today.
- **Provider health on the Overview:** a card with the number of providers that had errors in the last hour, linking to the Providers page, and a panel listing them.

## Testing

- Server tests: an admin's call is metered under the team and audited; a viewer and an admin of another organization get 403; the team's model allow list and guardrails apply; streaming works.
- The Playwright test sends a playground message as the owner and checks the answer and the target.
