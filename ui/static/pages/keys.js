import {
  html, useState, api, qs, useLoad, useSession, Loading, Table, Modal, PolicyForm, Confirm, PageHeader, CopyButton, Field, ErrorBox,
  OrgPicker, defaultOrg, policySummary, usd, num, compact, day, when,
} from '../lib.js';

// Virtual keys across every team the operator can read: search, create,
// edit, rotate and revoke (docs/virtual-keys.md).

const statuses = { '': 'Any status', active: 'Active', expiring: 'Expiring within 7 days', expired: 'Expired', rotating: 'Rotating', revoked: 'Revoked' };

export function keyStatus(k) {
  const now = new Date();
  if (k.revoked_at) return html`<span class="badge bad">Revoked</span>`;
  if (k.expires_at && new Date(k.expires_at) < now) return html`<span class="badge warn">Expired</span>`;
  const rotating = k.previous_expires_at && new Date(k.previous_expires_at) > now;
  return html`<span class="badge good">Active</span>${rotating && html` <span class="badge warn" title=${`The previous secret works until ${when(k.previous_expires_at)}`}>Rotating</span>`}`;
}

// teamsOf loads every team of the organizations the operator can read,
// keyed by id, with its organization's name.
async function teamsOf(orgNames) {
  const lists = await Promise.all(
    Object.keys(orgNames).map((id) => api('GET', `/admin/v1/orgs/${encodeURIComponent(id)}/teams?limit=500`).catch(() => ({ data: [] }))),
  );
  const out = {};
  for (const t of lists.flatMap((l) => l.data)) out[t.id] = { ...t, org_name: orgNames[t.org_id] || t.org_id };
  return out;
}

export function Keys() {
  const s = useSession();
  const [org, setOrg] = useState(defaultOrg(s));
  const [status, setStatus] = useState('active');
  const [search, setSearch] = useState('');
  const [query, setQuery] = useState('');
  const [modal, setModal] = useState(null);
  const teams = useLoad(() => teamsOf(s.orgNames), [Object.keys(s.orgNames).join()]);
  const state = useLoad(() => api('GET', '/admin/v1/keys' + qs({ org_id: org, status, q: query, limit: 500 })), [org, status, query]);
  const close = () => setModal(null);
  const done = () => {
    close();
    state.reload();
  };
  const writable = Object.values(teams.data || {}).filter((t) => s.canWrite(t.org_id));

  return html`
    <${PageHeader} title="Virtual keys" sub="Applications use these keys instead of provider credentials, which stay in the gateway. Each key belongs to a team and can narrow its access and limits.">
      ${writable.length > 0 && html`<button onClick=${() => setModal({ kind: 'create' })}>New key</button>`}
    <//>
    <form class="filters" onSubmit=${(e) => { e.preventDefault(); setQuery(search.trim()); }}>
      <${OrgPicker} value=${org} onChange=${setOrg} />
      <label class="inline"><span>Status</span>
        <select value=${status} onChange=${(e) => setStatus(e.target.value)}>
          ${Object.entries(statuses).map(([v, label]) => html`<option value=${v} key=${v}>${label}</option>`)}
        </select>
      </label>
      <label class="inline"><span>Search</span>
        <input type="search" placeholder="Name, prefix or owner" value=${search} onInput=${(e) => setSearch(e.target.value)} onBlur=${() => setQuery(search.trim())} />
      </label>
    </form>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rows=${d.data}
      empty="No keys match."
      columns=${[
        { label: 'Name', render: (k) => html`<strong>${k.name}</strong><br /><code class="muted small">${k.prefix}…</code>` },
        { label: 'Owner', render: (k) => k.owner || html`<span class="muted">None</span>` },
        {
          label: 'Team',
          render: (k) => {
            const t = teams.data?.[k.team_id];
            return html`<a href=${`#/teams/${encodeURIComponent(k.team_id)}`}>${t?.name || k.team_id}</a><div class="muted small">${t?.org_name || ''}</div>`;
          },
        },
        { label: 'Status', render: keyStatus },
        { label: 'Last used', render: (k) => (k.last_used_at ? when(k.last_used_at) : html`<span class="muted">Never</span>`) },
        { label: 'Expires', render: (k) => (k.expires_at ? day(k.expires_at) : html`<span class="muted">Never</span>`) },
        { label: '', render: (k) => html`<${KeyActions} k=${k} canWrite=${s.canWrite(k.org_id)} setModal=${setModal} />` },
      ]}
    />`}<//>
    ${modal?.kind === 'create' && html`<${CreateKey} teams=${writable} onClose=${close} onCreated=${(k) => { setModal({ kind: 'secret', key: k, title: 'Key created' }); state.reload(); }} />`}
    <${KeyModals} modal=${modal} setModal=${setModal} close=${close} done=${done} teams=${teams.data} />`;
}

// KeyActions are the row actions for one key.
export function KeyActions({ k, canWrite, setModal }) {
  return html`<div class="row-actions">
    <button class="link" onClick=${() => setModal({ kind: 'access', key: k })}>Access</button>
    ${canWrite && !k.revoked_at && html`
      <button class="link" onClick=${() => setModal({ kind: 'edit-key', key: k })}>Edit</button>
      <button class="link" onClick=${() => setModal({ kind: 'rotate', key: k })}>Rotate</button>
      <button class="link danger" onClick=${() => setModal({ kind: 'revoke', key: k })}>Revoke</button>`}
  </div>`;
}

// KeyModals are the dialogs KeyActions open.
export function KeyModals({ modal, setModal, close, done, teams }) {
  if (!modal) return null;
  const k = modal.key;
  switch (modal.kind) {
    case 'edit-key':
      return html`<${Modal} title="Edit key" onClose=${close}>
        <${PolicyForm} kind="key" initial=${k} submitLabel="Save" onCancel=${close}
          onSubmit=${async (body) => { await api('PATCH', `/admin/v1/keys/${encodeURIComponent(k.id)}`, body); done(); }} />
      <//>`;
    case 'rotate':
      return html`<${Rotate} k=${k} onClose=${close} onRotated=${(r) => { done(); setModal({ kind: 'secret', key: r, title: 'Key rotated', rotated: true }); }} />`;
    case 'revoke':
      return html`<${Confirm} title="Revoke key" action="Revoke key" onClose=${close}
        message=${`Revoke ${k.name}? Requests with it fail at once, including with a previous secret still in its rotation grace period, and it cannot be restored.`}
        onConfirm=${async () => { await api('POST', `/admin/v1/keys/${encodeURIComponent(k.id)}/revoke`); done(); }} />`;
    case 'access':
      return html`<${Access} k=${k} team=${teams?.[k.team_id]} onClose=${close} />`;
    case 'secret':
      return html`<${Modal} title=${modal.title} onClose=${close}>
        <p>Copy the key now. The gateway stores only its hash, so it cannot be shown again.</p>
        ${modal.rotated && k.previous_expires_at && html`<p class="muted">The previous secret keeps working until ${when(k.previous_expires_at)}.</p>`}
        <div class="secret"><code>${k.key}</code><${CopyButton} text=${k.key} /></div>
        <footer><button onClick=${close}>Done</button></footer>
      <//>`;
  }
  return null;
}

function CreateKey({ teams, onClose, onCreated }) {
  const [team, setTeam] = useState(teams[0]?.id || '');
  return html`<${Modal} title="New key" onClose=${onClose}>
    <div class="form">
      <${Field} label="Team" hint="The key gets the team's and organization's access and limits, and can narrow them.">
        <select value=${team} onChange=${(e) => setTeam(e.target.value)}>
          ${teams.map((t) => html`<option value=${t.id} key=${t.id}>${t.org_name} / ${t.name}</option>`)}
        </select>
      <//>
    </div>
    <${PolicyForm} kind="key" submitLabel="Create key" onCancel=${onClose}
      onSubmit=${async (body) => onCreated(await api('POST', `/admin/v1/teams/${encodeURIComponent(team)}/keys`, body))} />
  <//>`;
}

const graces = { '0': 'Stop it now', '1h': '1 hour', '24h': '24 hours', '7d': '7 days', '30d': '30 days' };

function Rotate({ k, onClose, onRotated }) {
  const [grace, setGrace] = useState('24h');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      onRotated(await api('POST', `/admin/v1/keys/${encodeURIComponent(k.id)}/rotate`, { grace }));
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return html`<${Modal} title=${`Rotate ${k.name}`} onClose=${onClose}>
    <form class="form" onSubmit=${submit}>
      <${ErrorBox} error=${error} />
      <p>The key gets a new secret and keeps its name, access, limits and usage. Give applications time to switch to the new secret.</p>
      <${Field} label="Keep the current secret working for">
        <select value=${grace} onChange=${(e) => setGrace(e.target.value)}>
          ${Object.entries(graces).map(([v, label]) => html`<option value=${v} key=${v}>${label}</option>`)}
        </select>
      <//>
      <footer>
        <button type="button" class="secondary" onClick=${onClose}>Cancel</button>
        <button type="submit" disabled=${busy}>Rotate</button>
      </footer>
    </form>
  <//>`;
}

// Access shows each policy field at the organization, team and key; a
// request must pass every level.
function Access({ k, team, onClose }) {
  const state = useLoad(async () => {
    const [org, t] = await Promise.all([
      api('GET', `/admin/v1/orgs/${encodeURIComponent(k.org_id)}`),
      team ? Promise.resolve(team) : api('GET', `/admin/v1/teams/${encodeURIComponent(k.team_id)}`),
    ]);
    return [org, t, k];
  });
  const list = (v) => (v?.length ? v.join(', ') : null);
  const rows = [
    ['Models', (p) => list(p.allowed_models), 'All'],
    ['MCP servers', (p) => list(p.allowed_mcp_servers), 'All'],
    ['Requests per minute', (p) => (p.rpm ? num(p.rpm) : null), 'No limit'],
    ['Tokens per minute', (p) => (p.tpm ? compact(p.tpm) : null), 'No limit'],
    ['Budget', (p) => (p.budget ? `${usd(p.budget.amount_usd)} ${p.budget.period}` : null), 'None'],
    ['Guardrails', (p) => list(p.guardrails), 'None'],
    ['Response cache', (p) => (p.cache ? (p.cache.mode === 'off' ? 'Off' : `${p.cache.mode} ${p.cache.ttl || '1h'}`) : null), 'Inherit'],
  ];
  return html`<${Modal} title=${`Access of ${k.name}`} onClose=${onClose}>
    <p class="muted">A request with this key must pass every level: allow lists narrow, and each level's limits and budget apply on their own.</p>
    <${Loading} state=${state}>${([org, t]) => html`<${Table}
      rowKey=${(r) => r[0]}
      rows=${rows}
      columns=${[
        { label: '', render: (r) => html`<strong>${r[0]}</strong>` },
        ...[['Organization', org], ['Team', t], ['Key', k]].map(([label, p]) => ({
          label: html`${label}<div class="muted small">${p.name}</div>`,
          render: (r) => r[1](p) ?? html`<span class="muted">${r[2]}</span>`,
        })),
      ]}
    />`}<//>
    <p class="muted small">${policySummary(k)}</p>
    <footer><button onClick=${onClose}>Close</button></footer>
  <//>`;
}
