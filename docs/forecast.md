# Spend forecasts and unusual spend

A budget alert at 80% says a team is nearly out of money. Two earlier signals give time to act: where each monthly budget is heading, and a day that cost far more than usual. Both come from the usage the gateway already records, need a store, and are checked every hour.

## Forecast

For every organization and team with a **monthly** budget:

- **Per day** is the spend of the last 7 days, divided by 7.
- **Projected** is the spend so far this month plus the per-day rate for the rest of the month.
- **Runs out** is when the budget is reached at that rate, if that is before the month ends.

The rate is a plain average on purpose: it can be checked by hand against the Usage page, and it follows a change in traffic within a week. It does not know about weekends, launches or month-end batch jobs, so read it as "if the last week repeats".

Daily budgets reset too soon to forecast, and total budgets have no end to forecast to, so neither is listed.

**Where it shows**

- **Usage and cost**, **Budget forecast**: each budget with spent, per day, projected and an outlook (lasts the month, runs out on a date, or used up).
- `GET /admin/v1/usage/forecast?org_id=`, with `usage.read` for the organization, or for everything without `org_id`:

```json
{"data": [{
  "scope": "team", "org_id": "org_…", "org_name": "acme", "team_id": "team_…", "team_name": "search",
  "budget_usd": 100, "spent_usd": 60, "daily_usd": 6, "projected_usd": 183,
  "period_start": "2026-03-01T00:00:00Z", "period_end": "2026-04-01T00:00:00Z",
  "exhausts_at": "2026-03-18T04:00:00Z"
}]}
```

Rows are ordered by projected share of the budget, highest first. `exhausted` is true once the budget is used up.

**The alert.** `budget.forecast` is raised once per budget and month, the first time the budget is set to run out before the month ends. Its data has `scope`, `budget_usd`, `spend_usd`, `daily_usd`, `projected_usd`, `exhausts_at`, `period` and `period_start`. It is not raised for a budget that is already used up: `budget.threshold` at 100% says that.

## Unusual spend

`spend.anomaly` is raised for a team, or for an organization's keys that belong to no team, when its spend in the last 24 hours is both:

- at least `alerts.spend_anomaly.factor` times its average day over the 14 days before that (default 3), and
- at least `alerts.spend_anomaly.min_usd` (default 5), so that a team going from 10 cents to 40 cents wakes nobody.

It is raised once per team and UTC day. A team with no spend in the 14 days before has no usual day to compare with and raises nothing; its budget still protects it. The data has `spend_24h_usd`, `usual_daily_usd`, `factor` and `threshold_factor`.

A leaked key, a retry loop and a batch job pointed at the wrong model all look like this. [Requests](admin-ui.md) and Usage grouped by key show which it is.

```yaml
alerts:
  spend_anomaly:
    factor: 3      # 0 turns the alert off; otherwise at least 1.5
    min_usd: 5
```

Both values can also be changed under **Settings, Notifications**.

## Sending them

Both alerts appear on the Alerts page and on Home whether or not a webhook wants them. To send them, name the events on a webhook ([alerts.md](alerts.md)):

```yaml
alerts:
  webhooks:
    - name: finance
      url: ${SLACK_WEBHOOK_URL}
      format: slack
      events: [budget.threshold, budget.forecast, spend.anomaly]
```

Existing webhooks do not start receiving the new events until they are added to their `events`.

## Limits

- The check runs hourly, so an alert can come up to an hour after the spend that caused it. Budgets, not these alerts, are what stops spend.
- Forecasts read the usage table; usage older than `store.usage_retention` does not count. Keep at least 15 days for the anomaly baseline.
- Prices are the ones configured on each model, as everywhere in the gateway.
