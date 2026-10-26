import { html, useState, api, useLoad, useSession, Loading, Table, PageHeader, KV, ErrorBox, Confirm, num, policySummary } from '../lib.js';

// Settings edits the gateway's own settings, for owners
// (docs/admin-ui-settings.md). Each field shows its value, its default and
// where the value comes from; a value saved here wins over the
// configuration file. Every path below is also checked by
// ui/coverage_test.go, which fails when a gateway setting has no control.
const sections = [
  {
    title: 'Request limits',
    fields: [
      { path: 'limits.max_request_bytes', label: 'Largest request body', type: 'bytes', hint: 'Also the largest realtime message.' },
      { path: 'limits.max_batch_file_bytes', label: 'Largest batch input file', type: 'bytes', hint: 'Read into memory to check each line.' },
      { path: 'limits.max_realtime_session', label: 'Longest realtime session', type: 'duration' },
    ],
  },
  {
    title: 'If the limiter is unreachable',
    fields: [
      { path: 'limiter.fail_open_rate_limits', label: 'Rate limits', type: 'bool', on: 'Allow requests', off: 'Refuse requests' },
      { path: 'limiter.fail_open_budgets', label: 'Budgets', type: 'bool', on: 'Allow requests', off: 'Refuse requests' },
    ],
  },
  {
    title: 'Data and refresh',
    fields: [
      { path: 'store.usage_retention', label: 'Keep usage, MCP calls and guardrail events for', type: 'duration', zero: 'Forever' },
      { path: 'store.refresh_interval', label: 'Replicas reload keys and configuration every', type: 'duration' },
    ],
  },
  {
    title: 'Body logging',
    note: 'Policies choose which requests are logged; this switch turns logging off everywhere.',
    fields: [
      { path: 'body_logging.enabled', label: 'Body logging', type: 'bool', on: 'Allowed', off: 'Off everywhere' },
      { path: 'body_logging.retention', label: 'Keep bodies for', type: 'duration' },
      { path: 'body_logging.max_body_bytes', label: 'Largest body kept', type: 'bytes', hint: 'Longer bodies are cut and marked truncated.' },
      { path: 'body_logging.redact.builtin', label: 'Redaction detectors', type: 'detectors', hint: 'None checked means all of them.' },
      { path: 'body_logging.redact.patterns', label: 'Custom redaction patterns', type: 'list', hint: 'One RE2 expression per line.' },
    ],
  },
  {
    title: 'Response cache',
    note: 'Organizations, teams and keys turn the cache on in their policy.',
    fields: [
      { path: 'cache.max_entry_bytes', label: 'Largest answer kept', type: 'bytes' },
      { path: 'cache.max_memory_bytes', label: 'Memory per replica', type: 'bytes', hint: 'Used when the limiter is not Valkey.' },
      { path: 'cache.semantic.model', label: 'Semantic matching model', type: 'model', hint: 'An embeddings model. Needed for the semantic mode.' },
      { path: 'cache.semantic.threshold', label: 'Similarity a reworded question needs', type: 'number', step: '0.01', min: '0', max: '1' },
      { path: 'cache.semantic.max_entries', label: 'Questions remembered per replica', type: 'int' },
    ],
  },
  {
    title: 'Budget alerts',
    fields: [{ path: 'alerts.budget_thresholds', label: 'Alert at (% of budget)', type: 'percents', hint: 'For example 50, 80, 100. A budget can set its own.' }],
  },
  {
    title: 'Single sign-on',
    note: 'Changes apply at the next sign-in. The admin token always signs in, so a mistake here can be fixed.',
    fields: [
      { path: 'sso.oidc.issuer', label: 'Identity provider (issuer URL)', type: 'text' },
      { path: 'sso.oidc.client_id', label: 'Client id', type: 'text' },
      { path: 'sso.oidc.client_secret', label: 'Client secret', type: 'secret' },
      { path: 'sso.oidc.redirect_url', label: 'Redirect URL', type: 'text', hint: "The gateway's URL ending in /ui/auth/callback." },
      { path: 'sso.oidc.groups_claim', label: 'Groups claim', type: 'text' },
      { path: 'sso.oidc.scopes', label: 'Scopes', type: 'list', hint: 'One per line.' },
      { path: 'sso.session_ttl', label: 'Session length', type: 'duration' },
      { path: 'sso.roles', label: 'Roles', type: 'roles' },
    ],
  },
  {
    title: 'MCP',
    fields: [
      { path: 'mcp.public_url', label: 'Public URL of the gateway', type: 'text', hint: 'Names each virtual server for OAuth clients.' },
      { path: 'mcp.refresh_interval', label: 'Refresh tool catalogs every', type: 'duration' },
      { path: 'mcp.max_user_sessions', label: 'Upstream sessions per replica for per-user servers', type: 'int' },
      { path: 'mcp.oauth.issuer', label: 'OAuth issuer for MCP clients', type: 'text', hint: 'Empty: clients use gateway keys.' },
      { path: 'mcp.oauth.jwks_url', label: 'JWKS URL', type: 'text' },
      { path: 'mcp.oauth.audience', label: 'Audience', type: 'text' },
      { path: 'mcp.oauth.organization', label: 'Organization tokens belong to', type: 'text' },
      { path: 'mcp.oauth.team_claim', label: 'Team claim', type: 'text' },
      { path: 'mcp.oauth.scopes', label: 'Required scopes', type: 'list', hint: 'One per line.' },
    ],
  },
];

const sourceBadge = {
  default: html`<span class="badge">Default</span>`,
  file: html`<span class="badge warn" title="From the configuration file">Config file</span>`,
  stored: html`<span class="badge good" title="Saved in the admin UI; wins over the configuration file">Set here</span>`,
};

const units = [[1 << 30, 'GiB'], [1 << 20, 'MiB'], [1 << 10, 'KiB']];
const bytesText = (n) => {
  for (const [size, unit] of units) if (n >= size && n % (size / 16) === 0) return `${+(n / size).toFixed(2)} ${unit}`;
  return `${num(n)} bytes`;
};

// show formats a value for reading.
function show(f, v) {
  if (v === null || v === undefined || (Array.isArray(v) && !v.length)) {
    return f.type === 'detectors' ? 'All' : f.type === 'roles' ? 'None' : f.zero || 'Not set';
  }
  switch (f.type) {
    case 'bytes': return bytesText(v);
    case 'bool': return v ? f.on : f.off;
    case 'secret': return v ? 'Set' : 'Not set';
    case 'percents': return v.map((x) => `${Math.round(x * 100)}%`).join(', ');
    case 'duration': return v || f.zero || 'Not set';
    case 'roles': return `${v.length} mapping${v.length === 1 ? '' : 's'}`;
    case 'list': case 'detectors': return v.join(', ');
  }
  return String(v);
}

// input is the form value of a setting, and parse its inverse. parse
// returns undefined to leave the setting as it is.
function input(f, v) {
  switch (f.type) {
    case 'bool': return String(!!v);
    case 'list': return (v || []).join('\n');
    case 'detectors': return v || [];
    case 'percents': return v ? (v.length ? v.map((x) => Math.round(x * 100)).join(', ') : 'off') : '';
    case 'roles': return v || [];
    case 'secret': return '';
  }
  return v ?? '';
}

function parse(f, raw) {
  switch (f.type) {
    case 'bool': return raw === 'true';
    case 'bytes': case 'int': case 'number': return raw === '' ? undefined : Number(raw);
    case 'duration': return raw.trim() === '' ? (f.zero ? '0s' : undefined) : raw.trim();
    case 'list': return raw.split('\n').map((s) => s.trim()).filter(Boolean);
    case 'detectors': return raw.length ? raw : null;
    case 'percents': {
      if (raw.trim().toLowerCase() === 'off') return [];
      if (raw.trim() === '') return undefined;
      const xs = raw.split(',').map((s) => Number(s.trim()) / 100);
      if (xs.some((x) => !(x > 0 && x <= 1))) throw new Error(`${f.label}: use percentages such as "50, 80, 100", or "off".`);
      return xs;
    }
    case 'roles': return raw.map((r) => Object.fromEntries(Object.entries(r).filter(([, v]) => v)));
    case 'secret': return raw === '' ? undefined : raw;
  }
  return raw.trim() === '' ? undefined : raw.trim();
}

const clone = (v) => JSON.parse(JSON.stringify(v ?? {}));

function setPath(obj, path, value) {
  const keys = path.split('.');
  let o = obj;
  for (const k of keys.slice(0, -1)) o = o[k] && typeof o[k] === 'object' ? o[k] : (o[k] = {});
  o[keys.at(-1)] = value;
}

// deletePath removes a setting and the sections it leaves empty.
function deletePath(obj, path) {
  const keys = path.split('.');
  const walk = (o, i) => {
    if (!o || typeof o !== 'object') return;
    if (i === keys.length - 1) delete o[keys[i]];
    else {
      walk(o[keys[i]], i + 1);
      if (o[keys[i]] && typeof o[keys[i]] === 'object' && !Array.isArray(o[keys[i]]) && !Object.keys(o[keys[i]]).length) delete o[keys[i]];
    }
  };
  walk(obj, 0);
}

const save = (spec) => api('PUT', '/admin/v1/config/settings/gateway', spec);

export function Settings() {
  const state = useLoad(() => api('GET', '/admin/v1/settings'));
  const [signOut, setSignOut] = useState(false);
  const [signedOut, setSignedOut] = useState(null);
  return html`
    <${PageHeader} title="Settings" sub="How this gateway runs. A value saved here applies to every replica within the refresh interval and wins over the configuration file; Reset goes back to the file's value or the default.">
      <button class="secondary" onClick=${() => setSignOut(true)}>Sign out everyone else</button>
    <//>
    ${signedOut !== null && html`<p class="notice">Signed out ${num(signedOut)} other ${signedOut === 1 ? 'session' : 'sessions'}.</p>`}
    ${signOut && html`<${Confirm} title="Sign out everyone else" action="Sign out everyone"
      message="Every other admin UI session ends now, on every replica; people sign in again. Use it when someone leaves or a session may be stolen. Your session stays."
      onClose=${() => setSignOut(false)}
      onConfirm=${async () => { const r = await api('DELETE', '/admin/v1/sessions'); setSignedOut(r.deleted); setSignOut(false); }} />`}
    <${Loading} state=${state}>${(d) => {
      const fields = Object.fromEntries(d.fields.map((f) => [f.path, f]));
      return html`
        ${sections.map((s) => html`<${Section} key=${s.title} section=${s} fields=${fields} stored=${d.stored} detectors=${d.detectors} onSaved=${state.reload} />`)}
        <${Startup} d=${d} />`;
    }}<//>`;
}

function Section({ section, fields, stored, detectors, onSaved }) {
  const [draft, setDraft] = useState({});
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const changed = Object.keys(draft).length > 0;
  const write = async (change) => {
    setBusy(true);
    setError(null);
    try {
      const spec = clone(stored);
      change(spec);
      await save(spec);
      setDraft({});
      onSaved();
    } catch (err) {
      setError(err);
    }
    setBusy(false);
  };
  const submit = (e) => {
    e.preventDefault();
    write((spec) => {
      for (const f of section.fields) {
        if (!(f.path in draft)) continue;
        const v = parse(f, draft[f.path]);
        if (v !== undefined) setPath(spec, f.path, v);
      }
    });
  };
  return html`<section class="panel">
    <h2>${section.title}</h2>
    ${section.note && html`<p class="muted small">${section.note}</p>`}
    <form class="form" onSubmit=${submit}>
      <${ErrorBox} error=${error} />
      ${section.fields.map((f) => html`<${Setting} key=${f.path} f=${f} info=${fields[f.path] || {}} detectors=${detectors}
        value=${f.path in draft ? draft[f.path] : input(f, fields[f.path]?.value)}
        onChange=${(v) => setDraft({ ...draft, [f.path]: v })}
        onReset=${() => write((spec) => deletePath(spec, f.path))} />`)}
      <footer>
        ${changed && html`<button type="button" class="secondary" onClick=${() => setDraft({})}>Discard</button>`}
        <button type="submit" disabled=${busy || !changed}>${busy ? 'Saving…' : 'Save'}</button>
      </footer>
    </form>
  </section>`;
}

function Setting({ f, info, value, detectors, onChange, onReset }) {
  const { catalog } = useSession();
  const set = (e) => onChange(e.target.value);
  let control;
  switch (f.type) {
    case 'bool':
      control = html`<select value=${value} onChange=${set}><option value="true">${f.on}</option><option value="false">${f.off}</option></select>`;
      break;
    case 'bytes': case 'int':
      control = html`<input type="number" min="1" step="1" value=${value} onInput=${set} />`;
      break;
    case 'number':
      control = html`<input type="number" min=${f.min} max=${f.max} step=${f.step} value=${value} onInput=${set} />`;
      break;
    case 'list':
      control = html`<textarea rows="3" value=${value} onInput=${set}></textarea>`;
      break;
    case 'secret':
      control = html`<input type="password" autocomplete="off" value=${value} placeholder=${info.value ? '•••••••• (stored; type to replace)' : ''} onInput=${set} />`;
      break;
    case 'model':
      control = html`<select value=${value} onChange=${set}>
        <option value="">None</option>
        ${catalog.models.map((m) => html`<option key=${m.name} value=${m.name}>${m.name}</option>`)}
      </select>`;
      break;
    case 'detectors':
      control = html`<div class="checks">${detectors.map((n) => html`<label class="check" key=${n}>
        <input type="checkbox" checked=${value.includes(n)} onChange=${() => onChange(value.includes(n) ? value.filter((x) => x !== n) : [...value, n])} /> ${n}
      </label>`)}</div>`;
      break;
    case 'roles':
      control = html`<${RolesEditor} roles=${value} onChange=${onChange} />`;
      break;
    default:
      control = html`<input value=${value} placeholder=${f.type === 'duration' ? (f.zero ? `Empty: ${f.zero.toLowerCase()}` : 'For example 30s, 5m, 24h or 7d') : ''} onInput=${set} />`;
  }
  return html`<div class="field setting" data-setting=${f.path}>
    <span>${f.label} ${sourceBadge[info.source] || ''}</span>
    ${control}
    <small class="muted">
      Default: ${show(f, info.default)}${f.hint ? ` · ${f.hint}` : ''}
      ${info.source === 'stored' && html` · <button type="button" class="link" onClick=${onReset}>Reset</button>`}
    </small>
  </div>`;
}

function RolesEditor({ roles, onChange }) {
  const set = (i, k, v) => onChange(roles.map((r, n) => (n === i ? { ...r, [k]: v } : r)));
  return html`<div>
    ${roles.length > 0 && html`<div class="table-wrap"><table>
      <thead><tr><th>Group</th><th>or email</th><th>Role</th><th>Organization</th><th></th></tr></thead>
      <tbody>${roles.map((r, i) => html`<tr key=${i}>
        <td data-setting="sso.roles.group"><input value=${r.group || ''} onInput=${(e) => set(i, 'group', e.target.value)} /></td>
        <td data-setting="sso.roles.email"><input value=${r.email || ''} onInput=${(e) => set(i, 'email', e.target.value)} /></td>
        <td data-setting="sso.roles.role"><select value=${r.role || 'viewer'} onChange=${(e) => set(i, 'role', e.target.value)}>
          <option value="owner">Owner</option><option value="admin">Admin</option><option value="viewer">Viewer</option>
        </select></td>
        <td data-setting="sso.roles.org"><input value=${r.org || ''} placeholder="All" onInput=${(e) => set(i, 'org', e.target.value)} /></td>
        <td><button type="button" class="link danger" onClick=${() => onChange(roles.filter((_, n) => n !== i))}>Remove</button></td>
      </tr>`)}</tbody>
    </table></div>`}
    <button type="button" class="secondary" onClick=${() => onChange([...roles, { group: '', role: 'viewer' }])}>Add a role mapping</button>
  </div>`;
}

// Startup shows what the process reads when it starts, which stays in the
// environment and configuration file.
function Startup({ d }) {
  return html`
    <section class="panel"><h2>Startup settings</h2>
      <p class="muted small">Read when the gateway starts, so they stay in the configuration file and environment: ${d.startup.map((p, i) => html`${i ? ', ' : ''}<code>${p}</code>`)}.</p>
      <${KV} items=${[
        ['Version', d.version],
        ['TLS on the gateway', d.tls ? 'Yes' : 'No'],
        ['Metrics listener', d.metrics.listen || 'Main listener'],
        ['Admin token', d.admin_token ? 'Set' : 'Not set'],
        ['Store', d.store.driver],
        ['Limiter', d.limiter.driver === 'valkey' ? 'Valkey (shared by replicas)' : 'In memory (this replica only)'],
        ['Response cache store', d.cache.store === 'valkey' ? 'Valkey (shared by replicas)' : 'In memory, per replica'],
        ['Encryption key', d.secrets.encryption_key ? 'Set' : 'Not set'],
        ['Previous keys still accepted', num(d.secrets.previous_keys)],
        ['Identity provider', d.sso.oidc?.issuer || 'None'],
      ]} />
    </section>
    <section class="panel"><h2>Keys from the configuration file</h2>
      <p class="muted small">Declared in <code>keys</code>, read-only, outside any organization. The secrets are never shown.</p>
      <${Table} rows=${d.static_keys} rowKey=${(k) => k.name} empty="None." columns=${[
        { label: 'Name', render: (k) => html`<strong>${k.name}</strong>` },
        { label: 'Policy', render: (k) => policySummary(k) || 'No limits' },
      ]} />
    </section>`;
}
