import {
  html, useState, api, useLoad, useSession, Loading, Table, Bars, Columns, PageHeader, OrgPicker, defaultOrg, Card,
  periods, usageQuery, totals, downloadCSV, usd, num, compact, endpointNames, mediaSummary,
} from '../lib.js';

const groups = { model: 'Model', team: 'Team', key: 'Key', day: 'Day', provider: 'Provider', endpoint: 'Endpoint' };

export function Usage({ params }) {
  const s = useSession();
  const [f, setF] = useState({
    org: params.get('org') ?? defaultOrg(s),
    team: params.get('team') ?? '',
    group_by: params.get('group_by') ?? 'model',
    period: periods[params.get('period')] ? params.get('period') : 'month',
  });
  const set = (k) => (v) => setF({ ...f, [k]: v, ...(k === 'org' ? { team: '' } : {}) });

  const teams = useLoad(async () => (f.org ? (await api('GET', `/admin/v1/orgs/${encodeURIComponent(f.org)}/teams?limit=500`)).data : []), [f.org]);
  const state = useLoad(() => api('GET', usageQuery(f)), [f.org, f.team, f.group_by, f.period]);

  // Rows grouped by team or key show names where this page knows them.
  const names = { ...s.orgNames, ...Object.fromEntries((teams.data || []).map((t) => [t.id, t.name])) };
  const label = (g) => names[g] || (f.group_by === 'endpoint' && endpointNames[g]) || g;

  const csv = (rows) =>
    downloadCSV(
      `usage-${f.group_by}-${f.period}.csv`,
      [f.group_by, 'requests', 'input_tokens', 'output_tokens', 'cached_input_tokens', 'reasoning_tokens', 'images', 'characters', 'audio_seconds', 'video_seconds', 'cost_usd'],
      rows.map((r) => [r.group, r.requests, r.input_tokens, r.output_tokens, r.cached_input_tokens, r.reasoning_tokens, r.images, r.characters, r.audio_seconds, r.video_seconds, r.cost_usd]),
    );

  return html`
    <${PageHeader} title="Usage" sub="Requests, tokens and spend, in UTC. Spend uses the prices configured on each model." />
    <div class="filters">
      <${OrgPicker} value=${f.org} onChange=${set('org')} />
      ${f.org && html`<label class="inline"><span>Team</span>
        <select value=${f.team} onChange=${(e) => set('team')(e.target.value)}>
          <option value="">All teams</option>
          ${(teams.data || []).map((t) => html`<option key=${t.id} value=${t.id}>${t.name}</option>`)}
        </select></label>`}
      <label class="inline"><span>Period</span>
        <select value=${f.period} onChange=${(e) => set('period')(e.target.value)}>
          ${Object.entries(periods).map(([k, [l]]) => html`<option key=${k} value=${k}>${l}</option>`)}
        </select></label>
      <label class="inline"><span>Group by</span>
        <select value=${f.group_by} onChange=${(e) => set('group_by')(e.target.value)}>
          ${Object.entries(groups).map(([k, l]) => html`<option key=${k} value=${k}>${l}</option>`)}
        </select></label>
    </div>
    <${Loading} state=${state}>${(d) => {
      const t = totals(d.data);
      const items = d.data.map((r) => ({ label: f.group_by === 'day' ? r.group : label(r.group), value: r.cost_usd }));
      return html`
        <div class="cards">
          <${Card} label="Spend" value=${usd(t.cost_usd)} />
          <${Card} label="Requests" value=${num(t.requests)} />
          <${Card} label="Input tokens" value=${compact(t.input_tokens)} sub=${`${compact(t.cached_input_tokens)} cached`} />
          <${Card} label="Output tokens" value=${compact(t.output_tokens)} sub=${`${compact(t.reasoning_tokens)} reasoning`} />
          ${mediaSummary(t) && html`<${Card} label="Media" value=${mediaSummary(t)} />`}
        </div>
        <section class="panel">
          <h2>Spend by ${groups[f.group_by].toLowerCase()}</h2>
          ${f.group_by === 'day' ? html`<${Columns} items=${items} format=${usd} />` : html`<${Bars} items=${[...items].sort((a, b) => b.value - a.value).slice(0, 15)} format=${usd} />`}
        </section>
        <section class="panel">
          <div class="panel-header"><h2>Details</h2>${d.data.length > 0 && html`<button class="secondary" onClick=${() => csv(d.data)}>Download CSV</button>`}</div>
          <${Table}
            rowKey=${(r) => r.group}
            rows=${f.group_by === 'day' ? d.data : [...d.data].sort((a, b) => b.cost_usd - a.cost_usd)}
            empty="No usage in this period."
            columns=${[
              { label: groups[f.group_by], render: (r) => (f.group_by === 'team' && names[r.group] ? html`<a href=${`#/teams/${encodeURIComponent(r.group)}`}>${label(r.group)}</a>` : label(r.group)) },
              { label: 'Requests', num: true, render: (r) => num(r.requests) },
              { label: 'Input tokens', num: true, render: (r) => num(r.input_tokens) },
              { label: 'Cached', num: true, render: (r) => num(r.cached_input_tokens) },
              { label: 'Output tokens', num: true, render: (r) => num(r.output_tokens) },
              ...(d.data.some((r) => mediaSummary(r)) ? [{ label: 'Media', render: (r) => html`<span class="small">${mediaSummary(r)}</span>` }] : []),
              { label: 'Spend', num: true, render: (r) => usd(r.cost_usd) },
            ]}
          />
        </section>`;
    }}<//>`;
}

