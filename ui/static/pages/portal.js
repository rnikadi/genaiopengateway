// The developer portal (docs/developer-portal.md): what each of your
// teams may use, your own keys, this month's use of them, and code to
// start with.
import {
  html, useState, api, useLoad, Loading, Table, Modal, Confirm, PageHeader, CopyButton, Field, ErrorBox, Ago, usd, num, when, Tabs,
} from '../lib.js';

const statusOf = (k) => {
  const now = Date.now();
  if (k.revoked_at) return 'Revoked';
  if (k.expires_at && new Date(k.expires_at) <= now) return 'Expired';
  if (k.previous_expires_at && new Date(k.previous_expires_at) > now) return 'Rotating';
  return 'Active';
};

export function Portal({ params }) {
  const state = useLoad(() => api('GET', '/admin/v1/portal'), []);
  const [team, setTeam] = useState(params.get('team') || '');
  const [modal, setModal] = useState(null);
  return html`<${PageHeader} title="My keys and access" sub="Create and rotate your own keys, see what your teams may use, and copy code to get started." />
    <${Loading} state=${state}>${() => {
      const { teams, limits, owner } = state.data;
      const connections = state.data.mcp_connections_url && html`<section class="panel">
        <h2>Your MCP connections</h2>
        <p class="muted small">The accounts you connected so the gateway can call MCP servers as you, such as GitHub or Linear. See them and disconnect any of them on their own page.</p>
        <p><a href=${state.data.mcp_connections_url} target="_blank" rel="noopener">Open your MCP connections</a></p>
      </section>`;
      if (!teams.length) {
        return html`<section class="panel"><p>You are not a developer on any team yet. Ask an admin to map one of your groups to the <code>developer</code> role for your team.</p></section>${connections}`;
      }
      const t = teams.find((x) => x.id === team) || teams[0];
      const active = teams.flatMap((x) => x.keys).filter((k) => statusOf(k) === 'Active' || statusOf(k) === 'Rotating').length;
      const close = () => setModal(null);
      const done = () => { close(); state.reload(); };
      return html`
        ${teams.length > 1 && html`<${Field} label="Team"><select value=${t.id} onChange=${(e) => setTeam(e.target.value)}>
          ${teams.map((x) => html`<option key=${x.id} value=${x.id}>${x.name} (${x.org_name})</option>`)}
        </select><//>`}
        <section class="panel">
          <div class="panel-header"><h2>Your keys in ${t.name}</h2>
            <button disabled=${active >= limits.max_keys_per_user} onClick=${() => setModal({ kind: 'create', team: t })}>Create a key</button>
          </div>
          <p class="muted small">Keys belong to ${owner}. You may have ${limits.max_keys_per_user} active keys across your teams (${active} now), each for at most ${limits.max_key_ttl}. A key may use what ${t.name} may use, within its limits.</p>
          <${Table} rows=${t.keys} empty="You have no keys in this team yet." columns=${[
            { label: 'Name', render: (k) => k.name },
            { label: 'Key', render: (k) => html`<code>${k.prefix}…</code>` },
            { label: 'Status', render: (k) => html`<span class="badge">${statusOf(k)}</span>` },
            { label: 'Expires', render: (k) => (k.expires_at ? html`<${Ago} t=${k.expires_at} />` : 'Never') },
            { label: 'Last used', render: (k) => (k.last_used_at ? html`<${Ago} t=${k.last_used_at} />` : 'Never') },
            { label: '', render: (k) => !k.revoked_at && html`<span class="row-actions">
              <a href=${'#/explain?key=' + encodeURIComponent(k.id)}>Why denied?</a>
              <button class="link" onClick=${() => setModal({ kind: 'rotate', key: k })}>Rotate</button>
              <button class="link danger" onClick=${() => setModal({ kind: 'revoke', key: k })}>Revoke</button>
            </span>` },
          ]} />
        </section>
        <div class="grid2">
          <section class="panel">
            <h2>Models</h2>
            <${Table} rowKey=${(m) => m.name} rows=${t.models} empty="This team may not use any model yet." columns=${[{ label: 'Model', render: (m) => html`<code>${m.name}</code>` }]} />
            <p class="muted small">${[
              t.budget && `Budget ${usd(t.budget.amount_usd)} per ${t.budget.period.replace(/ly$/, '').replace('dai', 'day')}`,
              t.rpm && `${num(t.rpm)} requests a minute`,
              t.tpm && `${num(t.tpm)} tokens a minute`,
            ].filter(Boolean).join(' · ') || 'No team limits beyond the organization’s.'}</p>
          </section>
          <section class="panel">
            <h2>Your use this month</h2>
            <${Table} rowKey=${(u) => u.model} rows=${t.usage} empty="No requests with your keys this month." columns=${[
              { label: 'Model', render: (u) => u.model },
              { label: 'Requests', num: true, render: (u) => num(u.requests) },
              { label: 'Tokens', num: true, render: (u) => num(u.tokens) },
              { label: 'Cost', num: true, render: (u) => usd(u.cost_usd) },
            ]} />
          </section>
        </div>
        <section class="panel">
          <h2>MCP servers</h2>
          <${Table} rowKey=${(m) => m.name} rows=${t.mcp_servers} empty="No MCP servers are set up on this gateway." columns=${[
            { label: 'Server', render: (m) => html`<code>${m.name}</code>${m.description && html` <span class="muted small">${m.description}</span>`}` },
            { label: 'Endpoint', render: (m) => html`<code class="small">/mcp/${m.name}</code>` },
            { label: 'Access', render: (m) => (m.access === 'allowed' ? 'Yes' : m.pending ? 'Requested' : m.access === 'blocked' ? 'Not allowed in this organization'
              : html`<button class="link" onClick=${() => setModal({ kind: 'request', team: t, server: m.name })}>Request access</button>`) },
          ]} />
        </section>
        <${Quickstart} models=${t.models} />
        ${modal?.kind === 'create' && html`<${CreateKey} team=${modal.team} maxTTL=${limits.max_key_ttl} onClose=${close} onCreated=${(k) => { state.reload(); setModal({ kind: 'secret', key: k, title: 'Key created' }); }} />`}
        ${modal?.kind === 'rotate' && html`<${Confirm} title="Rotate key" action="Rotate" danger=${false} onClose=${close}
          message=${`Give ${modal.key.name} a new secret? The old secret keeps working for an hour, so you can switch over.`}
          onConfirm=${async () => { const k = await api('POST', `/admin/v1/portal/keys/${encodeURIComponent(modal.key.id)}/rotate`); state.reload(); setTimeout(() => setModal({ kind: 'secret', key: k, title: 'Key rotated' })); }} />`}
        ${modal?.kind === 'revoke' && html`<${Confirm} title="Revoke key" action="Revoke key" onClose=${close}
          message=${`Revoke ${modal.key.name}? Requests with it fail at once, and it cannot be restored.`}
          onConfirm=${async () => { await api('POST', `/admin/v1/portal/keys/${encodeURIComponent(modal.key.id)}/revoke`); done(); }} />`}
        ${modal?.kind === 'request' && html`<${RequestAccess} team=${modal.team} server=${modal.server} onClose=${close} onDone=${done} />`}
        ${modal?.kind === 'secret' && html`<${Modal} title=${modal.title} onClose=${close}>
          <p>Copy the key now. The gateway stores only its hash, so it cannot be shown again.</p>
          <div class="secret"><code>${modal.key.key}</code><${CopyButton} text=${modal.key.key} /></div>
          ${modal.key.previous_expires_at && html`<p class="muted">The previous secret keeps working until ${when(modal.key.previous_expires_at)}.</p>`}
          <footer><button onClick=${close}>Done</button></footer>
        <//>`}`;
    }}<//>`;
}

function CreateKey({ team, maxTTL, onClose, onCreated }) {
  const [name, setName] = useState('');
  const [ttl, setTTL] = useState('30d');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const choices = ['7d', '30d', '90d', '180d', '365d'].filter((d) => parseInt(d) <= parseInt(maxTTL) || !maxTTL.endsWith('d'));
  const submit = async (e) => {
    e.preventDefault();
    setBusy(true);
    try {
      onCreated(await api('POST', `/admin/v1/portal/teams/${encodeURIComponent(team.id)}/keys`, { name, expires_in: ttl }));
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return html`<${Modal} title=${'Create a key in ' + team.name} onClose=${onClose}>
    <form class="form" onSubmit=${submit}>
      <${ErrorBox} error=${error} />
      <${Field} label="Name" hint="What it is for, such as the app or the laptop."><input required maxlength="100" value=${name} onInput=${(e) => setName(e.target.value)} /><//>
      <${Field} label="Expires after"><select value=${ttl} onChange=${(e) => setTTL(e.target.value)}>
        ${(choices.length ? choices : [maxTTL]).map((d) => html`<option key=${d} value=${d}>${d.replace('d', ' days')}</option>`)}
      </select><//>
      <footer><button type="button" class="secondary" onClick=${onClose}>Cancel</button><button type="submit" disabled=${busy}>Create key</button></footer>
    </form>
  <//>`;
}

function RequestAccess({ team, server, onClose, onDone }) {
  const [reason, setReason] = useState('');
  const [error, setError] = useState(null);
  const submit = async (e) => {
    e.preventDefault();
    try {
      await api('POST', `/admin/v1/portal/teams/${encodeURIComponent(team.id)}/requests`, { server, reason });
      onDone();
    } catch (err) {
      setError(err);
    }
  };
  return html`<${Modal} title=${`Request ${server} for ${team.name}`} onClose=${onClose}>
    <form class="form" onSubmit=${submit}>
      <${ErrorBox} error=${error} />
      <p class="muted">Your team's admins see the request on the MCP page and decide. Access applies to the whole team.</p>
      <${Field} label="Why does the team need it?"><textarea maxlength="2000" value=${reason} onInput=${(e) => setReason(e.target.value)} /><//>
      <footer><button type="button" class="secondary" onClick=${onClose}>Cancel</button><button type="submit">Send request</button></footer>
    </form>
  <//>`;
}

// Quickstart shows code for the team's first model, with the key read
// from the environment so that it never lands in source control.
function Quickstart({ models }) {
  const [lang, setLang] = useState('curl');
  const base = location.origin;
  const model = models[0]?.name || 'MODEL';
  const code = {
    curl: `export GATEWAY_KEY=sk-...   # your key
curl ${base}/v1/chat/completions \\
  -H "Authorization: Bearer $GATEWAY_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"model": "${model}", "messages": [{"role": "user", "content": "Hello"}]}'`,
    python: `# pip install openai; export GATEWAY_KEY=sk-...
import os
from openai import OpenAI

client = OpenAI(base_url="${base}/v1", api_key=os.environ["GATEWAY_KEY"])
reply = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "Hello"}],
)
print(reply.choices[0].message.content)`,
    node: `// npm install openai; export GATEWAY_KEY=sk-...
import OpenAI from 'openai';

const client = new OpenAI({ baseURL: '${base}/v1', apiKey: process.env.GATEWAY_KEY });
const reply = await client.chat.completions.create({
  model: '${model}',
  messages: [{ role: 'user', content: 'Hello' }],
});
console.log(reply.choices[0].message.content);`,
    env: `# Tools that read the OpenAI variables work unchanged
export OPENAI_BASE_URL=${base}/v1
export OPENAI_API_KEY=sk-...   # your key`,
  }[lang];
  return html`<section class="panel">
    <h2>Get started</h2>
    <${Tabs} tabs=${{ curl: 'curl', python: 'Python', node: 'Node.js', env: 'Environment' }} value=${lang} onChange=${setLang} />
    <pre class="code"><code>${code}</code></pre>
    <${CopyButton} text=${code} label="Copy code" />
  </section>`;
}
