import { html, useState, useEffect, api, qs, useSession, PageHeader, OrgPicker, defaultOrg, Table, ErrorBox, JSONView, when } from '../lib.js';

// Audit lists who changed what, newest first.
export function Audit() {
  const s = useSession();
  const [org, setOrg] = useState(defaultOrg(s));
  const [list, setList] = useState({ items: [], more: false, loading: true });
  const [error, setError] = useState(null);

  const load = async (after) => {
    setError(null);
    try {
      const d = await api('GET', '/admin/v1/audit' + qs({ org_id: org, limit: 50, after }));
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

  const resource = (e) => {
    if (e.resource_type === 'org') return html`<a href=${`#/orgs/${encodeURIComponent(e.resource_id)}`}>${s.orgNames[e.resource_id] || e.resource_id}</a>`;
    if (e.resource_type === 'team') return html`<a href=${`#/teams/${encodeURIComponent(e.resource_id)}`}>${e.resource_id}</a>`;
    return html`<span>${e.resource_type} <code class="small">${e.resource_id}</code></span>`;
  };

  return html`
    <${PageHeader} title="Audit log" sub="Every change made through the admin API or this UI, newest first." />
    <div class="filters"><${OrgPicker} value=${org} onChange=${setOrg} /></div>
    <${ErrorBox} error=${error} />
    ${list.loading ? html`<p class="muted">Loading…</p>` : html`<${Table}
      rows=${list.items}
      empty="Nothing has been changed yet."
      columns=${[
        { label: 'Time', render: (e) => when(e.ts) },
        { label: 'Who', render: (e) => e.actor },
        { label: 'Action', render: (e) => html`<code>${e.action}</code>` },
        { label: 'Resource', render: resource },
        { label: 'Details', render: (e) => (e.details && e.details !== 'null' ? html`<details><summary>Show</summary><${JSONView} value=${e.details} /></details>` : '') },
      ]}
    />`}
    ${list.more && html`<p class="center"><button class="secondary" onClick=${() => load(list.items[list.items.length - 1].id)}>Load more</button></p>`}`;
}
