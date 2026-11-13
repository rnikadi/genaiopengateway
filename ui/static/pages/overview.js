import { html, useState, api, qs, useLoad, useSession, Loading, Card, Bars, Columns, PageHeader, OrgPicker, defaultOrg, usageQuery, totals, usd, num, compact, when, Health, Ago } from '../lib.js';

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
    case 'budget.forecast':
      return `A ${d.scope} budget of ${usd(d.budget_usd)} is set to run out on ${when(d.exhausts_at)}, at ${usd(d.daily_usd)} a day.`;
    case 'spend.anomaly':
      return `Spend was ${usd(d.spend_24h_usd)} in 24 hours, ${d.factor} times the usual ${usd(d.usual_daily_usd)} a day.`;
    default:
      return a.event;
  }
}

const week = 7 * 864e5;

// Overview is Home: what needs someone's action first, then spend and
// traffic this month, and for owners a setup checklist until it is done
// (docs/admin-ui.md).
export function Overview() {
  const s = useSession();
  const [org, setOrg] = useState(defaultOrg(s));
  const state = useLoad(async () => {
    const [byModel, byDay, pending, changed, alerts, providers, requests] = await Promise.all([
      api('GET', usageQuery({ org, group_by: 'model' })),
      api('GET', usageQuery({ org, group_by: 'day' })),
      api('GET', '/admin/v1/mcp/tools?status=pending'),
      api('GET', '/admin/v1/mcp/tools?status=changed'),
      api('GET', '/admin/v1/alerts' + qs({ org_id: org, limit: 50 })),
      api('GET', '/admin/v1/providers'),
      api('GET', '/admin/v1/mcp/requests?status=pending').catch(() => ({ data: [] })),
    ]);
    const failing = providers.data.filter((p) => p.health.errors > 0);
    return {
      byModel: byModel.data, byDay: byDay.data, review: pending.data.length + changed.data.length,
      alerts: alerts.data, failing, providers: providers.data.length,
      access: (requests.data || []).filter((q) => q.can_decide),
    };
  }, [org]);

  return html`
    <${PageHeader} title="Home" sub="What needs attention, and spend and traffic this month in UTC."><${OrgPicker} value=${org} onChange=${setOrg} /><//>
    ${s.owner && html`<${SetupChecklist} />`}
    <${Loading} state=${state}>${(d) => {
      const t = totals(d.byModel);
      const byCost = [...d.byModel].sort((a, b) => b.cost_usd - a.cost_usd);
      const recent = d.alerts.filter((a) => Date.now() - new Date(a.ts).getTime() < week);
      const budgets = recent.filter((a) => a.event === 'budget.threshold' && (a.data?.threshold || 0) >= 0.8);
      const expiring = recent.filter((a) => a.event === 'key.expiring');
      const items = [
        d.review > 0 && { tone: 'warn', text: `${num(d.review)} MCP ${d.review === 1 ? 'tool is' : 'tools are'} new or changed and hidden from clients until reviewed.`, href: '#/mcp?tab=review', action: 'Review' },
        d.access.length > 0 && { tone: 'warn', text: `${num(d.access.length)} ${d.access.length === 1 ? 'team asks' : 'teams ask'} for access to an MCP server.`, href: '#/mcp?tab=review', action: 'Decide' },
        d.failing.length > 0 && { tone: 'bad', text: `${num(d.failing.length)} ${d.failing.length === 1 ? 'provider has' : 'providers have'} errors in the last hour: ${d.failing.map((p) => p.name).join(', ')}.`, href: '#/providers', action: 'Look' },
        budgets.length > 0 && { tone: 'warn', text: `${num(budgets.length)} ${budgets.length === 1 ? 'budget' : 'budgets'} passed 80% this week.`, href: '#/alerts?tab=alerts', action: 'See alerts' },
        expiring.length > 0 && { tone: 'warn', text: `${num(expiring.length)} ${expiring.length === 1 ? 'key expires' : 'keys expire'} soon.`, href: '#/keys', action: 'See keys' },
        d.providers === 0 && !s.owner && { tone: 'warn', text: 'No providers are configured, so the gateway has no models to serve. Ask an owner to add one.' },
      ].filter(Boolean);
      return html`
        <section class=${`panel${items.length ? ' attention' : ''}`} aria-labelledby="attention-title">
          <h2 id="attention-title">Needs attention</h2>
          ${items.length
            ? html`<ul class="todo">${items.map((i) => html`<li key=${i.text}><span class=${'dot ' + i.tone} aria-hidden="true"></span><span>${i.text}</span>${i.href && html`<a class="button secondary" href=${i.href}>${i.action}</a>`}</li>`)}</ul>`
            : html`<p class="muted">Nothing needs you right now.</p>`}
        </section>
        <div class="cards">
          <${Card} label="Spend" value=${usd(t.cost_usd)} href="#/usage" />
          <${Card} label="Requests" value=${num(t.requests)} href="#/requests" />
          <${Card} label="Tokens" value=${compact(t.input_tokens + t.output_tokens)} sub=${`${compact(t.input_tokens)} in · ${compact(t.output_tokens)} out`} />
          ${t.cache_hits > 0 && html`<${Card} label="Saved by the cache" value=${usd(t.saved_usd)} sub=${`${num(t.cache_hits)} answers`} />`}
        </div>
        <div class="grid2">
          <section class="panel"><h2>Daily spend</h2><${Columns} items=${d.byDay.map((r) => ({ label: r.group, value: r.cost_usd }))} format=${usd} /></section>
          <section class="panel"><h2>Spend by model</h2><${Bars} items=${byCost.slice(0, 8).map((r) => ({ label: r.group, value: r.cost_usd }))} format=${usd} /></section>
        </div>
        ${d.failing.length > 0 && html`<section class="panel">
          <h2>Provider errors in the last hour</h2>
          <ul class="alerts">${d.failing.map((p) => html`<li key=${p.name}><strong>${p.name}</strong> <${Health} h=${p.health} /></li>`)}</ul>
        </section>`}
        <section class="panel">
          <div class="panel-header"><h2>Recent alerts</h2><a href="#/alerts">All alerts</a></div>
          ${d.alerts.length
            ? html`<ul class="alerts">${d.alerts.slice(0, 8).map((a) => html`<li key=${a.id}><span class="muted small"><${Ago} t=${a.ts} /></span> ${a.org_id && s.orgNames[a.org_id] ? html`<strong>${s.orgNames[a.org_id]}</strong> ` : ''}${alertText(a)}</li>`)}</ul>`
            : html`<p class="muted">No alerts. Budget thresholds, MCP tools awaiting approval and expiring keys show up here.</p>`}
        </section>`;
    }}<//>`;
}

const hideKey = 'gog.setup-checklist.hidden';
const hidden = () => {
  try {
    return localStorage.getItem(hideKey) === '1';
  } catch {
    return false;
  }
};

// SetupChecklist walks an owner through a first useful setup. Each step is
// checked from the gateway's own state, and the list goes away once every
// step is done, or when hidden.
function SetupChecklist() {
  const [gone, setGone] = useState(hidden());
  const state = useLoad(async () => {
    if (gone) return null;
    const [providers, keys, usage, settings, directory] = await Promise.all([
      api('GET', '/admin/v1/providers'),
      api('GET', '/admin/v1/keys?limit=1'),
      api('GET', usageQuery({ period: '90d' })),
      api('GET', '/admin/v1/settings'),
      api('GET', '/admin/v1/directory').catch(() => ({ enabled: false })),
    ]);
    const f = Object.fromEntries(settings.fields.map((x) => [x.path, x.value]));
    return [
      { done: providers.data.length > 0, text: 'Add a provider', href: '#/setup', hint: 'Pick a preset; its models and prices come with it.' },
      { done: keys.data.length > 0, text: 'Create a team and a key', href: '#/orgs', hint: 'Applications call the gateway with a key of their team.' },
      { done: totals(usage.data).requests > 0, text: 'Send a first request', href: '#/playground', hint: 'From the playground, or with the snippet the setup wizard shows.' },
      { done: !!(f['sso.oidc.issuer'] || f['sso.saml.idp_metadata_url'] || f['sso.saml.idp_metadata']), text: 'Connect single sign-on', href: '#/settings/sign-in', hint: 'People sign in with your identity provider instead of the admin token.' },
      { done: (f['sso.roles'] || []).length > 0 || directory.enabled, text: 'Give people roles', href: '#/settings/sign-in', hint: 'Map identity provider groups to owner, admin or viewer.' },
    ];
  }, [gone]);
  if (gone || !state.data || state.data.every((x) => x.done)) return null;
  const steps = state.data;
  const left = steps.filter((x) => !x.done).length;
  const hide = () => {
    try {
      localStorage.setItem(hideKey, '1');
    } catch {
      // Private mode; it is hidden until the page is reloaded.
    }
    setGone(true);
  };
  return html`<section class="panel" aria-labelledby="setup-title">
    <div class="panel-header"><h2 id="setup-title">Set up your gateway <span class="muted small">${steps.length - left} of ${steps.length} done</span></h2>
      <button class="link" onClick=${hide}>Hide</button></div>
    ${!steps[0].done && html`<p>No providers are configured yet, so the gateway has no models to serve. The setup wizard adds a provider from a preset, creates a first key and sends a test request in a few minutes.</p>
      <p><a class="button" href="#/setup">Start the setup wizard</a></p>`}
    <ol class="checklist">${steps.map((x) => html`<li key=${x.text} class=${x.done ? 'done' : ''}>
      <span class="tick" aria-hidden="true">${x.done ? '✓' : ''}</span>
      <span>${x.done ? html`<span class="sr-only">Done: </span>${x.text}` : html`<a href=${x.href}>${x.text}</a><span class="muted small"> · ${x.hint}</span>`}</span>
    </li>`)}</ol>
  </section>`;
}
