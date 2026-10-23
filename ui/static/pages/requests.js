import { html, useState, useEffect, api, qs, useLoad, useSession, PageHeader, OrgPicker, defaultOrg, Table, Loading, ErrorBox, JSONView, when, usd, num } from '../lib.js';

// Requests lists recent model requests; those with a logged body link to
// it (docs/body-logging.md).
export function Requests({ params }) {
  const s = useSession();
  const [org, setOrg] = useState(params.get('org') ?? defaultOrg(s));
  const [list, setList] = useState({ items: [], more: false, loading: true });
  const [error, setError] = useState(null);

  const load = async (after) => {
    setError(null);
    try {
      const d = await api('GET', '/admin/v1/requests' + qs({ org_id: org, limit: 50, after }));
      setList((l) => ({ items: after ? [...l.items, ...d.data] : d.data, more: d.has_more, loading: false }));
    } catch (err) {
      setError(err);
      setList((l) => ({ ...l, loading: false }));
    }
  };
  useEffect(() => {
    setList({ items: [], more: false, loading: true });
    load();
  }, [org]);

  const body = (r) => {
    if (!r.has_body) return html`<span class="muted">—</span>`;
    if (!s.canWrite(r.org_id)) return html`<span class="muted small" title="Reading bodies needs the owner or admin role">Logged</span>`;
    return html`<a href=${`#/requests/${encodeURIComponent(r.request_id)}`}>View</a>`;
  };

  return html`
    <${PageHeader} title="Requests" sub="Recent model requests, newest first. Bodies are stored only where a policy turns on log_bodies." />
    <div class="filters"><${OrgPicker} value=${org} onChange=${setOrg} /></div>
    <${ErrorBox} error=${error} />
    ${list.loading ? html`<p class="muted">Loading…</p>` : html`<${Table}
      rows=${list.items}
      rowKey=${(r) => r.org_id + r.request_id}
      empty="No requests yet."
      columns=${[
        { label: 'Time', render: (r) => when(r.ts) },
        { label: 'Model', render: (r) => r.model },
        { label: 'Status', render: (r) => html`<span class=${'badge ' + (r.status === 200 ? 'good' : 'bad')}>${r.status}</span>` },
        { label: 'Tokens', num: true, render: (r) => `${num(r.input_tokens)} / ${num(r.output_tokens)}` },
        { label: 'Cost', num: true, render: (r) => usd(r.cost_usd) },
        { label: 'Latency', num: true, render: (r) => `${num(r.latency_ms)} ms` },
        { label: 'Body', render: body },
      ]}
    />`}
    ${list.more && html`<p class="center"><button class="secondary" onClick=${() => load(list.items[list.items.length - 1].request_id)}>Load more</button></p>`}`;
}

// RequestBody shows one logged body. Opening it is audited.
export function RequestBody({ args: [id] }) {
  const state = useLoad(() => api('GET', `/admin/v1/requests/${encodeURIComponent(id)}`), [id]);
  return html`
    <${PageHeader} title="Request" sub=${id}>
      <a class="button secondary" href="#/requests">Back to requests</a>
    <//>
    <${Loading} state=${state}>${(b) => {
      const redactions = Object.entries(b.redactions || {});
      const redacted = redactions.length ? `Redacted: ${redactions.map(([k, n]) => `${n} ${k}`).join(', ')}.` : 'Nothing was redacted.';
      return html`
        <p class="muted">
          ${when(b.ts)} · <code>${b.endpoint}</code> · key <code>${b.key_id}</code>
          ${b.truncated && html` · <span class="badge warn">Truncated</span>`}
        </p>
        <p class="notice">${`Content is shown as stored, redacted before it was written. ${redacted} This view is recorded in the audit log.`}</p>
        <section class="panel"><h2>Request</h2><${JSONView} value=${b.request} /></section>
        <section class="panel"><h2>Response</h2><${JSONView} value=${b.response} /></section>`;
    }}<//>`;
}
