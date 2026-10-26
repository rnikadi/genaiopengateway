import {
  html, useState, api, useLoad, useSession, Loading, Table, Modal, PolicyForm, Confirm, PageHeader, Card, CopyButton,
  usageQuery, totals, policySummary, usd, num, day, when, Field, ErrorBox,
} from '../lib.js';
import { keyStatus, KeyActions, KeyModals } from './keys.js';

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
  const [notice, setNotice] = useState('');
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
        ${s.owner && html`<button class="secondary" onClick=${() => setModal('edit')}>Edit policy</button>`}
        ${canWrite && html`<button class="secondary" onClick=${() => setModal('clear')}>Clear cache</button>`}
        ${s.owner && html`<button class="danger" disabled=${teams.length > 0} title=${teams.length ? 'Delete its teams first' : ''} onClick=${() => setModal('delete')}>Delete</button>`}
        <a class="button secondary" href=${`#/usage?org=${encodeURIComponent(id)}`}>Usage</a>
      <//>
      ${notice && html`<p class="notice" role="status">${notice}</p>`}
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
      ${modal === 'clear' && html`<${Confirm} title="Clear response cache" action="Clear cache" onClose=${close}
        message=${`Remove every cached answer of ${org.name}'s teams? Their next requests go to the providers.`}
        onConfirm=${async () => { const r = await api('DELETE', `/admin/v1/cache?org_id=${encodeURIComponent(id)}`); setNotice(cacheCleared(r.cleared)); }} />`}
      ${modal === 'delete' && html`<${Confirm} title="Delete organization" action="Delete organization" onClose=${close}
        message=${`Delete ${org.name}? This cannot be undone. Usage and audit history stay.`}
        onConfirm=${async () => { await api('DELETE', `/admin/v1/orgs/${encodeURIComponent(id)}`); s.refresh(); location.hash = '#/orgs'; }} />`}`;
  }}<//>`;
}

// cacheCleared reports how many answers a cache clear removed. Semantic
// questions remembered on other replicas no longer find their answers.
const cacheCleared = (n) => `Cleared ${num(n)} cached answer${n === 1 ? '' : 's'}.`;


export function Team({ args: [id] }) {
  const s = useSession();
  const [modal, setModal] = useState(null);
  const [notice, setNotice] = useState('');
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
        ${canWrite && html`<button class="secondary" onClick=${() => setModal({ kind: 'clear' })}>Clear cache</button>`}
        ${canWrite && html`<button class="danger" disabled=${keys.some((k) => !k.revoked_at)} title=${keys.some((k) => !k.revoked_at) ? 'Revoke its keys first' : ''} onClick=${() => setModal({ kind: 'delete' })}>Delete</button>`}
        <a class="button secondary" href=${`#/usage?org=${encodeURIComponent(org.id)}&team=${encodeURIComponent(id)}`}>Usage</a>
      <//>
      ${notice && html`<p class="notice" role="status">${notice}</p>`}
      <div class="cards">
        <${Card} label="Spend this month" value=${usd(t.cost_usd)} sub=${team.budget ? `of ${usd(team.budget.amount_usd)} ${team.budget.period}` : 'No budget'} />
        <${Card} label="Requests this month" value=${num(t.requests)} />
        <${Card} label="Active keys" value=${num(keys.filter((k) => !k.revoked_at).length)} />
      </div>
      <section class="panel">
        <div class="panel-header"><h2>Virtual keys</h2>${canWrite && html`<button onClick=${() => setModal({ kind: 'key' })}>New key</button>`}</div>
        <${Table}
          rows=${keys}
          empty="No keys yet."
          columns=${[
            { label: 'Name', render: (k) => html`<strong>${k.name}</strong><br /><code class="muted small">${k.prefix}…</code>${k.owner && html`<div class="muted small">${k.owner}</div>`}` },
            { label: 'Status', render: keyStatus },
            { label: 'Policy', render: (k) => html`<span class="muted">${policySummary(k)}</span>` },
            { label: 'Spend this month', num: true, render: (k) => usd(byKey[k.id]?.cost_usd) },
            { label: 'Last used', render: (k) => (k.last_used_at ? when(k.last_used_at) : html`<span class="muted">Never</span>`) },
            { label: 'Expires', render: (k) => (k.expires_at ? day(k.expires_at) : html`<span class="muted">Never</span>`) },
            {
              label: '',
              render: (k) => html`<${KeyActions} k=${k} canWrite=${canWrite} setModal=${setModal} />`,
            },
          ]}
        />
      </section>
      <${Credentials} team=${team} canWrite=${canWrite} />
      ${modal?.kind === 'edit' && html`<${Modal} title="Edit team" onClose=${close}>
        <${PolicyForm} kind="team" initial=${team} submitLabel="Save" onCancel=${close}
          onSubmit=${async (body) => { await api('PATCH', `/admin/v1/teams/${encodeURIComponent(id)}`, body); close(); state.reload(); }} />
      <//>`}
      ${modal?.kind === 'clear' && html`<${Confirm} title="Clear response cache" action="Clear cache" onClose=${close}
        message=${`Remove every cached answer of ${team.name}? Its next requests go to the providers.`}
        onConfirm=${async () => { const r = await api('DELETE', `/admin/v1/cache?org_id=${encodeURIComponent(org.id)}&team_id=${encodeURIComponent(id)}`); setNotice(cacheCleared(r.cleared)); }} />`}
      ${modal?.kind === 'delete' && html`<${Confirm} title="Delete team" action="Delete team" onClose=${close}
        message=${`Delete ${team.name}? This cannot be undone. Usage and audit history stay.`}
        onConfirm=${async () => { await api('DELETE', `/admin/v1/teams/${encodeURIComponent(id)}`); location.hash = `#/orgs/${encodeURIComponent(org.id)}`; }} />`}
      ${modal?.kind === 'key' && html`<${Modal} title="New virtual key" onClose=${close}>
        <${PolicyForm} kind="key" submitLabel="Create key" onCancel=${close}
          onSubmit=${async (body) => { const k = await api('POST', `/admin/v1/teams/${encodeURIComponent(id)}/keys`, body); close(); setCreated(k); state.reload(); }} />
      <//>`}
      <${KeyModals} modal=${modal?.key ? modal : null} setModal=${setModal} close=${close} done=${() => { close(); state.reload(); }} teams=${{ [team.id]: team }} />
      ${created && html`<${Modal} title="Key created" onClose=${() => setCreated(null)}>
        <p>Copy the key now. The gateway stores only its hash, so it cannot be shown again.</p>
        <div class="secret"><code>${created.key}</code><${CopyButton} text=${created.key} /></div>
        <footer><button onClick=${() => setCreated(null)}>Done</button></footer>
      <//>`}`;
  }}<//>`;
}

// Credentials are the team's own provider keys, used for its requests in
// place of the shared ones (docs/team-credentials.md).
function Credentials({ team, canWrite }) {
  const state = useLoad(() => api('GET', `/admin/v1/teams/${encodeURIComponent(team.id)}/credentials`), [team.id]);
  const [editing, setEditing] = useState(null);
  const [removing, setRemoving] = useState(null);
  return html`<section class="panel">
    <div class="panel-header"><h2>Provider credentials</h2></div>
    <p class="muted">A provider key set here is used for this team's requests instead of the gateway's shared one, so they are billed to the team's own account. Applications keep using their virtual keys. If the team's key fails, the gateway does not fall back to the shared key of that provider.</p>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(c) => c.provider}
      rows=${d.data}
      empty="No providers are configured."
      columns=${[
        { label: 'Provider', render: (c) => html`<code>${c.provider}</code><div class="muted small">${c.type}</div>` },
        { label: 'Credential', render: (c) => (c.set ? html`<span class="badge good">Team's own key</span><div class="muted small">${when(c.updated_at)} · ${c.updated_by}</div>` : c.supported ? html`<span class="muted">Shared key</span>` : html`<span class="muted">Shared credential (not settable per team)</span>`) },
        {
          label: '',
          render: (c) => canWrite && c.supported && html`<div class="row-actions">
            <button class="link" onClick=${() => setEditing(c)}>${c.set ? 'Replace' : 'Set'}</button>
            ${c.set && html`<button class="link danger" onClick=${() => setRemoving(c)}>Remove</button>`}
          </div>`,
        },
      ]}
    />`}<//>
    ${editing && html`<${Modal} title=${`${editing.set ? 'Replace' : 'Set'} the ${editing.provider} key of ${team.name}`} onClose=${() => setEditing(null)}>
      <${CredentialForm} onCancel=${() => setEditing(null)}
        onSubmit=${async (apiKey) => { await api('PUT', `/admin/v1/teams/${encodeURIComponent(team.id)}/credentials/${encodeURIComponent(editing.provider)}`, { api_key: apiKey }); setEditing(null); state.reload(); }} />
    <//>`}
    ${removing && html`<${Confirm} title="Remove the team's key" action="Remove" onClose=${() => setRemoving(null)}
      message=${`${team.name} goes back to the gateway's shared ${removing.provider} key.`}
      onConfirm=${async () => { await api('DELETE', `/admin/v1/teams/${encodeURIComponent(team.id)}/credentials/${encodeURIComponent(removing.provider)}`); state.reload(); }} />`}
  </section>`;
}

function CredentialForm({ onSubmit, onCancel }) {
  const [key, setKey] = useState('');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      await onSubmit(key.trim());
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return html`<form class="form" onSubmit=${submit}>
    <${ErrorBox} error=${error} />
    <${Field} label="API key" hint="Stored encrypted and never shown again.">
      <input type="password" required autocomplete="off" value=${key} onInput=${(e) => setKey(e.target.value)} />
    <//>
    <footer>
      <button type="button" class="secondary" onClick=${onCancel}>Cancel</button>
      <button type="submit" disabled=${busy}>Save</button>
    </footer>
  </form>`;
}
