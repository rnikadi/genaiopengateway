import { html, api, useLoad, Loading, Table, PageHeader, Ago } from '../lib.js';

// Directory shows the users and groups the identity provider provisioned
// over SCIM (docs/scim.md), for owners checking that provisioning works.
export function Directory() {
  const state = useLoad(() => api('GET', '/admin/v1/directory'));
  return html`
    <${PageHeader} title="Directory" sub="Users and groups provisioned by your identity provider over SCIM. A provisioned user's groups here are the ones role mappings match, and deactivating them signs them out." />
    <${Loading} state=${state}>${(d) => html`
      ${!d.enabled && html`<p class="notice">SCIM provisioning is off. Set a SCIM token on the <a href="#/settings/sign-in">Sign-in settings</a> page and give your identity provider <code>${location.origin}/scim/v2</code>.</p>`}
      <section class="panel">
        <h2>Users <span class="muted small">${d.total_users}</span></h2>
        <${Table}
          rowKey=${(u) => u.id}
          rows=${d.users}
          empty="No users are provisioned."
          columns=${[
            { label: 'User', render: (u) => html`<strong>${u.user_name}</strong>${u.display_name ? html`<div class="muted small">${u.display_name}</div>` : ''}` },
            { label: 'Email', render: (u) => u.email },
            { label: 'Status', render: (u) => (u.active ? html`<span class="badge good">Active</span>` : html`<span class="badge bad">Deactivated</span>`) },
            { label: 'Groups', render: (u) => (u.groups.length ? html`<span class="chips">${u.groups.map((g) => html`<span class="badge" key=${g.id}>${g.display}</span>`)}</span>` : html`<span class="muted small">None</span>`) },
            { label: 'Updated', render: (u) => html`<span class="small"><${Ago} t=${u.updated_at} /></span>` },
          ]}
        />
      </section>
      <section class="panel">
        <h2>Groups <span class="muted small">${d.total_groups}</span></h2>
        <${Table}
          rowKey=${(g) => g.id}
          rows=${d.groups}
          empty="No groups are provisioned."
          columns=${[
            { label: 'Group', render: (g) => html`<strong>${g.display_name}</strong>` },
            { label: 'External id', render: (g) => html`<code class="small">${g.external_id || ''}</code>` },
            { label: 'Updated', render: (g) => html`<span class="small"><${Ago} t=${g.updated_at} /></span>` },
          ]}
        />
      </section>`}<//>`;
}
