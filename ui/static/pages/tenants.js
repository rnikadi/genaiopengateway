import {
  html, useState, api, useLoad, useSession, Loading, Table, Modal, PolicyForm, Confirm, PageHeader, Card, CopyButton,
  usageQuery, totals, policySummary, usd, num, day, when,
} from '../lib.js';

// usageBy indexes this month's usage rows by group.
const usageBy = (rows) => Object.fromEntries(rows.map((r) => [r.group, r]));

export function Orgs() {
  const s = useSession();
  const [creating, setCreating] = useState(false);
  const state = useLoad(() => api('GET', '/admin/v1/orgs?limit=500'));
  const create = async (body) => {
    const o = await api('POST', '/admin/v1/orgs', body);
    s.refresh();
    location.hash = `#/orgs/${encodeURIComponent(o.id)}`;
  };
  return html`
    <${PageHeader} title="Organizations" sub="Each organization holds teams, and each team holds API keys.">
      ${s.owner && html`<button onClick=${() => setCreating(true)}>New organization</button>`}
    <//>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rows=${d.data}
      empty=${s.owner ? 'No organizations yet. Create one to start issuing keys.' : 'You have no role in any organization.'}
      columns=${[
        { label: 'Name', render: (o) => html`<a href=${`#/orgs/${encodeURIComponent(o.id)}`}>${o.name}</a>` },
        { label: 'Policy', render: (o) => html`<span class="muted">${policySummary(o)}</span>` },
        { label: 'Created', render: (o) => day(o.created_at) },
      ]}
    />`}<//>
    ${creating && html`<${Modal} title="New organization" onClose=${() => setCreating(false)}>
      <${PolicyForm} kind="org" submitLabel="Create organization" onSubmit=${create} onCancel=${() => setCreating(false)} />
    <//>`}`;
}

export function Org({ args: [id] }) {
  const s = useSession();
  const [modal, setModal] = useState(null);
  const state = useLoad(async () => {
    const [org, teams, usage] = await Promise.all([
      api('GET', `/admin/v1/orgs/${encodeURIComponent(id)}`),
      api('GET', `/admin/v1/orgs/${encodeURIComponent(id)}/teams?limit=500`),
      api('GET', usageQuery({ org: id, group_by: 'team' })),
    ]);
    return { org, teams: teams.data, usage: usage.data };
  }, [id]);
  const canWrite = s.canWrite(id);
  const close = () => setModal(null);

  return html`<${Loading} state=${state}>${({ org, teams, usage }) => {
    const t = totals(usage);
    const byTeam = usageBy(usage);
    return html`
      <p class="crumbs"><a href="#/orgs">Organizations</a></p>
      <${PageHeader} title=${org.name} sub=${policySummary(org)}>
        ${canWrite && html`<button class="secondary" onClick=${() => setModal('edit')}>Edit policy</button>`}
        ${s.owner && html`<button class="danger" disabled=${teams.length > 0} title=${teams.length ? 'Delete its teams first' : ''} onClick=${() => setModal('delete')}>Delete</button>`}
        <a class="button secondary" href=${`#/usage?org=${encodeURIComponent(id)}`}>Usage</a>
      <//>
      <div class="cards">
        <${Card} label="Spend this month" value=${usd(t.cost_usd)} sub=${org.budget ? `of ${usd(org.budget.amount_usd)} ${org.budget.period}` : 'No budget'} />
        <${Card} label="Requests this month" value=${num(t.requests)} />
        <${Card} label="Teams" value=${num(teams.length)} />
      </div>
      <section class="panel">
        <div class="panel-header"><h2>Teams</h2>${canWrite && html`<button onClick=${() => setModal('team')}>New team</button>`}</div>
        <${Table}
          rows=${teams}
          empty="No teams yet. Keys belong to teams, so create one first."
          columns=${[
            { label: 'Name', render: (tm) => html`<a href=${`#/teams/${encodeURIComponent(tm.id)}`}>${tm.name}</a>` },
            { label: 'Policy', render: (tm) => html`<span class="muted">${policySummary(tm)}</span>` },
            { label: 'Spend this month', num: true, render: (tm) => usd(byTeam[tm.id]?.cost_usd) },
            { label: 'Requests', num: true, render: (tm) => num(byTeam[tm.id]?.requests) },
          ]}
        />
      </section>
      ${modal === 'edit' && html`<${Modal} title="Edit organization" onClose=${close}>
        <${PolicyForm} kind="org" initial=${org} submitLabel="Save" onCancel=${close}
          onSubmit=${async (body) => { await api('PATCH', `/admin/v1/orgs/${encodeURIComponent(id)}`, body); close(); s.refresh(); state.reload(); }} />
      <//>`}
      ${modal === 'team' && html`<${Modal} title="New team" onClose=${close}>
        <${PolicyForm} kind="team" submitLabel="Create team" onCancel=${close}
          onSubmit=${async (body) => { const tm = await api('POST', `/admin/v1/orgs/${encodeURIComponent(id)}/teams`, body); location.hash = `#/teams/${encodeURIComponent(tm.id)}`; }} />
      <//>`}
      ${modal === 'delete' && html`<${Confirm} title="Delete organization" action="Delete organization" onClose=${close}
        message=${`Delete ${org.name}? This cannot be undone. Usage and audit history stay.`}
        onConfirm=${async () => { await api('DELETE', `/admin/v1/orgs/${encodeURIComponent(id)}`); s.refresh(); location.hash = '#/orgs'; }} />`}`;
  }}<//>`;
}

function keyStatus(k) {
  if (k.revoked_at) return html`<span class="badge bad">Revoked</span>`;
  if (k.expires_at && new Date(k.expires_at) < new Date()) return html`<span class="badge warn">Expired</span>`;
  return html`<span class="badge good">Active</span>`;
}

export function Team({ args: [id] }) {
  const s = useSession();
  const [modal, setModal] = useState(null);
  const [created, setCreated] = useState(null);
  const state = useLoad(async () => {
    const team = await api('GET', `/admin/v1/teams/${encodeURIComponent(id)}`);
    const [org, keys, usage] = await Promise.all([
      api('GET', `/admin/v1/orgs/${encodeURIComponent(team.org_id)}`),
      api('GET', `/admin/v1/teams/${encodeURIComponent(id)}/keys?limit=500`),
      api('GET', usageQuery({ team: id, group_by: 'key' })),
    ]);
    return { team, org, keys: keys.data, usage: usage.data };
  }, [id]);
  const close = () => setModal(null);

  return html`<${Loading} state=${state}>${({ team, org, keys, usage }) => {
    const canWrite = s.canWrite(team.org_id);
    const byKey = usageBy(usage);
    const t = totals(usage);
    return html`
      <p class="crumbs"><a href="#/orgs">Organizations</a> / <a href=${`#/orgs/${encodeURIComponent(org.id)}`}>${org.name}</a></p>
      <${PageHeader} title=${team.name} sub=${policySummary(team)}>
        ${canWrite && html`<button class="secondary" onClick=${() => setModal({ kind: 'edit' })}>Edit policy</button>`}
        ${canWrite && html`<button class="danger" disabled=${keys.some((k) => !k.revoked_at)} title=${keys.some((k) => !k.revoked_at) ? 'Revoke its keys first' : ''} onClick=${() => setModal({ kind: 'delete' })}>Delete</button>`}
        <a class="button secondary" href=${`#/usage?org=${encodeURIComponent(org.id)}&team=${encodeURIComponent(id)}`}>Usage</a>
      <//>
      <div class="cards">
        <${Card} label="Spend this month" value=${usd(t.cost_usd)} sub=${team.budget ? `of ${usd(team.budget.amount_usd)} ${team.budget.period}` : 'No budget'} />
        <${Card} label="Requests this month" value=${num(t.requests)} />
        <${Card} label="Active keys" value=${num(keys.filter((k) => !k.revoked_at).length)} />
      </div>
      <section class="panel">
        <div class="panel-header"><h2>API keys</h2>${canWrite && html`<button onClick=${() => setModal({ kind: 'key' })}>New key</button>`}</div>
        <${Table}
          rows=${keys}
          empty="No keys yet."
          columns=${[
            { label: 'Name', render: (k) => html`<strong>${k.name}</strong><br /><code class="muted small">${k.prefix}…</code>` },
            { label: 'Status', render: keyStatus },
            { label: 'Policy', render: (k) => html`<span class="muted">${policySummary(k)}</span>` },
            { label: 'Spend this month', num: true, render: (k) => usd(byKey[k.id]?.cost_usd) },
            { label: 'Last used', render: (k) => (k.last_used_at ? when(k.last_used_at) : html`<span class="muted">Never</span>`) },
            { label: 'Expires', render: (k) => (k.expires_at ? day(k.expires_at) : html`<span class="muted">Never</span>`) },
            {
              label: '',
              render: (k) => canWrite && !k.revoked_at && html`<div class="row-actions">
                <button class="link" onClick=${() => setModal({ kind: 'edit-key', key: k })}>Edit</button>
                <button class="link danger" onClick=${() => setModal({ kind: 'revoke', key: k })}>Revoke</button>
              </div>`,
            },
          ]}
        />
      </section>
      ${modal?.kind === 'edit' && html`<${Modal} title="Edit team" onClose=${close}>
        <${PolicyForm} kind="team" initial=${team} submitLabel="Save" onCancel=${close}
          onSubmit=${async (body) => { await api('PATCH', `/admin/v1/teams/${encodeURIComponent(id)}`, body); close(); state.reload(); }} />
      <//>`}
      ${modal?.kind === 'delete' && html`<${Confirm} title="Delete team" action="Delete team" onClose=${close}
        message=${`Delete ${team.name}? This cannot be undone. Usage and audit history stay.`}
        onConfirm=${async () => { await api('DELETE', `/admin/v1/teams/${encodeURIComponent(id)}`); location.hash = `#/orgs/${encodeURIComponent(org.id)}`; }} />`}
      ${modal?.kind === 'key' && html`<${Modal} title="New API key" onClose=${close}>
        <${PolicyForm} kind="key" submitLabel="Create key" onCancel=${close}
          onSubmit=${async (body) => { const k = await api('POST', `/admin/v1/teams/${encodeURIComponent(id)}/keys`, body); close(); setCreated(k); state.reload(); }} />
      <//>`}
      ${modal?.kind === 'edit-key' && html`<${Modal} title="Edit API key" onClose=${close}>
        <${PolicyForm} kind="key" initial=${modal.key} submitLabel="Save" onCancel=${close}
          onSubmit=${async (body) => { await api('PATCH', `/admin/v1/keys/${encodeURIComponent(modal.key.id)}`, body); close(); state.reload(); }} />
      <//>`}
      ${modal?.kind === 'revoke' && html`<${Confirm} title="Revoke API key" action="Revoke key" onClose=${close}
        message=${`Revoke ${modal.key.name}? Requests with it fail at once, and it cannot be restored.`}
        onConfirm=${async () => { await api('POST', `/admin/v1/keys/${encodeURIComponent(modal.key.id)}/revoke`); state.reload(); }} />`}
      ${created && html`<${Modal} title="Key created" onClose=${() => setCreated(null)}>
        <p>Copy the key now. The gateway stores only its hash, so it cannot be shown again.</p>
        <div class="secret"><code>${created.key}</code><${CopyButton} text=${created.key} /></div>
        <footer><button onClick=${() => setCreated(null)}>Done</button></footer>
      <//>`}`;
  }}<//>`;
}
