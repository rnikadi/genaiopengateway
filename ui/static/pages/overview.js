import { html, useState, api, qs, useLoad, useSession, Loading, Card, Bars, Columns, PageHeader, OrgPicker, defaultOrg, usageQuery, totals, usd, num, compact, when, Health } from '../lib.js';

// alertText summarizes an alert from its event and data.
export function alertText(a) {
  const d = a.data || {};
  switch (a.event) {
    case 'budget.threshold':
      return `A ${d.scope} budget reached ${Math.round(d.threshold * 100)}%: ${usd(d.spend_usd)} of ${usd(d.budget_usd)} ${d.period}.`;
    case 'mcp.tool_pending':
      return `MCP tool ${d.server}/${d.tool} ${d.changed ? 'changed its definition' : 'is new'} and awaits approval.`;
    case 'key.expiring':
      return `A key expires on ${when(d.expires_at)}.`;
    default:
      return a.event;
  }
}

export function Overview() {
  const s = useSession();
  const [org, setOrg] = useState(defaultOrg(s));
  const state = useLoad(async () => {
    const [byModel, byDay, pending, changed, alerts, providers] = await Promise.all([
      api('GET', usageQuery({ org, group_by: 'model' })),
      api('GET', usageQuery({ org, group_by: 'day' })),
      api('GET', '/admin/v1/mcp/tools?status=pending'),
      api('GET', '/admin/v1/mcp/tools?status=changed'),
      api('GET', '/admin/v1/alerts' + qs({ org_id: org, limit: 8 })),
      api('GET', '/admin/v1/providers'),
    ]);
    const failing = providers.data.filter((p) => p.health.errors > 0);
    return { byModel: byModel.data, byDay: byDay.data, review: pending.data.length + changed.data.length, alerts: alerts.data, failing };
  }, [org]);

  return html`
    <${PageHeader} title="Overview" sub="Month to date, in UTC."><${OrgPicker} value=${org} onChange=${setOrg} /><//>
    <${Loading} state=${state}>${(d) => {
      const t = totals(d.byModel);
      const byCost = [...d.byModel].sort((a, b) => b.cost_usd - a.cost_usd);
      return html`
        <div class="cards">
          <${Card} label="Spend" value=${usd(t.cost_usd)} href="#/usage" />
          <${Card} label="Requests" value=${num(t.requests)} />
          <${Card} label="Tokens" value=${compact(t.input_tokens + t.output_tokens)} sub=${`${compact(t.input_tokens)} in · ${compact(t.output_tokens)} out`} />
          <${Card} label="MCP tools to review" value=${num(d.review)} href="#/mcp" sub=${d.review ? 'New or changed definitions' : 'All approved'} />
          <${Card} label="Providers with errors" value=${num(d.failing.length)} href="#/providers" sub="In the last hour" />
        </div>
        ${d.failing.length > 0 && html`<section class="panel attention">
          <h2>Provider errors in the last hour</h2>
          <ul class="alerts">${d.failing.map((p) => html`<li key=${p.name}><strong>${p.name}</strong> <${Health} h=${p.health} /></li>`)}</ul>
        </section>`}
        <div class="grid2">
          <section class="panel"><h2>Daily spend</h2><${Columns} items=${d.byDay.map((r) => ({ label: r.group, value: r.cost_usd }))} format=${usd} /></section>
          <section class="panel"><h2>Spend by model</h2><${Bars} items=${byCost.slice(0, 8).map((r) => ({ label: r.group, value: r.cost_usd }))} format=${usd} /></section>
        </div>
        <section class="panel">
          <h2>Recent alerts</h2>
          ${d.alerts.length
            ? html`<ul class="alerts">${d.alerts.map((a) => html`<li key=${a.id}><span class="muted small">${when(a.ts)}</span> ${a.org_id && s.orgNames[a.org_id] ? html`<strong>${s.orgNames[a.org_id]}</strong> ` : ''}${alertText(a)}</li>`)}</ul>`
            : html`<p class="muted">No alerts. Budget thresholds, MCP tools awaiting approval and expiring keys show up here.</p>`}
        </section>`;
    }}<//>`;
}
