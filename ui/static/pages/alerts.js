import { html, useState, useEffect, api, qs, useSession, useLoad, Loading, PageHeader, OrgPicker, defaultOrg, Table, ErrorBox, when } from '../lib.js';
import { alertText } from './overview.js';

const events = { 'budget.threshold': 'Budget threshold', 'mcp.tool_pending': 'MCP tool awaiting approval', 'key.expiring': 'Key expiring' };

// Alerts lists alerts raised and, for owners, the webhooks they go to
// (docs/alerts.md).
export function Alerts({ params }) {
  const s = useSession();
  const [org, setOrg] = useState(params.get('org') ?? defaultOrg(s));
  const [event, setEvent] = useState('');
  const [list, setList] = useState({ items: [], more: false, loading: true });
  const [error, setError] = useState(null);

  const load = async (after) => {
    setError(null);
    try {
      const d = await api('GET', '/admin/v1/alerts' + qs({ org_id: org, event, limit: 50, after }));
      setList((l) => ({ items: after ? [...l.items, ...d.data] : d.data, more: d.has_more, loading: false }));
    } catch (err) {
      setError(err);
      setList((l) => ({ ...l, loading: false }));
    }
  };
  useEffect(() => {
    setList({ items: [], more: false, loading: true });
    load();
  }, [org, event]);

  return html`
    <${PageHeader} title="Alerts" sub="Budget thresholds, MCP tools awaiting approval and expiring keys, newest first." />
    <div class="filters">
      <${OrgPicker} value=${org} onChange=${setOrg} />
      <label class="inline"><span>Event</span>
        <select value=${event} onChange=${(e) => setEvent(e.target.value)}>
          <option value="">All</option>${Object.entries(events).map(([k, v]) => html`<option value=${k}>${v}</option>`)}
        </select>
      </label>
    </div>
    <${ErrorBox} error=${error} />
    ${list.loading
      ? html`<p class="muted">Loading…</p>`
      : html`<${Table} rows=${list.items} rowKey=${(a) => a.id} empty="No alerts yet." columns=${[
          { label: 'Time', render: (a) => when(a.ts) },
          { label: 'Event', render: (a) => events[a.event] || a.event },
          { label: 'Organization', render: (a) => s.orgNames[a.org_id] || a.org_id || '' },
          { label: 'Detail', render: (a) => alertText(a) },
        ]} />`}
    ${list.more && html`<p class="center"><button class="secondary" onClick=${() => load(list.items[list.items.length - 1].id)}>Load more</button></p>`}
    ${s.owner && html`<${Webhooks} />`}`;
}

function Webhooks() {
  const state = useLoad(() => api('GET', '/admin/v1/alerts/webhooks'));
  const [results, setResults] = useState({});
  const test = async (name) => {
    setResults((r) => ({ ...r, [name]: { sending: true } }));
    try {
      const d = await api('POST', '/admin/v1/alerts/test', { webhook: name });
      setResults((r) => ({ ...r, [name]: d }));
    } catch (err) {
      setResults((r) => ({ ...r, [name]: { ok: false, error: err.message } }));
    }
  };
  const result = (r) =>
    !r ? '' : r.sending ? html`<span class="muted small">Sending…</span>` : r.ok ? html`<span class="badge good">Delivered (${r.status})</span>` : html`<span class="badge bad" title=${r.error}>Failed${r.status ? ` (${r.status})` : ''}</span>`;
  return html`<section class="panel">
    <h2>Webhooks</h2>
    <${Loading} state=${state}>${(d) => html`
      <p class="muted small">Budget alerts fire at ${(d.budget_thresholds?.length ? d.budget_thresholds : [0.5, 0.8, 1]).map((t) => `${Math.round(t * 100)}%`).join(', ')} of a budget unless a policy sets its own. Webhooks are configured in <code>alerts.webhooks</code>; addresses are shown without their path, which often holds a token.</p>
      <${Table} rows=${d.data} rowKey=${(h) => h.name} empty="No webhooks are configured: alerts are only listed here." columns=${[
        { label: 'Webhook', render: (h) => html`<strong>${h.name}</strong><div class="muted small">${h.format === 'slack' ? 'Slack' : 'JSON'}${h.signed ? ', signed' : ''}</div>` },
        { label: 'Address', render: (h) => html`<code class="small">${h.host}</code>` },
        { label: 'Events', render: (h) => (h.events?.length ? h.events.map((e) => events[e] || e).join(', ') : 'All') },
        { label: 'Organization', render: (h) => h.org || 'All' },
        { label: '', render: (h) => html`<button class="secondary" onClick=${() => test(h.name)}>Send test</button> ${result(results[h.name])}` },
      ]} />`}<//>
  </section>`;
}
