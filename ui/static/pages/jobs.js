import { html, useState, useEffect, api, qs, useSession, PageHeader, OrgPicker, defaultOrg, Table, ErrorBox, Tabs, Confirm, when, usd, num } from '../lib.js';

const batchStatuses = ['validating', 'in_progress', 'finalizing', 'cancelling', 'completed', 'failed', 'expired', 'cancelled'];
const done = (status) => ['completed', 'failed', 'expired', 'cancelled'].includes(status);
const statusClass = (status) => ({ completed: 'good', failed: 'bad', expired: 'bad', cancelled: 'warn', cancelling: 'warn' })[status] || '';

// Jobs lists batches and video jobs, which run after the request that
// started them (docs/batch.md, docs/modalities.md). Contents stay with
// the team: only metadata is shown.
export function Jobs({ params }) {
  const s = useSession();
  const [org, setOrg] = useState(params.get('org') ?? defaultOrg(s));
  const [tab, setTab] = useState(params.get('tab') || 'batches');
  const [status, setStatus] = useState('');
  const [list, setList] = useState({ items: [], more: false, loading: true });
  const [error, setError] = useState(null);
  const [cancelling, setCancelling] = useState(null);

  const load = async (after) => {
    setError(null);
    try {
      const path = tab === 'batches' ? '/admin/v1/batches' : '/admin/v1/videos';
      const d = await api('GET', path + qs({ org_id: org, status: tab === 'batches' ? status : '', limit: 50, after }));
      setList((l) => ({ items: after ? [...l.items, ...d.data] : d.data, more: d.has_more, loading: false }));
    } catch (err) {
      setError(err);
      setList((l) => ({ ...l, loading: false }));
    }
  };
  useEffect(() => {
    setList({ items: [], more: false, loading: true });
    load();
  }, [org, tab, status]);

  const cancel = async (b) => {
    const updated = await api('POST', `/admin/v1/batches/${encodeURIComponent(b.id)}/cancel`);
    setList((l) => ({ ...l, items: l.items.map((x) => (x.id === b.id ? updated : x)) }));
  };

  const common = [
    { label: 'Created', render: (j) => when(j.created_at) },
    { label: 'Team', render: (j) => (j.team_id === 'team_static' ? html`<span class="muted small">Config file key</span>` : html`<a href=${`#/teams/${encodeURIComponent(j.team_id)}`}><code>${j.team_id}</code></a>`) },
    { label: 'Model', render: (j) => j.model },
    { label: 'Target', render: (j) => html`<code>${j.provider}/${j.upstream_model}</code>` },
  ];
  const batchColumns = [
    ...common,
    { label: 'Status', render: (b) => html`<span class=${'badge ' + statusClass(b.status)}>${b.status.replace('_', ' ')}</span>` },
    { label: 'Requests', num: true, render: (b) => {
      const c = b.object?.request_counts;
      return c ? html`${num(c.completed)} / ${num(c.total)}${c.failed > 0 && html` <span class="badge bad">${num(c.failed)} failed</span>`}` : html`<span class="muted">—</span>`;
    } },
    { label: 'Cost', num: true, render: (b) => (b.charged_at ? usd(b.cost_usd) : html`<span class="muted small">When finished</span>`) },
    { label: '', render: (b) => !done(b.status) && b.status !== 'cancelling' && s.canWrite(b.org_id) && html`<button class="secondary small" onClick=${() => setCancelling(b)}>Cancel</button>` },
  ];
  const videoColumns = [
    ...common,
    { label: 'Status', render: (v) => html`<span class=${'badge ' + statusClass(v.object?.status)}>${v.object?.status || 'unknown'}</span>` },
    { label: 'Seconds', num: true, render: (v) => v.object?.seconds ?? '' },
    { label: 'Updated', render: (v) => when(v.updated_at) },
  ];

  return html`
    <${PageHeader} title="Jobs" sub="Batches and video jobs, newest first. Batches are charged once, when they finish; their files and results stay with the team." />
    <${Tabs} tabs=${{ batches: 'Batches', videos: 'Video jobs' }} value=${tab} onChange=${setTab} />
    <div class="filters">
      <${OrgPicker} value=${org} onChange=${setOrg} />
      ${tab === 'batches' && html`<label class="inline"><span>Status</span>
        <select value=${status} onChange=${(e) => setStatus(e.target.value)}>
          <option value="">Any</option>${batchStatuses.map((v) => html`<option value=${v}>${v.replace('_', ' ')}</option>`)}
        </select></label>`}
    </div>
    <${ErrorBox} error=${error} />
    ${list.loading ? html`<p class="muted">Loading…</p>` : html`<${Table}
      rows=${list.items}
      empty=${tab === 'batches' ? 'No batches yet. Teams start them with the OpenAI SDK through /v1/batches.' : 'No video jobs yet.'}
      columns=${tab === 'batches' ? batchColumns : videoColumns}
    />`}
    ${list.more && html`<p class="center"><button class="secondary" onClick=${() => load(list.items[list.items.length - 1].id)}>Load more</button></p>`}
    ${cancelling && html`<${Confirm} title="Cancel batch" action="Cancel batch"
      message=${`Cancel ${cancelling.id}? Requests already done are kept and charged; the rest are not run. This is recorded in the audit log.`}
      onConfirm=${() => cancel(cancelling)} onClose=${() => setCancelling(null)} />`}`;
}
