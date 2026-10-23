import {
  html, useState, useEffect, api, qs, useLoad, useSession, Loading, Table, Modal, Confirm, PageHeader, ErrorBox, JSONView, OrgPicker, defaultOrg, when, num,
} from '../lib.js';

const statusBadge = {
  approved: html`<span class="badge good">Approved</span>`,
  pending: html`<span class="badge warn">New</span>`,
  changed: html`<span class="badge bad">Changed</span>`,
};

export function MCP() {
  const s = useSession();
  const [review, setReview] = useState(null);
  const [filter, setFilter] = useState('');
  const state = useLoad(async () => {
    const [servers, tools, virtual] = await Promise.all([
      api('GET', '/admin/v1/mcp/servers'),
      api('GET', '/admin/v1/mcp/tools'),
      api('GET', '/admin/v1/mcp/virtual-servers'),
    ]);
    return { servers: servers.data, tools: tools.data, virtual: virtual.data };
  });

  return html`
    <${PageHeader} title="MCP" sub="Upstream MCP servers, the tools they offer, and the virtual servers clients connect to." />
    <${Loading} state=${state}>${(d) => {
      const toReview = d.tools.filter((t) => t.status !== 'approved');
      const shown = filter ? d.tools.filter((t) => t.status === filter) : d.tools;
      return html`
        ${toReview.length > 0 && html`<section class="panel attention">
          <h2>Tools to review</h2>
          <p class="muted">Pinned servers hide a tool from clients until an owner approves its definition, and hide it again if the definition changes.</p>
          <${Table}
            rowKey=${(t) => t.server + '/' + t.tool}
            rows=${toReview}
            columns=${[
              { label: 'Tool', render: (t) => html`<code>${t.server}/${t.tool}</code>` },
              { label: 'Status', render: (t) => statusBadge[t.status] },
              { label: 'Seen', render: (t) => when(t.updated_at) },
              { label: '', render: (t) => html`<button class="link" onClick=${() => setReview(t)}>Review</button>` },
            ]}
          />
        </section>`}
        <section class="panel">
          <h2>Servers</h2>
          <${Table}
            rowKey=${(v) => v.name}
            rows=${d.servers}
            empty="No MCP servers are configured."
            columns=${[
              { label: 'Server', render: (v) => html`<strong>${v.name}</strong><br /><span class="muted small">${v.url}</span>` },
              { label: 'Status', render: (v) => (v.up ? html`<span class="badge good">Up</span>` : html`<span class="badge bad">Down</span>`) },
              { label: 'Credentials', render: (v) => (v.user_auth ? 'Per user' : 'Shared') },
              { label: 'Pinning', render: (v) => v.pin_tools },
              { label: 'Tools', num: true, render: (v) => html`${num(v.tools)}${v.hidden_tools ? html` <span class="muted small">(${num(v.hidden_tools)} hidden)</span>` : ''}` },
              { label: 'Prompts', num: true, render: (v) => num(v.prompts) },
              { label: 'Resources', num: true, render: (v) => num(v.resources) },
              { label: 'Refreshed', render: (v) => when(v.refreshed_at) },
            ]}
          />
        </section>
        <section class="panel">
          <h2>Virtual servers</h2>
          <${Table}
            rowKey=${(v) => v.name}
            rows=${d.virtual}
            empty="No virtual servers are configured."
            columns=${[
              { label: 'Name', render: (v) => html`<strong>${v.name}</strong>${v.description && html`<br /><span class="muted small">${v.description}</span>`}` },
              { label: 'Endpoint', render: (v) => html`<code class="small">/mcp/${v.name}</code>` },
              {
                label: 'Upstreams',
                render: (v) => html`<ul class="plain">${v.servers.map((u) => html`<li key=${u.server}><code>${u.server}</code>${u.prefix && html` as <code>${u.prefix}*</code>`}${u.tools?.length ? html` <span class="muted small">${u.tools.join(', ')}</span>` : html` <span class="muted small">all tools</span>`}</li>`)}</ul>`,
              },
            ]}
          />
        </section>
        <section class="panel">
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
              { label: 'Updated', render: (t) => when(t.updated_at) },
              { label: '', render: (t) => html`<button class="link" onClick=${() => setReview(t)}>Definition</button>` },
            ]}
          />
        </section>
        ${s.owner && d.servers.some((v) => v.user_auth) && html`<${Connections} />`}
        <${Calls} />
        ${review && html`<${Review} tool=${review} canApprove=${s.owner} onClose=${() => setReview(null)} onApproved=${() => { setReview(null); state.reload(); }} />`}`;
    }}<//>`;
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
        { label: 'Connected', render: (c) => when(c.created_at) },
        { label: 'Last used', render: (c) => when(c.last_used_at) },
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
        { label: 'Time', render: (c) => when(c.ts) },
        { label: 'Virtual server', render: (c) => c.virtual_server },
        { label: 'Call', render: (c) => html`<code>${c.method}${c.name ? ' ' + c.name : ''}</code>` },
        { label: 'Upstream', render: (c) => c.server },
        { label: 'Who', render: (c) => c.subject || c.key_id },
        { label: 'Result', render: (c) => (c.status === 'ok' ? html`<span class="badge good">OK</span>` : html`<span class="badge bad" title=${c.error_code || ''}>${c.status}</span>`) },
        { label: 'Latency', num: true, render: (c) => `${num(c.latency_ms)} ms` },
        { label: 'Body', render: (c) => (c.has_body && s.canWrite(c.org_id) ? html`<a href=${`#/requests/${encodeURIComponent(c.request_id)}`}>View</a>` : '') },
      ]}
    />
    ${list.more && html`<p class="center"><button class="secondary" onClick=${() => load(list.items[list.items.length - 1].id)}>Load more</button></p>`}
  </section>`;
}
