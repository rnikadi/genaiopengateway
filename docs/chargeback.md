# Chargeback and FOCUS exports

Finance teams need two things from the gateway: a statement of what each team spent, to charge it back, and the same spend in a format their cost tool imports next to the cloud bill. Both are CSV downloads of the usage the gateway already records.

Costs in these files are what the gateway computed from the prices configured on each model ([api.md](api.md#usage-query), [catalog.md](catalog.md)). They are not the provider's invoice: reconcile them with it, and fix a price in the configuration when they drift.

## Where

- In the admin UI: **Usage and cost**, **Exports for finance**. The files cover the organization, team and period chosen on the page.
- From the API, with the `usage.read` permission for the organization or team asked for; without `org_id`, an owner or a global viewer gets every organization:

```sh
curl -H "Authorization: Bearer $GATEWAY_ADMIN_TOKEN" \
  "https://gateway.example/admin/v1/usage/export?format=chargeback&org_id=org_…&from=2026-03-01T00:00:00Z&to=2026-04-01T00:00:00Z" \
  -o chargeback-march.csv
```

| Parameter | Meaning |
| --- | --- |
| `format` | `chargeback` (default) or `focus` |
| `org_id`, `team_id`, `key_id` | Limit the export. Organization admins and viewers must name their organization. |
| `from`, `to` | RFC 3339 times, from inclusive and to exclusive. Default: this month so far. Days are UTC days. |

Exports reach back past `store.usage_retention`: days whose usage rows are pruned are read from the daily rollups ([analytics.md](analytics.md)), which are kept for `store.rollup_retention`.

## The chargeback statement

One line per team, grouped by organization with the biggest spender first.

| Column | Meaning |
| --- | --- |
| `period_start`, `period_end` | The range asked for |
| `organization_id`, `organization`, `team_id`, `team` | Who. A deleted team shows its id as its name. Usage of keys that belong to no team has an empty team. |
| `requests`, `input_tokens`, `output_tokens`, `cached_input_tokens` | How much |
| `cost_usd` | Spend, to six decimals |
| `share_of_total` | The line's part of the file's total spend, from 0 to 1 |
| `cache_saved_usd` | What answers from the response cache would have cost |

## FOCUS

[FOCUS](https://focus.finops.org) is the FinOps Foundation's common format for cost and usage data. The export has one row per UTC day, team, upstream model and endpoint, with FOCUS 1.2 column names. Tools that read FOCUS take the file as it is; the mapping is:

| FOCUS column | Value |
| --- | --- |
| `BilledCost`, `EffectiveCost`, `ListCost`, `ContractedCost` | The gateway's computed cost. They are equal: the gateway knows one price per model. |
| `BillingCurrency` | `USD` |
| `BillingAccountId`, `BillingAccountName` | The organization |
| `SubAccountId`, `SubAccountName` | The team |
| `BillingPeriodStart`, `BillingPeriodEnd` | The calendar month of the charge |
| `ChargePeriodStart`, `ChargePeriodEnd` | The UTC day |
| `ChargeCategory`, `ChargeFrequency` | `Usage`, `Usage-Based` |
| `ChargeDescription` | For example `gpt-4.1-mini on openai (chat.completions)` |
| `ConsumedQuantity`, `ConsumedUnit`, `PricingQuantity`, `PricingUnit` | Input plus output tokens in `Tokens`; for media, `Images`, `Characters` or `Seconds` |
| `ProviderName`, `InvoiceIssuerName`, `ServiceName` | The provider's name in the gateway's configuration |
| `PublisherName` | Who makes the model, when the provider type says so (Anthropic, Google, Cohere and the speech and image providers); otherwise the provider's name |
| `ServiceCategory` | `AI and Machine Learning` |
| `SkuId` | The upstream model |
| `ResourceId`, `ResourceName`, `ResourceType` | The gateway's model name, and `Model` |
| `Tags` | JSON with `organization`, `team`, `endpoint` and `gateway_model` |

Columns that start with `x_` are the gateway's own, as FOCUS allows: requests, the token kinds, media amounts, cache hits and `x_CacheSavedCost`.

What the export does not have: commitment discounts, credits, taxes and invoice ids, which only the provider's bill knows. `ChargeClass` is empty because the gateway records no corrections.

## Spreadsheets

A team or organization name that starts with `=`, `+`, `-` or `@` is written with a leading apostrophe, so a spreadsheet shows it as text instead of running it as a formula.
