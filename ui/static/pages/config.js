import { html, useState, api, useLoad, Loading, Table, PageHeader, Modal, Field, ErrorBox, Tabs, Confirm, when, providerTypes } from '../lib.js';

const kindNames = {
  providers: 'Providers',
  models: 'Models',
  guardrails: 'Guardrails',
  mcp_servers: 'MCP servers',
  mcp_virtual_servers: 'Virtual MCP servers',
  alert_webhooks: 'Alert webhooks',
};

// A starting spec for each kind, in the config file's shape.
const templates = {
  providers: { type: 'openai', base_url: 'https://api.openai.com/v1', api_key: '' },
  models: { targets: [{ provider: '', model: '' }] },
  guardrails: { type: 'pii', action: 'block', stages: ['input'] },
  mcp_servers: { url: 'https://mcp.example.com/mcp', pin_tools: 'strict' },
  mcp_virtual_servers: { servers: [{ server: '' }] },
  alert_webhooks: { url: '', format: 'slack', events: ['budget.threshold'] },
};

// Config lists configuration objects from the file and the store, and lets
// owners add and change the stored ones (docs/admin-ui-config-editing.md).
export function Config() {
  const [reload, setReload] = useState(0);
  const state = useLoad(() => api('GET', '/admin/v1/config'), [reload]);
  const [kind, setKind] = useState('providers');
  const [editing, setEditing] = useState(null);
  const [deleting, setDeleting] = useState(null);
  const done = () => {
    setEditing(null);
    setReload((n) => n + 1);
  };
  return html`
    <${PageHeader} title="Configuration" sub="Objects from the config file are read-only here. Objects added here are stored in the database, apply to every replica within the refresh interval, and never override the file.">
      <button onClick=${() => setEditing({ kind, name: '', spec: templates[kind], isNew: true })}>New ${kindNames[kind].toLowerCase().replace(/s$/, '')}</button>
    <//>
    <${Tabs} tabs=${kindNames} value=${kind} onChange=${setKind} />
    <${Loading} state=${state}>${(d) => html`
      ${d.error && html`<div class="notice error">The stored objects do not apply to the config file, so the gateway is running without the latest changes: ${d.error}</div>`}
      <${Table}
        rowKey=${(o) => o.source + o.name}
        rows=${d.data.filter((o) => o.kind === kind)}
        empty=${`No ${kindNames[kind].toLowerCase()} yet.`}
        columns=${[
          { label: 'Name', render: (o) => html`<code>${o.name}</code>${o.spec?.type && html`<div class="muted small">${providerTypes[o.spec.type] || o.spec.type}</div>`}` },
          { label: 'Source', render: (o) => (o.source === 'file' ? html`<span class="badge">From config file</span>` : o.shadowed ? html`<span class="badge warn" title="The config file defines the same name, and the file wins">Shadowed by the file</span>` : html`<span class="badge good">Stored</span>`) },
          { label: 'Updated', render: (o) => (o.updated_at ? html`${when(o.updated_at)}<div class="muted small">${o.updated_by}</div>` : '') },
          {
            label: '',
            render: (o) => o.source === 'store' && html`<span class="row-actions">
              <button class="secondary" onClick=${() => setEditing({ kind, name: o.name, spec: o.spec })}>Edit</button>
              <button class="secondary" onClick=${() => setDeleting(o)}>Delete</button>
            </span>`,
          },
        ]}
      />`}<//>
    ${editing && html`<${Editor} obj=${editing} providers=${(state.data?.data || []).filter((o) => o.kind === 'providers').map((o) => o.name)} onDone=${done} onClose=${() => setEditing(null)} />`}
    ${deleting && html`<${Confirm}
      title=${`Delete ${deleting.name}`}
      message="The gateway stops using it on every replica within the refresh interval. Anything that still refers to it must be changed first."
      action="Delete"
      onConfirm=${async () => {
        await api('DELETE', `/admin/v1/config/${deleting.kind}/${deleting.name}`);
        setReload((n) => n + 1);
      }}
      onClose=${() => setDeleting(null)} />`}`;
}

// Editor changes one stored object, with a form for providers and models
// and the whole spec as JSON for everything.
function Editor({ obj, providers, onDone, onClose }) {
  const hasForm = obj.kind === 'providers' || obj.kind === 'models';
  const [name, setName] = useState(obj.name);
  const [spec, setSpec] = useState(obj.spec);
  const [text, setText] = useState(JSON.stringify(obj.spec, null, 2));
  const [view, setView] = useState(hasForm ? 'form' : 'json');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);

  const switchView = (v) => {
    setError(null);
    if (v === 'form') {
      try {
        setSpec(JSON.parse(text));
      } catch (err) {
        setError(new Error('The JSON is not valid: ' + err.message));
        return;
      }
    } else {
      setText(JSON.stringify(spec, null, 2));
    }
    setView(v);
  };
  const submit = async (e) => {
    e.preventDefault();
    setError(null);
    let body = spec;
    if (view === 'json') {
      try {
        body = JSON.parse(text);
      } catch (err) {
        setError(new Error('The JSON is not valid: ' + err.message));
        return;
      }
    }
    setBusy(true);
    try {
      await api('PUT', `/admin/v1/config/${obj.kind}/${name}`, body);
      onDone();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return html`<${Modal} title=${obj.isNew ? `New ${kindNames[obj.kind].toLowerCase().replace(/s$/, '')}` : `Edit ${obj.name}`} onClose=${onClose}>
    <form class="form" onSubmit=${submit}>
      <${ErrorBox} error=${error} />
      ${obj.isNew && html`<${Field} label="Name" hint="Lowercase letters, digits, '.', '_' and '-'. It cannot be changed later.">
        <input required pattern="[a-z0-9][a-z0-9._\\-]{0,62}" value=${name} onInput=${(e) => setName(e.target.value)} />
      <//>`}
      ${hasForm && html`<${Tabs} tabs=${{ form: 'Form', json: 'Edit as JSON' }} value=${view} onChange=${switchView} />`}
      ${view === 'json'
        ? html`<${Field} label="Spec" hint='The same fields as this object in config.yaml. Secrets show as "set": leave them as "set" to keep them, or send "" to remove one.'>
            <textarea class="mono" rows="14" spellcheck="false" value=${text} onInput=${(e) => setText(e.target.value)}></textarea>
          <//>`
        : obj.kind === 'providers'
          ? html`<${ProviderForm} spec=${spec} onChange=${setSpec} />`
          : html`<${ModelForm} spec=${spec} providers=${providers} onChange=${setSpec} />`}
      <footer>
        <button type="button" class="secondary" onClick=${onClose}>Cancel</button>
        <button type="submit" disabled=${busy}>Save</button>
      </footer>
    </form>
  <//>`;
}

function ProviderForm({ spec, onChange }) {
  const set = (k) => (e) => {
    const next = { ...spec, [k]: e.target.value };
    // An empty key is left out, which keeps a stored one.
    if (e.target.value === '') delete next[k];
    onChange(next);
  };
  const t = spec.type || 'openai';
  return html`
    <${Field} label="Type">
      <select value=${t} onChange=${set('type')}>${Object.entries(providerTypes).map(([k, v]) => html`<option value=${k}>${v}</option>`)}</select>
    <//>
    ${t === 'vertex'
      ? html`<div class="row">
          <${Field} label="Project"><input value=${spec.project || ''} onInput=${set('project')} /><//>
          <${Field} label="Location"><input value=${spec.location || ''} onInput=${set('location')} placeholder="us-central1" /><//>
        </div>`
      : t === 'bedrock'
        ? html`<${Field} label="Region"><input value=${spec.region || ''} onInput=${set('region')} placeholder="us-east-1" /><//>`
        : html`<${Field} label="Base URL"><input value=${spec.base_url || ''} onInput=${set('base_url')} /><//>`}
    ${t !== 'vertex' && html`<${Field} label="API key" hint=${spec.api_key === 'set' ? 'A key is stored. Leave this as is to keep it.' : t === 'bedrock' ? 'Leave empty to use the AWS default credential chain.' : 'Encrypted in the database and never shown again.'}>
      <input type="password" autocomplete="off" value=${spec.api_key === 'set' ? '' : spec.api_key || ''} placeholder=${spec.api_key === 'set' ? '••••••••' : ''} onInput=${set('api_key')} />
    <//>`}
    <${Field} label="Timeout" hint="For example 60s. Empty uses the default."><input value=${spec.timeout || ''} onInput=${set('timeout')} /><//>`;
}

// mediaPrices are the price fields of media endpoints (docs/modalities.md).
const mediaPrices = [
  ['per_image', 'USD per image'],
  ['per_mchars', 'USD per million characters (speech)'],
  ['per_audio_minute', 'USD per audio minute (transcription)'],
  ['per_video_second', 'USD per video second'],
  ['per_request', 'USD per request'],
];

function ModelForm({ spec, providers, onChange }) {
  const targets = spec.targets?.length ? spec.targets : [{ provider: '', model: '' }];
  const setTarget = (i, patch) => onChange({ ...spec, targets: targets.map((t, n) => (n === i ? { ...t, ...patch } : t)) });
  const setPrice = (i, k, v) => {
    const price = { ...(targets[i].price || {}) };
    if (v === '') delete price[k];
    else price[k] = Number(v);
    setTarget(i, { price });
  };
  return html`
    <p class="muted small">Targets are tried in order: when one fails, the next one answers.</p>
    ${targets.map((t, i) => html`<div class="panel" key=${i}>
      <div class="row">
        <${Field} label=${`Target ${i + 1}: provider`}>
          <select required value=${t.provider} onChange=${(e) => setTarget(i, { provider: e.target.value })}>
            <option value="">Choose…</option>${providers.map((p) => html`<option value=${p}>${p}</option>`)}
          </select>
        <//>
        <${Field} label="Upstream model"><input required value=${t.model} onInput=${(e) => setTarget(i, { model: e.target.value })} /><//>
      </div>
      <div class="row">
        <${Field} label="Input, USD per million tokens"><input type="number" min="0" step="any" value=${t.price?.input_per_mtok ?? ''} onInput=${(e) => setPrice(i, 'input_per_mtok', e.target.value)} /><//>
        <${Field} label="Output, USD per million tokens"><input type="number" min="0" step="any" value=${t.price?.output_per_mtok ?? ''} onInput=${(e) => setPrice(i, 'output_per_mtok', e.target.value)} /><//>
      </div>
      <details open=${mediaPrices.some(([k]) => t.price?.[k])}>
        <summary class="small">Media prices: images, speech, audio, video, requests</summary>
        <div class="row">
          ${mediaPrices.map(([k, label]) => html`<${Field} label=${label} key=${k}><input type="number" min="0" step="any" value=${t.price?.[k] ?? ''} onInput=${(e) => setPrice(i, k, e.target.value)} /><//>`)}
        </div>
      </details>
      ${targets.length > 1 && html`<button type="button" class="secondary small" onClick=${() => onChange({ ...spec, targets: targets.filter((_, n) => n !== i) })}>Remove target</button>`}
    </div>`)}
    <button type="button" class="secondary" onClick=${() => onChange({ ...spec, targets: [...targets, { provider: '', model: '' }] })}>Add a fallback target</button>`;
}
