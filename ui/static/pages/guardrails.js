import { html, useState, useEffect, api, qs, useSession, useLoad, Loading, PageHeader, OrgPicker, defaultOrg, Table, ErrorBox, Tabs, when, num } from '../lib.js';

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

// Guardrails shows what guardrails did and how they are defined
// (docs/guardrails.md).
export function Guardrails({ params }) {
  const [tab, setTab] = useState(params.get('tab') === 'definitions' ? 'definitions' : 'events');
  return html`
    <${PageHeader} title="Guardrails" sub="Checks on model and MCP calls. Matched text is never stored." />
    <${Tabs} tabs=${{ events: 'Events', definitions: 'Definitions' }} value=${tab} onChange=${setTab} />
    ${tab === 'events' ? html`<${Events} params=${params} />` : html`<${Definitions} />`}`;
}

const stageNames = { input: 'Input', output: 'Output', tool_input: 'Tool input', tool_output: 'Tool output' };

// check describes what a guardrail looks for.
function check(g) {
  switch (g.type) {
    case 'pii':
      return g.detect?.length ? `PII: ${g.detect.join(', ')}` : 'PII: every detector';
    case 'regex':
      return html`Regex: ${g.patterns.map((p) => html`<code class="small" key=${p}>${p}</code> `)}`;
    case 'model':
      return html`Classifier model <code>${g.model}</code>${g.unsafe_if ? html`, unsafe if <code>${g.unsafe_if}</code>` : ''}`;
    case 'webhook':
      return html`Webhook <code class="small">${g.webhook_host}</code>`;
    default:
      return g.type;
  }
}

function Definitions() {
  const state = useLoad(() => api('GET', '/admin/v1/guardrails'));
  return html`<${Loading} state=${state}>${(d) => html`
    <p class="muted small">Guardrails are defined in <code>guardrails</code> in the gateway's configuration. They run where an organization, team or key policy names them, and on the virtual MCP servers listed here.</p>
    <${Table} rows=${d.data} rowKey=${(g) => g.name} empty="No guardrails are configured." columns=${[
      { label: 'Guardrail', render: (g) => html`<code>${g.name}</code>` },
      { label: 'Checks', render: (g) => check(g) },
      { label: 'Action', render: (g) => html`<span class=${'badge ' + (actionBadge[g.action] ?? '')}>${g.action}</span>` },
      { label: 'Stages', render: (g) => g.stages.map((st) => stageNames[st] || st).join(', ') },
      { label: 'If the check fails', render: (g) => (g.on_error ? `${g.on_error === 'allow' ? 'Allow' : 'Block'}${g.timeout ? `; timeout ${g.timeout}` : ''}` : html`<span class="muted small">Runs in the gateway</span>`) },
      { label: 'Virtual MCP servers', render: (g) => g.virtual_servers.join(', ') },
    ]} />`}<//>`;
}

function Events({ params }) {
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
