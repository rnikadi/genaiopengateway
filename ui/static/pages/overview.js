import { html, useState, api, useLoad, useSession, Loading, Card, Bars, Columns, PageHeader, OrgPicker, defaultOrg, usageQuery, totals, usd, num, compact } from '../lib.js';

export function Overview() {
  const s = useSession();
  const [org, setOrg] = useState(defaultOrg(s));
  const state = useLoad(async () => {
    const [byModel, byDay, pending, changed] = await Promise.all([
      api('GET', usageQuery({ org, group_by: 'model' })),
      api('GET', usageQuery({ org, group_by: 'day' })),
      api('GET', '/admin/v1/mcp/tools?status=pending'),
      api('GET', '/admin/v1/mcp/tools?status=changed'),
    ]);
    return { byModel: byModel.data, byDay: byDay.data, review: pending.data.length + changed.data.length };
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
        </div>
        <div class="grid2">
          <section class="panel"><h2>Daily spend</h2><${Columns} items=${d.byDay.map((r) => ({ label: r.group, value: r.cost_usd }))} format=${usd} /></section>
          <section class="panel"><h2>Spend by model</h2><${Bars} items=${byCost.slice(0, 8).map((r) => ({ label: r.group, value: r.cost_usd }))} format=${usd} /></section>
        </div>`;
    }}<//>`;
}
