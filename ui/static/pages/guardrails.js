import { html, useState, useEffect, api, qs, useSession, PageHeader, OrgPicker, defaultOrg, Table, ErrorBox, when, num } from '../lib.js';

const actionBadge = { block: 'bad', redact: 'warn', log: '' };

// guardrailColumns describe events; matched text is never stored, only
// what kind of thing matched and how often.
export const guardrailColumns = [
  { label: 'Time', render: (e) => when(e.ts) },
  { label: 'Guardrail', render: (e) => html`<code>${e.guardrail}</code>` },
  { label: 'Stage', render: (e) => e.stage.replace('_', ' ') },
  { label: 'Action', render: (e) => html`<span class=${'badge ' + (actionBadge[e.action] ?? '')}>${e.action}</span>${e.failed ? html` <span class="badge warn" title="The check itself failed; on_error chose the action">check failed</span>` : ''}` },
  { label: 'Detail', render: (e) => e.reason || Object.entries(e.counts || {}).map(([k, n]) => `${n} ${k}`).join(', ') },
  { label: 'Latency', num: true, render: (e) => `${num(e.latency_ms)} ms` },
];

// Guardrails lists what guardrails blocked, redacted or logged
// (docs/guardrails.md).
export function Guardrails({ params }) {
  const s = useSession();
  const [org, setOrg] = useState(params.get('org') ?? defaultOrg(s));
  const [action, setAction] = useState('');
  const [list, setList] = useState({ items: [], more: false, loading: true });
  const [error, setError] = useState(null);

  const load = async (after) => {
    setError(null);
    try {
      const d = await api('GET', '/admin/v1/guardrail-events' + qs({ org_id: org, action, limit: 50, after }));
      setList((l) => ({ items: after ? [...l.items, ...d.data] : d.data, more: d.has_more, loading: false }));
    } catch (err) {
      setError(err);
      setList((l) => ({ ...l, loading: false }));
    }
  };
  useEffect(() => {
    setList({ items: [], more: false, loading: true });
    load();
  }, [org, action]);

  const columns = [
    ...guardrailColumns,
    { label: 'Request', render: (e) => html`<code class="small">${e.request_id}</code>` },
  ];
  return html`
    <${PageHeader} title="Guardrails" sub="What guardrails blocked, redacted or logged, newest first. Matched text is never stored." />
    <div class="filters">
      <${OrgPicker} value=${org} onChange=${setOrg} />
      <label class="inline"><span>Action</span>
        <select value=${action} onChange=${(e) => setAction(e.target.value)}>
          <option value="">All</option><option value="block">Blocked</option><option value="redact">Redacted</option><option value="log">Logged</option>
        </select>
      </label>
    </div>
    <${ErrorBox} error=${error} />
    ${list.loading ? html`<p class="muted">Loading…</p>` : html`<${Table} rows=${list.items} rowKey=${(e) => e.id} empty=${s.me.guardrails?.length ? 'No guardrail has matched yet.' : 'No guardrails are configured.'} columns=${columns} />`}
    ${list.more && html`<p class="center"><button class="secondary" onClick=${() => load(list.items[list.items.length - 1].id)}>Load more</button></p>`}`;
}
