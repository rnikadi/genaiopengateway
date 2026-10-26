import { html, useState, api, useLoad, useSession, Loading, Table, PageHeader, Modal, Field, ErrorBox, Tabs, Confirm, when, providerTypes } from '../lib.js';

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

// namesOf lists the names of each kind, from the file and the store.
const namesOf = (objects) => {
  const out = {};
  for (const o of objects) (out[o.kind] ||= []).includes(o.name) || out[o.kind].push(o.name);
  return out;
};

const sources = {
  file: html`<span class="badge">From config file</span>`,
  changed: html`<span class="badge warn" title="Changed in the UI; this version is used instead of the config file's">Changed here</span>`,
  deleted: html`<span class="badge bad" title="Deleted in the UI; the config file's version is not used">Deleted here</span>`,
  store: html`<span class="badge good">Stored</span>`,
};

// Config lists configuration objects from the file and the store, and lets
// owners add, change, delete and reset them; a change wins over the file
// (docs/admin-ui-config-editing.md, ADR 0028).
export function Config() {
  const [reload, setReload] = useState(0);
  const state = useLoad(() => api('GET', '/admin/v1/config'), [reload]);
  const detectors = useLoad(async () => (await api('GET', '/admin/v1/settings')).detectors);
  const [kind, setKind] = useState('providers');
  const [editing, setEditing] = useState(null);
  const [deleting, setDeleting] = useState(null);
  const [resetting, setResetting] = useState(null);
  const done = () => {
    setEditing(null);
    setReload((n) => n + 1);
  };
  return html`
    <${PageHeader} title="Configuration" sub="Every object can be changed here, including those from the config file. Changes are stored in the database, win over the config file, and apply to every replica within the refresh interval. Reset returns an object to the config file's version.">
      <button onClick=${() => setEditing({ kind, name: '', spec: templates[kind], isNew: true })}>New ${kindNames[kind].toLowerCase().replace(/s$/, '')}</button>
    <//>
    <${Tabs} tabs=${kindNames} value=${kind} onChange=${setKind} />
    <${Loading} state=${state}>${(d) => html`
      ${d.error && html`<div class="notice error">The stored objects do not apply to the config file, so the gateway is running without the latest changes: ${d.error}</div>`}
      <${Table}
        rowKey=${(o) => o.name}
        rows=${d.data.filter((o) => o.kind === kind)}
        empty=${`No ${kindNames[kind].toLowerCase()} yet.`}
        columns=${[
          { label: 'Name', render: (o) => html`<code>${o.name}</code>${o.spec?.type && html`<div class="muted small">${providerTypes[o.spec.type] || o.spec.type}</div>`}` },
          { label: 'Source', render: (o) => sources[o.source] },
          { label: 'Updated', render: (o) => (o.updated_at ? html`${when(o.updated_at)}<div class="muted small">${o.updated_by}</div>` : '') },
          {
            label: '',
            render: (o) => html`<span class="row-actions">
              ${o.source !== 'deleted' && html`<button class="secondary" onClick=${() => setEditing({ kind, name: o.name, spec: o.spec, fromFile: o.source !== 'store' })}>Edit</button>`}
              ${o.source !== 'deleted' && html`<button class="secondary" onClick=${() => setDeleting(o)}>Delete</button>`}
              ${(o.source === 'changed' || o.source === 'deleted') && html`<button class="secondary" onClick=${() => setResetting(o)}>Reset</button>`}
            </span>`,
          },
        ]}
      />`}<//>
    ${editing && html`<${Editor} obj=${editing} names=${namesOf(state.data?.data || [])} detectors=${detectors.data || []} onDone=${done} onClose=${() => setEditing(null)} />`}
    ${deleting && html`<${Confirm}
      title=${`Delete ${deleting.name}`}
      message=${`The gateway stops using it on every replica within the refresh interval. Anything that still refers to it must be changed first.${deleting.source === 'store' ? '' : ' It stays deleted after a restart; Reset brings back the config file\'s version.'}`}
      action="Delete"
      onConfirm=${async () => {
        await api('DELETE', `/admin/v1/config/${deleting.kind}/${deleting.name}`);
        setReload((n) => n + 1);
      }}
      onClose=${() => setDeleting(null)} />`}
    ${resetting && html`<${Confirm}
      title=${`Reset ${resetting.name}`}
      message="The change made here is removed, and the config file's version applies again on every replica within the refresh interval."
      action="Reset"
      danger=${false}
      onConfirm=${async () => {
        await api('POST', `/admin/v1/config/${resetting.kind}/${resetting.name}/reset`);
        setReload((n) => n + 1);
      }}
      onClose=${() => setResetting(null)} />`}`;
}

// Editor changes one stored object, with a form for each kind and the
// whole spec as JSON for everything.
function Editor({ obj, names, detectors, onDone, onClose }) {
  const [name, setName] = useState(obj.name);
  const [spec, setSpec] = useState(obj.spec);
  const [text, setText] = useState(JSON.stringify(obj.spec, null, 2));
  const [view, setView] = useState('form');
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
  const Form = forms[obj.kind];
  return html`<${Modal} title=${obj.isNew ? `New ${kindNames[obj.kind].toLowerCase().replace(/s$/, '')}` : `Edit ${obj.name}`} onClose=${onClose}>
    <form class="form" onSubmit=${submit}>
      <${ErrorBox} error=${error} />
      ${obj.fromFile && html`<p class="muted">This object comes from the config file. Saving stores your version, which is used instead of the file's until you reset it. Secrets shown as set keep the file's value.</p>`}
      ${obj.isNew && html`<${Field} label="Name" setting=${obj.kind === 'alert_webhooks' ? 'alerts.webhooks.name' : undefined} hint="Lowercase letters, digits, '.', '_' and '-'. It cannot be changed later.">
        <input required pattern="[a-z0-9][a-z0-9._\\-]{0,62}" value=${name} onInput=${(e) => setName(e.target.value)} />
      <//>`}
      <${Tabs} tabs=${{ form: 'Form', json: 'Edit as JSON' }} value=${view} onChange=${switchView} />
      ${view === 'json'
        ? html`<${Field} label="Spec" hint='The same fields as this object in config.yaml. Secrets show as "set": leave them as "set" to keep them, or send "" to remove one.'>
            <textarea class="mono" rows="14" spellcheck="false" value=${text} onInput=${(e) => setText(e.target.value)}></textarea>
          <//>`
        : html`<${Form} spec=${spec} names=${names} detectors=${detectors} onChange=${setSpec} />`}
      <footer>
        <button type="button" class="secondary" onClick=${onClose}>Cancel</button>
        <button type="submit" disabled=${busy}>Save</button>
      </footer>
    </form>
  <//>`;
}

// --- form helpers: each edits one path of the spec ---

const get = (spec, path) => path.split('.').reduce((o, k) => (o == null ? undefined : o[k]), spec);

// put returns spec with path set, or removed when value is undefined or
// "". Sections left empty are removed too.
function put(spec, path, value) {
  const keys = path.split('.');
  const next = JSON.parse(JSON.stringify(spec || {}));
  let o = next;
  const parents = [];
  for (const k of keys.slice(0, -1)) {
    parents.push([o, k]);
    o = o[k] && typeof o[k] === 'object' ? o[k] : (o[k] = {});
  }
  const last = keys.at(-1);
  if (value === undefined || value === '') delete o[last];
  else o[last] = value;
  for (const [p, k] of parents.reverse()) if (p[k] && !Array.isArray(p[k]) && !Object.keys(p[k]).length) delete p[k];
  return next;
}

const lines = (v) => (v || []).join('\n');
const unlines = (t) => {
  const xs = t.split('\n').map((s) => s.trim()).filter(Boolean);
  return xs.length ? xs : undefined;
};

function Text({ spec, onChange, path, ...rest }) {
  return html`<input value=${get(spec, path) ?? ''} onInput=${(e) => onChange(put(spec, path, e.target.value))} ...${rest} />`;
}

function Select({ spec, onChange, path, options, def }) {
  return html`<select value=${get(spec, path) ?? ''} onChange=${(e) => onChange(put(spec, path, e.target.value))}>
    ${def !== undefined && html`<option value="">Default (${def})</option>`}
    ${Object.entries(options).map(([k, v]) => html`<option key=${k} value=${k}>${v}</option>`)}
  </select>`;
}

function Lines({ spec, onChange, path, rows = 3 }) {
  return html`<textarea rows=${rows} value=${lines(get(spec, path))} onInput=${(e) => onChange(put(spec, path, unlines(e.target.value)))}></textarea>`;
}

function Checks({ spec, onChange, path, options }) {
  const cur = get(spec, path) || [];
  const toggle = (n) => {
    const next = cur.includes(n) ? cur.filter((x) => x !== n) : [...cur, n];
    onChange(put(spec, path, next.length ? next : undefined));
  };
  return options.length
    ? html`<div class="checks">${options.map((n) => html`<label class="check" key=${n}><input type="checkbox" checked=${cur.includes(n)} onChange=${() => toggle(n)} /> ${n}</label>`)}</div>`
    : html`<p class="muted small">None configured.</p>`;
}

// Secret is a write-only value: a stored one shows as dots, and typing
// replaces it. Leaving it untouched keeps it.
function Secret({ spec, onChange, path }) {
  const v = get(spec, path);
  return html`<input type="password" autocomplete="off" value=${v === 'set' ? '' : v || ''} placeholder=${v === 'set' ? '•••••••• (stored; type to replace)' : ''}
    onInput=${(e) => onChange(put(spec, path, e.target.value === '' && v === 'set' ? 'set' : e.target.value))} />`;
}

// Headers edits a map of header names to values; secret values show as
// stored and are kept unless replaced.
function Headers({ spec, onChange, path, secret }) {
  const rows = Object.entries(get(spec, path) || {});
  const write = (next) => onChange(put(spec, path, next.length ? Object.fromEntries(next) : undefined));
  return html`<div>
    ${rows.map(([k, v], i) => html`<div class="row" key=${i}>
      <input placeholder="Header" value=${k} onInput=${(e) => write(rows.map((r, n) => (n === i ? [e.target.value, r[1]] : r)))} />
      <span class="inline-row">
        <input type=${secret ? 'password' : 'text'} autocomplete="off" placeholder=${v === 'set' ? '•••••••• (stored)' : 'Value'} value=${v === 'set' ? '' : v}
          onInput=${(e) => write(rows.map((r, n) => (n === i ? [r[0], e.target.value || (v === 'set' ? 'set' : '')] : r)))} />
        <button type="button" class="link danger" onClick=${() => write(rows.filter((_, n) => n !== i))}>Remove</button>
      </span>
    </div>`)}
    <button type="button" class="secondary small" onClick=${() => write([...rows, ['', '']])}>Add a header</button>
  </div>`;
}

// --- one form per kind ---

function ProviderForm({ spec, onChange }) {
  const p = { spec, onChange };
  const t = spec.type || 'openai';
  return html`
    <${Field} label="Type" setting="providers.type"><${Select} ...${p} path="type" options=${providerTypes} /><//>
    ${t === 'vertex'
      ? html`<div class="row">
          <${Field} label="Project" setting="providers.project"><${Text} ...${p} path="project" /><//>
          <${Field} label="Location" setting="providers.location"><${Text} ...${p} path="location" placeholder="us-central1" /><//>
        </div>
        <${Field} label="Service account key file" setting="providers.credentials_file" hint="A path on the gateway's disk. Empty uses Application Default Credentials.">
          <${Text} ...${p} path="credentials_file" />
        <//>`
      : t === 'bedrock'
        ? html`<${Field} label="Region" setting="providers.region"><${Text} ...${p} path="region" placeholder="us-east-1" /><//>`
        : ''}
    <${Field} label="Base URL" setting="providers.base_url" hint=${t === 'openai' ? 'Required.' : 'Empty uses the provider\'s own endpoint.'}><${Text} ...${p} path="base_url" /><//>
    ${t !== 'vertex' && html`<${Field} label="API key" setting="providers.api_key" hint=${t === 'bedrock' ? 'Leave empty to use the AWS default credential chain.' : 'Encrypted in the database and never shown again.'}>
      <${Secret} ...${p} path="api_key" />
    <//>`}
    <${Field} label="Timeout" setting="providers.timeout" hint="Default: 5m."><${Text} ...${p} path="timeout" placeholder="5m" /><//>
    <${Field} label="Extra headers" setting="providers.headers" hint="Sent with every upstream request, such as an organization id."><${Headers} ...${p} path="headers" /><//>`;
}

// targetPrices are the price fields of a target (docs/modalities.md).
const targetPrices = [
  ['models.targets.price.input_per_mtok', 'Input, USD per million tokens'],
  ['models.targets.price.output_per_mtok', 'Output, USD per million tokens'],
  ['models.targets.price.cached_input_per_mtok', 'Cached input, USD per million tokens'],
  ['models.targets.price.per_image', 'USD per image'],
  ['models.targets.price.per_mchars', 'USD per million characters (speech)'],
  ['models.targets.price.per_audio_minute', 'USD per audio minute (transcription)'],
  ['models.targets.price.per_video_second', 'USD per video second'],
  ['models.targets.price.per_request', 'USD per request'],
  ['models.targets.price.audio_input_per_mtok', 'USD per million audio input tokens (realtime)'],
  ['models.targets.price.audio_output_per_mtok', 'USD per million audio output tokens (realtime)'],
  ['models.targets.price.batch_discount', 'Fraction off in batches, such as 0.5 for half price'],
];

function ModelForm({ spec, names, onChange }) {
  const providers = names.providers || [];
  const targets = spec.targets?.length ? spec.targets : [{ provider: '', model: '' }];
  const setTarget = (i, patch) => onChange({ ...spec, targets: targets.map((t, n) => (n === i ? { ...t, ...patch } : t)) });
  const setPrice = (i, k, v) => {
    const price = { ...(targets[i].price || {}) };
    if (v === '') delete price[k];
    else price[k] = Number(v);
    setTarget(i, { price });
  };
  const priceField = (t, i, [path, label]) => {
    const k = path.split('.').pop();
    return html`<${Field} label=${label} setting=${path} key=${k}><input type="number" min="0" step="any" value=${t.price?.[k] ?? ''} onInput=${(e) => setPrice(i, k, e.target.value)} /><//>`;
  };
  return html`
    <p class="muted small">Targets are tried in order: when one fails, the next one answers. Empty prices are zero.</p>
    ${targets.map((t, i) => html`<div class="panel" key=${i}>
      <div class="row">
        <${Field} label=${`Target ${i + 1}: provider`} setting="models.targets.provider">
          <select required value=${t.provider} onChange=${(e) => setTarget(i, { provider: e.target.value })}>
            <option value="">Choose…</option>${providers.map((p) => html`<option value=${p}>${p}</option>`)}
          </select>
        <//>
        <${Field} label="Upstream model" setting="models.targets.model"><input required value=${t.model} onInput=${(e) => setTarget(i, { model: e.target.value })} /><//>
      </div>
      <div class="row">${targetPrices.slice(0, 3).map((f) => priceField(t, i, f))}</div>
      <details open=${targetPrices.slice(3).some(([path]) => t.price?.[path.split('.').pop()])}>
        <summary class="small">More prices: images, speech, audio, video, requests, batch discount</summary>
        <div class="row">${targetPrices.slice(3).map((f) => priceField(t, i, f))}</div>
      </details>
      ${targets.length > 1 && html`<button type="button" class="secondary small" onClick=${() => onChange({ ...spec, targets: targets.filter((_, n) => n !== i) })}>Remove target</button>`}
    </div>`)}
    <button type="button" class="secondary" onClick=${() => onChange({ ...spec, targets: [...targets, { provider: '', model: '' }] })}>Add a fallback target</button>`;
}

const stages = ['input', 'output', 'tool_input', 'tool_output'];

function GuardrailForm({ spec, detectors, onChange }) {
  const { catalog } = useSession();
  const p = { spec, onChange };
  const t = spec.type || 'pii';
  return html`
    <div class="row">
      <${Field} label="Type" setting="guardrails.type"><${Select} ...${p} path="type" options=${{ pii: 'Personal data (pii)', regex: 'Patterns (regex)', model: 'Classifier model', webhook: 'Your service (webhook)' }} /><//>
      <${Field} label="Action" setting="guardrails.action"><${Select} ...${p} path="action" options=${{ block: 'Block', redact: 'Redact', log: 'Log only' }} /><//>
    </div>
    <${Field} label="Stages" setting="guardrails.stages" hint="Model requests (input), model responses (output), MCP tool arguments and results."><${Checks} ...${p} path="stages" options=${stages} /><//>
    ${t === 'pii' && html`<${Field} label="Detectors" setting="guardrails.detect" hint="None checked means all of them."><${Checks} ...${p} path="detect" options=${detectors} /><//>`}
    ${t === 'regex' && html`<${Field} label="Patterns" setting="guardrails.patterns" hint="One RE2 expression per line."><${Lines} ...${p} path="patterns" /><//>`}
    ${t === 'model' && html`
      <${Field} label="Model" setting="guardrails.model" hint="A virtual model, called through the gateway and metered to the caller.">
        <${Select} ...${p} path="model" options=${Object.fromEntries(catalog.models.map((m) => [m.name, m.name]))} />
      <//>
      <${Field} label="Prompt" setting="guardrails.prompt" hint="Optional instructions sent before the text. Empty sends the text alone, as Llama Guard expects."><${Lines} ...${p} path="prompt" rows="4" /><//>
      <${Field} label="Unsafe if the answer matches" setting="guardrails.unsafe_if" hint="An RE2 expression. Default: (?i)^\\s*unsafe."><${Text} ...${p} path="unsafe_if" placeholder="(?i)^\\s*unsafe" /><//>`}
    ${t === 'webhook' && html`
      <${Field} label="URL" setting="guardrails.url" hint="Encrypted in the database."><${Secret} ...${p} path="url" /><//>
      <${Field} label="Signing secret" setting="guardrails.secret" hint="Signs each call so your service can check it came from the gateway."><${Secret} ...${p} path="secret" /><//>`}
    ${(t === 'model' || t === 'webhook') && html`<div class="row">
      <${Field} label="Timeout" setting="guardrails.timeout" hint="Default: 2s, at most 30s."><${Text} ...${p} path="timeout" placeholder="2s" /><//>
      <${Field} label="If the check fails" setting="guardrails.on_error"><${Select} ...${p} path="on_error" def="allow" options=${{ allow: 'Allow the request', block: 'Block the request' }} /><//>
    </div>`}`;
}

function MCPServerForm({ spec, onChange }) {
  const p = { spec, onChange };
  const userAuth = !!spec.user_auth;
  return html`
    <${Field} label="URL" setting="mcp.servers.url" hint="The server's streamable HTTP endpoint."><${Text} ...${p} path="url" required /><//>
    <div class="row">
      <${Field} label="Timeout" setting="mcp.servers.timeout" hint="Default: 1m."><${Text} ...${p} path="timeout" placeholder="1m" /><//>
      <${Field} label="Tool pinning" setting="mcp.servers.pin_tools">
        <${Select} ...${p} path="pin_tools" def="on" options=${{ on: 'On: approve changed tools', strict: 'Strict: approve every new tool', off: 'Off' }} />
      <//>
    </div>
    <${Field} label="Headers" setting="mcp.servers.headers" hint="Sent to the server, such as an API key. Values are encrypted."><${Headers} ...${p} path="headers" secret /><//>
    <label class="check"><input type="checkbox" checked=${userAuth} onChange=${() => onChange(put(spec, 'user_auth', userAuth ? undefined : { client_id: '' }))} /> Call the server with each user's own sign-in (OAuth)</label>
    ${userAuth && html`
      <div class="row">
        <${Field} label="Client id" setting="mcp.servers.user_auth.client_id"><${Text} ...${p} path="user_auth.client_id" /><//>
        <${Field} label="Client secret" setting="mcp.servers.user_auth.client_secret"><${Secret} ...${p} path="user_auth.client_secret" /><//>
      </div>
      <${Field} label="Scopes" setting="mcp.servers.user_auth.scopes" hint="One per line."><${Lines} ...${p} path="user_auth.scopes" /><//>
      <p class="muted small">Endpoints left empty are discovered from the server's metadata.</p>
      <${Field} label="Authorization URL" setting="mcp.servers.user_auth.authorization_url"><${Text} ...${p} path="user_auth.authorization_url" /><//>
      <${Field} label="Token URL" setting="mcp.servers.user_auth.token_url"><${Text} ...${p} path="user_auth.token_url" /><//>
      <${Field} label="Revocation URL" setting="mcp.servers.user_auth.revocation_url"><${Text} ...${p} path="user_auth.revocation_url" /><//>`}`;
}

function VirtualServerForm({ spec, names, onChange }) {
  const { me } = useSession();
  const p = { spec, onChange };
  const servers = spec.servers?.length ? spec.servers : [{ server: '' }];
  const setServer = (i, s) => onChange({ ...spec, servers: servers.map((x, n) => (n === i ? s : x)) });
  const sp = (i) => ({ spec: servers[i], onChange: (s) => setServer(i, s) });
  const prefixMode = (s) => (s.prefix === undefined || s.prefix === null ? 'server' : s.prefix === '' ? 'none' : 'custom');
  return html`
    <${Field} label="Description" setting="mcp.virtual_servers.description"><${Text} ...${p} path="description" /><//>
    <${Field} label="Guardrails" setting="mcp.virtual_servers.guardrails" hint="Run on every tool call through this server."><${Checks} ...${p} path="guardrails" options=${me.guardrails || []} /><//>
    ${servers.map((s, i) => html`<div class="panel" key=${i}>
      <div class="row">
        <${Field} label=${`Upstream server ${i + 1}`} setting="mcp.virtual_servers.servers.server">
          <select required value=${s.server} onChange=${(e) => setServer(i, { ...s, server: e.target.value })}>
            <option value="">Choose…</option>${(names.mcp_servers || []).map((n) => html`<option value=${n}>${n}</option>`)}
          </select>
        <//>
        <${Field} label="Tool name prefix" setting="mcp.virtual_servers.servers.prefix">
          <select value=${prefixMode(s)} onChange=${(e) => setServer(i, { ...s, prefix: e.target.value === 'server' ? undefined : e.target.value === 'none' ? '' : s.prefix || s.server })}>
            <option value="server">Default: the server name</option><option value="none">No prefix</option><option value="custom">Custom</option>
          </select>
          ${prefixMode(s) === 'custom' && html`<input value=${s.prefix} onInput=${(e) => setServer(i, { ...s, prefix: e.target.value })} />`}
        <//>
      </div>
      <${Field} label="Tools" setting="mcp.virtual_servers.servers.tools" hint="One upstream tool name per line. Empty exposes every tool."><${Lines} ...${sp(i)} path="tools" /><//>
      <div class="row">
        <${Field} label="Prompts" setting="mcp.virtual_servers.servers.prompts">
          <select value=${s.prompts === false ? 'false' : 'true'} onChange=${(e) => setServer(i, { ...s, prompts: e.target.value === 'true' ? undefined : false })}>
            <option value="true">Exposed (default)</option><option value="false">Hidden</option>
          </select>
        <//>
        <${Field} label="Resources" setting="mcp.virtual_servers.servers.resources">
          <select value=${s.resources === false ? 'false' : 'true'} onChange=${(e) => setServer(i, { ...s, resources: e.target.value === 'true' ? undefined : false })}>
            <option value="true">Exposed (default)</option><option value="false">Hidden</option>
          </select>
        <//>
      </div>
      ${servers.length > 1 && html`<button type="button" class="secondary small" onClick=${() => onChange({ ...spec, servers: servers.filter((_, n) => n !== i) })}>Remove server</button>`}
    </div>`)}
    <button type="button" class="secondary" onClick=${() => onChange({ ...spec, servers: [...servers, { server: '' }] })}>Add an upstream server</button>`;
}

const alertEvents = ['budget.threshold', 'mcp.tool_pending', 'key.expiring'];

function WebhookForm({ spec, onChange }) {
  const p = { spec, onChange };
  return html`
    <${Field} label="URL" setting="alerts.webhooks.url" hint="https, or http to localhost. Encrypted in the database."><${Secret} ...${p} path="url" /><//>
    <${Field} label="Signing secret" setting="alerts.webhooks.secret" hint="Optional. Signs each delivery."><${Secret} ...${p} path="secret" /><//>
    <div class="row">
      <${Field} label="Format" setting="alerts.webhooks.format"><${Select} ...${p} path="format" def="json" options=${{ json: 'JSON', slack: 'Slack' }} /><//>
      <${Field} label="Organization" setting="alerts.webhooks.org" hint="Name or id. Empty receives alerts about every organization."><${Text} ...${p} path="org" /><//>
    </div>
    <${Field} label="Events" setting="alerts.webhooks.events"><${Checks} ...${p} path="events" options=${alertEvents} /><//>`;
}

const forms = {
  providers: ProviderForm,
  models: ModelForm,
  guardrails: GuardrailForm,
  mcp_servers: MCPServerForm,
  mcp_virtual_servers: VirtualServerForm,
  alert_webhooks: WebhookForm,
};
