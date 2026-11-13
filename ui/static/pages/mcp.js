import {
  html, useState, useEffect, api, qs, useLoad, useSession, Loading, Table, Modal, Confirm, PageHeader, ErrorBox, JSONView, OrgPicker, defaultOrg, when, num, Field, none, Ago, Tabs
} from '../lib.js';
import { useConfigObjects, DeletedObjects } from './config.js';

const statusBadge = {
  approved: html`<span class="badge good">Approved</span>`,
  pending: html`<span class="badge warn">New</span>`,
  changed: html`<span class="badge bad">Changed</span>`,
};

const tabNames = { review: 'To review', catalog: 'Catalog', servers: 'Servers', virtual: 'Virtual servers', tools: 'Tools', calls: 'Calls' };

// MCP is one page per job: what needs a decision first, then the catalog,
// the servers and virtual servers (editable by owners), the tools and the
// calls (docs/admin-ui.md).
export function MCP({ params }) {
  const s = useSession();
  const [tab, setTab] = useState(tabNames[params.get('tab')] ? params.get('tab') : params.get('edit') ? 'servers' : 'review');
  const [review, setReview] = useState(null);
  const [filter, setFilter] = useState('');
  // tick reloads the catalog and the requests when either changes.
  const [tick, setTick] = useState(0);
  const bump = () => setTick((n) => n + 1);
  const state = useLoad(async () => {
    const [servers, tools, virtual] = await Promise.all([
      api('GET', '/admin/v1/mcp/servers'),
      api('GET', '/admin/v1/mcp/tools'),
      api('GET', '/admin/v1/mcp/virtual-servers'),
    ]);
    return { servers: servers.data, tools: tools.data, virtual: virtual.data };
  });
  const servers = useConfigObjects('mcp_servers', { params: tab === 'servers' ? params : null, onChange: state.reload });
  const virtual = useConfigObjects('mcp_virtual_servers', { params: tab === 'virtual' ? params : null, onChange: state.reload });
  const toReview = (state.data?.tools || []).filter((t) => t.status !== 'approved');
  const tabs = { ...tabNames, review: toReview.length ? `To review (${toReview.length})` : 'To review' };

  return html`
    <${PageHeader} title="MCP" sub="The MCP servers behind the gateway, the tools they offer, and the virtual servers clients connect to." />
    <${Tabs} tabs=${tabs} value=${tab} onChange=${setTab} />
    ${tab === 'review' && html`<${AccessRequests} tick=${tick} onChange=${bump} />`}
    ${tab === 'catalog' && html`<${Catalog} tick=${tick} onChange=${bump} />`}
    ${tab === 'calls' && html`<${Calls} />`}
    <${Loading} state=${state}>${(d) => {
      const shown = filter ? d.tools.filter((t) => t.status === filter) : d.tools;
      return html`
        ${tab === 'review' && html`<section class=${`panel${toReview.length ? ' attention' : ''}`}>
          <h2>Tools to review</h2>
          <p class="muted">Pinned servers hide a tool from clients until an owner approves its definition, and hide it again if the definition changes.</p>
          <${Table}
            rowKey=${(t) => t.server + '/' + t.tool}
            rows=${toReview}
            empty="Every tool is approved."
            columns=${[
              { label: 'Tool', render: (t) => html`<code>${t.server}/${t.tool}</code>` },
              { label: 'Status', render: (t) => statusBadge[t.status] },
              { label: 'Seen', render: (t) => html`<${Ago} t=${t.updated_at} />` },
              { label: html`<span class="sr-only">Actions</span>`, render: (t) => html`<button class="secondary" onClick=${() => setReview(t)}>Review</button>` },
            ]}
          />
        </section>`}
        ${tab === 'servers' && html`<section class="panel">
          <div class="panel-header"><h2>Servers</h2>${servers.newButton}</div>
          <${Table}
            rowKey=${(v) => v.name}
            rows=${d.servers}
            empty="No MCP servers are configured."
            columns=${[
              { label: 'Server', render: (v) => html`<strong>${v.name}</strong> ${servers.source(v.name)}<br /><span class="muted small">${v.url}</span>` },
              { label: 'Status', render: (v) => (v.up ? html`<span class="badge good">Up</span>` : html`<span class="badge bad">Down</span>`) },
              { label: 'Credentials', render: (v) => (v.user_auth ? 'Per user' : 'Shared') },
              { label: 'Pinning', render: (v) => v.pin_tools },
              { label: 'Tools', num: true, render: (v) => html`${num(v.tools)}${v.hidden_tools ? html` <span class="muted small">(${num(v.hidden_tools)} hidden)</span>` : ''}` },
              { label: 'Prompts', num: true, render: (v) => num(v.prompts) },
              { label: 'Resources', num: true, render: (v) => num(v.resources) },
              { label: 'Refreshed', render: (v) => html`<${Ago} t=${v.refreshed_at} />` },
              ...(servers.owner ? [{ label: html`<span class="sr-only">Actions</span>`, render: (v) => servers.actions(v.name) }] : []),
            ]}
          />
        </section>
        <${DeletedObjects} cfg=${servers} />
        ${s.owner && d.servers.some((v) => v.user_auth) && html`<${Connections} />`}
        ${servers.dialogs}`}
        ${tab === 'virtual' && html`<section class="panel">
          <div class="panel-header"><h2>Virtual servers</h2>${virtual.newButton}</div>
          <${Table}
            rowKey=${(v) => v.name}
            rows=${d.virtual}
            empty="No virtual servers are configured."
            columns=${[
              { label: 'Name', render: (v) => html`<strong>${v.name}</strong> ${virtual.source(v.name)}${v.description && html`<br /><span class="muted small">${v.description}</span>`}` },
              { label: 'Endpoint', render: (v) => html`<code class="small">/mcp/${v.name}</code>` },
              {
                label: 'Upstreams',
                render: (v) => html`<ul class="plain">${v.servers.map((u) => html`<li key=${u.server}><code>${u.server}</code>${u.prefix && html` as <code>${u.prefix}*</code>`}${u.tools?.length ? html` <span class="muted small">${u.tools.join(', ')}</span>` : html` <span class="muted small">all tools</span>`}</li>`)}</ul>`,
              },
              ...(virtual.owner ? [{ label: html`<span class="sr-only">Actions</span>`, render: (v) => virtual.actions(v.name) }] : []),
            ]}
          />
        </section>
        <${DeletedObjects} cfg=${virtual} />
        ${virtual.dialogs}`}
        ${tab === 'tools' && html`<section class="panel">
          <div class="panel-header"><h2>Tools</h2>
            <label class="inline"><span>Status</span><select value=${filter} onChange=${(e) => setFilter(e.target.value)}>
              <option value="">All</option><option value="approved">Approved</option><option value="pending">New</option><option value="changed">Changed</option>
            </select></label>
          </div>
          <${Table}
            rowKey=${(t) => t.server + '/' + t.tool}
            rows=${shown}
            empty="No tools have been seen yet."
            columns=${[
              { label: 'Tool', render: (t) => html`<code>${t.server}/${t.tool}</code>` },
              { label: 'Status', render: (t) => statusBadge[t.status] },
              { label: 'Updated', render: (t) => html`<${Ago} t=${t.updated_at} />` },
              { label: html`<span class="sr-only">Actions</span>`, render: (t) => html`<button class="link" onClick=${() => setReview(t)}>Definition</button>` },
            ]}
          />
        </section>`}
        ${review && html`<${Review} tool=${review} canApprove=${s.owner} onClose=${() => setReview(null)} onApproved=${() => { setReview(null); state.reload(); }} />`}`;
    }}<//>`;
}

const accessBadge = {
  allowed: html`<span class="badge good">Allowed</span>`,
  pending: html`<span class="badge warn">Requested</span>`,
  available: html`<span class="badge">Not allowed</span>`,
  blocked_by_org: html`<span class="badge">Not allowed for the organization</span>`,
};

// Catalog lists the virtual servers with a team's access to each, and
// lets the operator ask for access (docs/mcp-catalog.md).
function Catalog({ tick, onChange }) {
  const s = useSession();
  const [org, setOrg] = useState(s.orgIDs[0] || Object.keys(s.orgNames)[0] || '');
  const [team, setTeam] = useState('');
  const [asking, setAsking] = useState(null);
  const teams = useLoad(() => (org ? api('GET', `/admin/v1/orgs/${encodeURIComponent(org)}/teams?limit=500`) : none), [org]);
  useEffect(() => { setTeam(teams.data?.data?.[0]?.id || ''); }, [teams.data]);
  const state = useLoad(() => api('GET', '/admin/v1/mcp/catalog' + qs({ team_id: team })), [team, tick]);
  const ids = s.readsAll ? Object.keys(s.orgNames) : s.orgIDs;
  return html`<section class="panel">
    <div class="panel-header"><h2>Catalog</h2>
      <div class="row-actions">
        ${ids.length > 1 && html`<label class="inline"><span>Organization</span><select value=${org} onChange=${(e) => setOrg(e.target.value)}>
          ${ids.map((id) => html`<option value=${id} key=${id}>${s.orgNames[id] || id}</option>`)}
        </select></label>`}
        <label class="inline"><span>Team</span><select value=${team} onChange=${(e) => setTeam(e.target.value)}>
          ${(teams.data?.data || []).map((t) => html`<option value=${t.id} key=${t.id}>${t.name}</option>`)}
        </select></label>
      </div>
    </div>
    <p class="muted">The virtual MCP servers clients can connect to. A team that may not use one can ask for it; an operator who manages the organization's teams approves.</p>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(v) => v.name}
      rows=${d.data}
      empty="No virtual servers are configured."
      columns=${[
        { label: 'Server', render: (v) => html`<strong>${v.name}</strong>${v.description && html`<br /><span class="muted small">${v.description}</span>`}` },
        { label: 'Endpoint', render: (v) => html`<code class="small">${v.endpoint}</code>` },
        { label: 'Upstreams', render: (v) => html`<span class="muted small">${v.servers.map((u) => u.server).join(', ')}</span>` },
        { label: 'Guardrails', render: (v) => html`<span class="muted small">${v.guardrails.join(', ') || 'None'}</span>` },
        { label: 'Access', render: (v) => accessBadge[v.access] || '' },
        { label: '', render: (v) => team && (v.access === 'available' || v.access === 'blocked_by_org') && html`<button class="link" onClick=${() => setAsking(v)}>Request access</button>` },
      ]}
    />`}<//>
    ${asking && html`<${Modal} title=${`Request ${asking.name}`} onClose=${() => setAsking(null)}>
      <${RequestForm} server=${asking} onCancel=${() => setAsking(null)}
        onSubmit=${async (reason) => { await api('POST', '/admin/v1/mcp/requests', { team_id: team, server: asking.name, reason }); setAsking(null); onChange(); }} />
    <//>`}
  </section>`;
}

function RequestForm({ server, onSubmit, onCancel }) {
  const [reason, setReason] = useState('');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      await onSubmit(reason.trim());
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return html`<form class="form" onSubmit=${submit}>
    <${ErrorBox} error=${error} />
    ${server.access === 'blocked_by_org' && html`<p class="muted">The organization's policy leaves ${server.name} out, so an owner has to allow it for the organization before the request can be approved.</p>`}
    <${Field} label="Why the team needs it"><textarea rows="4" maxlength="2000" value=${reason} onInput=${(e) => setReason(e.target.value)}></textarea><//>
    <footer>
      <button type="button" class="secondary" onClick=${onCancel}>Cancel</button>
      <button type="submit" disabled=${busy}>Request access</button>
    </footer>
  </form>`;
}

const requestBadge = {
  pending: html`<span class="badge warn">Pending</span>`,
  approved: html`<span class="badge good">Approved</span>`,
  denied: html`<span class="badge bad">Denied</span>`,
};

// AccessRequests lists requests for MCP access, with approve and deny
// for operators who may decide.
function AccessRequests({ tick, onChange }) {
  const [status, setStatus] = useState('pending');
  const [deciding, setDeciding] = useState(null);
  const state = useLoad(() => api('GET', '/admin/v1/mcp/requests' + qs({ status })), [status, tick]);
  return html`<section class=${`panel${state.data?.data?.some((q) => q.can_decide) ? ' attention' : ''}`}>
    <div class="panel-header"><h2>Access requests</h2>
      <label class="inline"><span>Status</span><select value=${status} onChange=${(e) => setStatus(e.target.value)}>
        <option value="pending">Pending</option><option value="approved">Approved</option><option value="denied">Denied</option><option value="">All</option>
      </select></label>
    </div>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rows=${d.data}
      empty=${status === 'pending' ? 'No requests are waiting.' : 'No requests.'}
      columns=${[
        { label: 'Team', render: (q) => html`<a href=${`#/teams/${encodeURIComponent(q.team_id)}`}>${q.team_name || q.team_id}</a>` },
        { label: 'Server', render: (q) => html`<code>${q.server}</code>` },
        { label: 'Reason', render: (q) => html`<span class="small">${q.reason}</span>` },
        { label: 'Requested', render: (q) => html`<${Ago} t=${q.created_at} /><div class="muted small">${q.requested_by}</div>` },
        { label: 'Status', render: (q) => html`${requestBadge[q.status]}${q.decided_by && html`<div class="muted small">${q.decided_by}${q.note && html` · ${q.note}`}</div>`}` },
        { label: '', render: (q) => q.can_decide && html`<div class="row-actions">
          <button class="link" onClick=${() => setDeciding({ q, action: 'approve' })}>Approve</button>
          <button class="link danger" onClick=${() => setDeciding({ q, action: 'deny' })}>Deny</button>
        </div>` },
      ]}
    />`}<//>
    ${deciding && html`<${Modal} title=${`${deciding.action === 'approve' ? 'Approve' : 'Deny'} ${deciding.q.server} for ${deciding.q.team_name}`} onClose=${() => setDeciding(null)}>
      <${DecideForm} action=${deciding.action} request=${deciding.q} onCancel=${() => setDeciding(null)}
        onSubmit=${async (note) => { await api('POST', `/admin/v1/mcp/requests/${encodeURIComponent(deciding.q.id)}/${deciding.action}`, { note }); setDeciding(null); onChange(); }} />
    <//>`}
  </section>`;
}

function DecideForm({ action, request, onSubmit, onCancel }) {
  const [note, setNote] = useState('');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      await onSubmit(note.trim());
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return html`<form class="form" onSubmit=${submit}>
    <${ErrorBox} error=${error} />
    <p>${action === 'approve' ? `${request.server} is added to ${request.team_name}'s allowed MCP servers.` : `${request.team_name} keeps its current MCP servers.`}</p>
    <${Field} label="Note" hint="Optional; the requester sees it."><input value=${note} onInput=${(e) => setNote(e.target.value)} /><//>
    <footer>
      <button type="button" class="secondary" onClick=${onCancel}>Cancel</button>
      <button type="submit" class=${action === 'deny' ? 'danger' : ''} disabled=${busy}>${action === 'approve' ? 'Approve' : 'Deny'}</button>
    </footer>
  </form>`;
}

function Review({ tool, canApprove, onClose, onApproved }) {
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const approve = async () => {
    setBusy(true);
    try {
      await api('POST', `/admin/v1/mcp/servers/${encodeURIComponent(tool.server)}/tools/${encodeURIComponent(tool.tool)}/approve`, { hash: tool.hash });
      onApproved();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return html`<${Modal} title=${`${tool.server}/${tool.tool}`} onClose=${onClose}>
    <${ErrorBox} error=${error} />
    <p>${statusBadge[tool.status]} ${tool.status === 'changed' ? ' The definition differs from the one approved. Check the description and input schema for instructions aimed at the model.' : ''}</p>
    <${JSONView} value=${tool.definition} />
    <p class="muted small">Definition hash <code>${tool.hash}</code></p>
    <footer>
      <button class="secondary" onClick=${onClose}>Close</button>
      ${tool.status !== 'approved' && (canApprove ? html`<button disabled=${busy} onClick=${approve}>Approve this definition</button>` : html`<span class="muted small">Only owners approve tools.</span>`)}
    </footer>
  <//>`;
}

// Connections lists users' connections to per-user servers. Only owners
// see them: they belong to people, not organizations.
function Connections() {
  const state = useLoad(() => api('GET', '/admin/v1/mcp/connections?limit=500'));
  const [revoking, setRevoking] = useState(null);
  return html`<section class="panel">
    <h2>User connections</h2>
    <p class="muted">Users connect their own accounts to per-user servers from their MCP client. Revoking a connection makes its user connect again.</p>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(c) => c.server + '/' + c.subject}
      rows=${d.data}
      empty="Nobody has connected an account yet."
      columns=${[
        { label: 'Server', render: (c) => html`<code>${c.server}</code>` },
        { label: 'User', render: (c) => c.subject },
        { label: 'Scopes', render: (c) => html`<span class="small">${(c.scopes || []).join(' ')}</span>` },
        { label: 'Connected', render: (c) => html`<${Ago} t=${c.created_at} />` },
        { label: 'Last used', render: (c) => html`<${Ago} t=${c.last_used_at} />` },
        { label: '', render: (c) => html`<button class="link danger" onClick=${() => setRevoking(c)}>Revoke</button>` },
      ]}
    />`}<//>
    ${revoking && html`<${Confirm}
      title="Revoke connection"
      message=${`${revoking.subject} will have to connect their ${revoking.server} account again. The gateway also asks ${revoking.server} to revoke the token.`}
      action="Revoke"
      onConfirm=${async () => {
        await api('DELETE', `/admin/v1/mcp/connections/${encodeURIComponent(revoking.server)}/${encodeURIComponent(revoking.subject)}`);
        setRevoking(null);
        state.reload();
      }}
      onClose=${() => setRevoking(null)}
    />`}
  </section>`;
}

// Calls lists recent MCP calls.
function Calls() {
  const s = useSession();
  const [org, setOrg] = useState(defaultOrg(s));
  const [list, setList] = useState({ items: [], more: false });
  const [error, setError] = useState(null);
  const load = async (after) => {
    setError(null);
    try {
      const d = await api('GET', '/admin/v1/mcp/calls' + qs({ org_id: org, limit: 50, after }));
      setList((l) => ({ items: after ? [...l.items, ...d.data] : d.data, more: d.has_more }));
    } catch (err) {
      setError(err);
    }
  };
  useEffect(() => {
    load();
  }, [org]);
  return html`<section class="panel">
    <div class="panel-header"><h2>Recent calls</h2><${OrgPicker} value=${org} onChange=${setOrg} /></div>
    <${ErrorBox} error=${error} />
    <${Table}
      rows=${list.items}
      empty="No MCP calls yet."
      columns=${[
        { label: 'Time', render: (c) => html`<${Ago} t=${c.ts} />` },
        { label: 'Virtual server', render: (c) => c.virtual_server },
        { label: 'Call', render: (c) => html`<code>${c.method}${c.name ? ' ' + c.name : ''}</code>` },
        { label: 'Upstream', render: (c) => c.server },
        { label: 'Who', render: (c) => c.subject || c.key_id },
        { label: 'Result', render: (c) => (c.status === 'ok' ? html`<span class="badge good">OK</span>` : html`<span class="badge bad" title=${c.error_code || ''}>${c.status}</span>`) },
        { label: 'Latency', num: true, render: (c) => `${num(c.latency_ms)} ms` },
        { label: 'Body', render: (c) => (c.has_body && s.can('bodies.read', c.org_id, c.team_id) ? html`<a href=${`#/requests/${encodeURIComponent(c.request_id)}`}>View</a>` : '') },
      ]}
    />
    ${list.more && html`<p class="center"><button class="secondary" onClick=${() => load(list.items[list.items.length - 1].id)}>Load more</button></p>`}
  </section>`;
}
