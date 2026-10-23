// Shared helpers and components for the admin UI. Text from the API is
// always rendered as text, never as HTML.
import { html, useState, useEffect, useContext, createContext } from './vendor/htm-preact.js';

export { html, useState, useEffect, useContext };

// --- API ---

export class APIError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

// api calls the admin API with the session cookie. Writes send JSON, which
// the gateway requires of cookie requests as a CSRF defence.
export async function api(method, path, body) {
  const init = { method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
  if (method !== 'GET') {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body ?? {});
  }
  const res = await fetch(path, init);
  if (res.status === 204) return null;
  let data = null;
  try {
    data = await res.json();
  } catch {
    // Not JSON; the status says enough.
  }
  if (!res.ok) {
    if (res.status === 401) window.dispatchEvent(new Event('gog:signed-out'));
    throw new APIError(res.status, data?.error?.code ?? 'error', data?.error?.message ?? res.statusText);
  }
  return data;
}

export const qs = (params) => {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') p.set(k, v);
  const s = p.toString();
  return s ? '?' + s : '';
};

// useLoad runs an async loader when deps change, and again on reload().
export function useLoad(loader, deps = []) {
  const [state, setState] = useState({ loading: true });
  const [tick, setTick] = useState(0);
  useEffect(() => {
    let live = true;
    setState((s) => ({ ...s, loading: true }));
    loader().then(
      (data) => live && setState({ data }),
      (error) => live && setState({ error }),
    );
    return () => {
      live = false;
    };
  }, [...deps, tick]);
  return { ...state, reload: () => setTick((t) => t + 1) };
}

// --- session context ---

export const Session = createContext(null);

// can answers role questions for the signed-in operator; the API enforces
// the same rules, so this only hides what would be refused.
export function useSession() {
  const s = useContext(Session);
  const grants = s.me.operator.grants || [];
  const owner = grants.some((g) => g.role === 'owner');
  const readsAll = owner || grants.some((g) => g.role === 'viewer' && !g.org_id);
  return {
    ...s,
    owner,
    readsAll,
    orgIDs: [...new Set(grants.filter((g) => g.org_id).map((g) => g.org_id))],
    canWrite: (orgID) => owner || grants.some((g) => g.role === 'admin' && g.org_id === orgID),
  };
}

// --- formatting ---

const usdFmt = new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', maximumFractionDigits: 2 });
const smallUsdFmt = new Intl.NumberFormat(undefined, { style: 'currency', currency: 'USD', maximumSignificantDigits: 3 });
const numFmt = new Intl.NumberFormat();
const compactFmt = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 });

export const usd = (n) => (n > 0 && n < 0.01 ? smallUsdFmt : usdFmt).format(n || 0);
export const num = (n) => numFmt.format(n || 0);
export const compact = (n) => compactFmt.format(n || 0);
export const when = (t) => (t ? new Date(t).toLocaleString() : '');
export const day = (t) => (t ? new Date(t).toLocaleDateString() : '');

export function policySummary(p) {
  const parts = [];
  parts.push(p.allowed_models?.length ? `${p.allowed_models.length} model${p.allowed_models.length > 1 ? 's' : ''}` : 'All models');
  if (p.allowed_mcp_servers?.length) parts.push(`${p.allowed_mcp_servers.length} MCP server${p.allowed_mcp_servers.length > 1 ? 's' : ''}`);
  if (p.rpm) parts.push(`${num(p.rpm)} RPM`);
  if (p.tpm) parts.push(`${compact(p.tpm)} TPM`);
  if (p.budget) parts.push(`${usd(p.budget.amount_usd)} ${p.budget.period}`);
  if (p.log_bodies !== undefined && p.log_bodies !== null) parts.push(p.log_bodies ? 'Bodies logged' : 'Bodies not logged');
  return parts.join(' · ');
}

export function downloadCSV(name, header, rows) {
  const cell = (v) => {
    const s = String(v ?? '');
    return /[",\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  };
  const text = [header, ...rows].map((r) => r.map(cell).join(',')).join('\n');
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], { type: 'text/csv' }));
  a.download = name;
  a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}

// --- components ---

export function Loading({ state, children }) {
  if (state.error) return html`<${ErrorBox} error=${state.error} />`;
  if (state.data === undefined) return html`<p class="muted">Loading…</p>`;
  return children(state.data);
}

export function ErrorBox({ error }) {
  if (!error) return null;
  const msg = error.status === 403 ? 'Your role does not allow this.' : error.message;
  return html`<div class="notice error" role="alert">${msg}</div>`;
}

export function Table({ columns, rows, empty = 'Nothing here yet.', rowKey = (r) => r.id }) {
  if (!rows.length) return html`<p class="muted">${empty}</p>`;
  return html`<div class="table-wrap"><table>
    <thead><tr>${columns.map((c) => html`<th class=${c.num ? 'num' : ''}>${c.label}</th>`)}</tr></thead>
    <tbody>${rows.map((r) => html`<tr key=${rowKey(r)}>${columns.map((c) => html`<td class=${c.num ? 'num' : ''}>${c.render(r)}</td>`)}</tr>`)}</tbody>
  </table></div>`;
}

export function Modal({ title, onClose, children }) {
  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [onClose]);
  return html`<div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
    <div class="modal" role="dialog" aria-modal="true" aria-label=${title}>
      <header><h2>${title}</h2><button class="icon" aria-label="Close" onClick=${onClose}>×</button></header>
      ${children}
    </div>
  </div>`;
}

export function Field({ label, hint, children }) {
  return html`<label class="field"><span>${label}</span>${children}${hint && html`<small class="muted">${hint}</small>`}</label>`;
}

export function Card({ label, value, sub, href }) {
  const body = html`<span class="label">${label}</span><span class="value">${value}</span>${sub && html`<span class="muted">${sub}</span>`}`;
  return href ? html`<a class="card" href=${href}>${body}</a>` : html`<div class="card">${body}</div>`;
}

// Bars is a horizontal bar chart of labelled values.
export function Bars({ items, format = num }) {
  if (!items.length) return html`<p class="muted">No data for this period.</p>`;
  const max = Math.max(...items.map((i) => i.value), 0) || 1;
  return html`<div class="bars">${items.map(
    (i) => html`<div class="bar-row" key=${i.label}>
      <span class="bar-label" title=${i.label}>${i.label}</span>
      <span class="bar-track"><span class="bar" style=${{ width: `${(i.value / max) * 100}%` }}></span></span>
      <span class="bar-value">${format(i.value)}</span>
    </div>`,
  )}</div>`;
}

// Columns is a vertical bar chart over time.
export function Columns({ items, format = num }) {
  if (!items.length) return html`<p class="muted">No data for this period.</p>`;
  const max = Math.max(...items.map((i) => i.value), 0) || 1;
  // Label at most about ten columns so the dates stay readable.
  const every = Math.ceil(items.length / 10);
  return html`<div class="columns">${items.map(
    (i, n) => html`<div class="column" key=${i.label} title=${`${i.label}: ${format(i.value)}`}>
      <span class="column-bar" style=${{ height: `${Math.max((i.value / max) * 100, 1)}%` }}></span>
      <span class="column-label">${n % every === 0 ? i.label.slice(5) : '\u00a0'}</span>
    </div>`,
  )}</div>`;
}

// PolicyForm edits the fields organizations, teams and keys share. Empty
// allow lists mean everything; an empty budget means none.
export function PolicyForm({ initial = {}, kind, submitLabel, onSubmit, onCancel }) {
  const { catalog, me } = useSession();
  const [v, setV] = useState({
    name: initial.name ?? '',
    allowed_models: initial.allowed_models ?? [],
    allowed_mcp_servers: initial.allowed_mcp_servers ?? [],
    rpm: initial.rpm || '',
    tpm: initial.tpm || '',
    budget_amount: initial.budget?.amount_usd ?? '',
    budget_period: initial.budget?.period ?? 'monthly',
    alert_at: alertAtText(initial.budget?.alert_at),
    expires_at: initial.expires_at ? initial.expires_at.slice(0, 10) : '',
    log_bodies: initial.log_bodies === undefined || initial.log_bodies === null ? '' : String(initial.log_bodies),
  });
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setV({ ...v, [k]: e.target.value });
  const toggle = (k, name) => () =>
    setV({ ...v, [k]: v[k].includes(name) ? v[k].filter((x) => x !== name) : [...v[k], name] });

  const submit = async (e) => {
    e.preventDefault();
    const body = {
      name: v.name.trim(),
      allowed_models: v.allowed_models,
      allowed_mcp_servers: v.allowed_mcp_servers,
      rpm: Number(v.rpm) || 0,
      tpm: Number(v.tpm) || 0,
      budget: v.budget_amount === '' ? null : { amount_usd: Number(v.budget_amount), period: v.budget_period },
      log_bodies: v.log_bodies === '' ? null : v.log_bodies === 'true',
    };
    if (body.budget && v.alert_at.trim()) {
      const at = parseAlertAt(v.alert_at);
      if (!at) {
        setError(new Error('Alert at takes percentages such as "50, 80, 100", or "off".'));
        return;
      }
      body.budget.alert_at = at;
    }
    if (kind === 'key') body.expires_at = v.expires_at ? new Date(v.expires_at + 'T23:59:59Z').toISOString() : null;
    setBusy(true);
    setError(null);
    try {
      await onSubmit(body);
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };

  const checks = (k, names) =>
    names.length
      ? html`<div class="checks">${names.map(
          (n) => html`<label class="check" key=${n}><input type="checkbox" checked=${v[k].includes(n)} onChange=${toggle(k, n)} /> ${n}</label>`,
        )}</div>`
      : html`<p class="muted">None configured.</p>`;

  return html`<form class="form" onSubmit=${submit}>
    <${ErrorBox} error=${error} />
    <${Field} label="Name"><input required maxlength="200" value=${v.name} onInput=${set('name')} /><//>
    <${Field} label="Allowed models" hint="None checked means every model.">${checks('allowed_models', catalog.models.map((m) => m.name))}<//>
    <${Field} label="Allowed MCP servers" hint="None checked means every MCP server.">${checks('allowed_mcp_servers', catalog.virtualServers.map((s) => s.name))}<//>
    <div class="row">
      <${Field} label="Requests per minute" hint="Empty for no limit."><input type="number" min="0" value=${v.rpm} onInput=${set('rpm')} /><//>
      <${Field} label="Tokens per minute" hint="Empty for no limit."><input type="number" min="0" value=${v.tpm} onInput=${set('tpm')} /><//>
    </div>
    <div class="row">
      <${Field} label="Budget (USD)" hint="Empty for no budget."><input type="number" min="0" step="0.01" value=${v.budget_amount} onInput=${set('budget_amount')} /><//>
      <${Field} label="Budget period">
        <select value=${v.budget_period} onChange=${set('budget_period')}>
          <option value="daily">Daily</option><option value="monthly">Monthly</option><option value="total">Total</option>
        </select>
      <//>
    </div>
    ${v.budget_amount !== '' && html`<${Field} label="Alert at (% of budget)" hint="For example 50, 80, 100. Empty uses the gateway's thresholds; off sends no budget alerts.">
      <input value=${v.alert_at} placeholder="Gateway default" onInput=${set('alert_at')} />
    <//>`}
    ${(me.body_logging || v.log_bodies !== '') && html`<${Field} label="Log request bodies" hint="Stores redacted, encrypted request and response content. The most specific setting wins.">
      <select value=${v.log_bodies} onChange=${set('log_bodies')}>
        <option value="">${kind === 'org' ? 'Off (default)' : 'Inherit'}</option><option value="true">On</option><option value="false">Off</option>
      </select>
    <//>`}
    ${kind === 'key' && html`<${Field} label="Expires" hint="Empty for no expiry."><input type="date" value=${v.expires_at} onInput=${set('expires_at')} /><//>`}
    <footer>
      <button type="button" class="secondary" onClick=${onCancel}>Cancel</button>
      <button type="submit" disabled=${busy}>${busy ? 'Saving…' : submitLabel}</button>
    </footer>
  </form>`;
}

// Budget alert thresholds are shares of the budget in the API and
// percentages in the form.
function alertAtText(a) {
  if (a === undefined || a === null) return '';
  return a.length ? a.map((x) => Math.round(x * 100)).join(', ') : 'off';
}

// parseAlertAt returns shares of the budget, or null when text is not
// "off" or a list of percentages.
function parseAlertAt(text) {
  const t = text.trim().toLowerCase();
  if (t === 'off') return [];
  const vals = t.split(/[\s,]+/).map(Number);
  return vals.every((x) => x > 0 && x <= 100) ? vals.map((x) => x / 100) : null;
}

// Confirm asks before something that cannot be undone.
export function Confirm({ title, message, action, danger = true, onConfirm, onClose }) {
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const go = async () => {
    setBusy(true);
    try {
      await onConfirm();
      onClose();
    } catch (err) {
      setError(err);
      setBusy(false);
    }
  };
  return html`<${Modal} title=${title} onClose=${onClose}>
    <${ErrorBox} error=${error} />
    <p>${message}</p>
    <footer>
      <button class="secondary" onClick=${onClose}>Cancel</button>
      <button class=${danger ? 'danger' : ''} disabled=${busy} onClick=${go}>${action}</button>
    </footer>
  <//>`;
}

export function PageHeader({ title, sub, children }) {
  return html`<div class="page-header"><div><h1>${title}</h1>${sub && html`<p class="muted">${sub}</p>`}</div><div class="actions">${children}</div></div>`;
}

// OrgPicker chooses which organization a page reports on. Operators who
// read every organization may also choose all of them.
export function OrgPicker({ value, onChange }) {
  const { readsAll, orgIDs, orgNames } = useSession();
  const ids = readsAll ? Object.keys(orgNames) : orgIDs;
  return html`<label class="inline"><span>Organization</span>
    <select value=${value} onChange=${(e) => onChange(e.target.value)}>
      ${readsAll && html`<option value="">All organizations</option>`}
      ${ids.map((id) => html`<option value=${id} key=${id}>${orgNames[id] || id}</option>`)}
    </select>
  </label>`;
}

export function defaultOrg(s) {
  return s.readsAll ? '' : s.orgIDs[0] || '';
}

// Periods offered by usage reports, as [from, to) in UTC.
export const periods = {
  month: ['This month', () => {
    const n = new Date();
    return [new Date(Date.UTC(n.getUTCFullYear(), n.getUTCMonth(), 1)), null];
  }],
  last_month: ['Last month', () => {
    const n = new Date();
    return [new Date(Date.UTC(n.getUTCFullYear(), n.getUTCMonth() - 1, 1)), new Date(Date.UTC(n.getUTCFullYear(), n.getUTCMonth(), 1))];
  }],
  '7d': ['Last 7 days', () => [new Date(Date.now() - 7 * 864e5), null]],
  '30d': ['Last 30 days', () => [new Date(Date.now() - 30 * 864e5), null]],
  '90d': ['Last 90 days', () => [new Date(Date.now() - 90 * 864e5), null]],
};

export function usageQuery({ org, team, key, group_by, period = 'month' }) {
  const [from, to] = periods[period][1]();
  return '/admin/v1/usage' + qs({ org_id: org, team_id: team, key_id: key, group_by, from: from.toISOString(), to: to?.toISOString() });
}

export function totals(rows) {
  const t = { requests: 0, input_tokens: 0, output_tokens: 0, cached_input_tokens: 0, reasoning_tokens: 0, cost_usd: 0 };
  for (const r of rows) for (const k of Object.keys(t)) t[k] += r[k] || 0;
  return t;
}

// CopyButton copies text to the clipboard.
export function CopyButton({ text, label = 'Copy' }) {
  const [done, setDone] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setDone(true);
      setTimeout(() => setDone(false), 1500);
    } catch {
      // Clipboard access denied; the text is still selectable.
    }
  };
  return html`<button type="button" class="secondary" onClick=${copy}>${done ? 'Copied' : label}</button>`;
}

export function JSONView({ value }) {
  let text = value;
  try {
    text = JSON.stringify(typeof value === 'string' ? JSON.parse(value) : value, null, 2);
  } catch {
    // Show it as it is.
  }
  return html`<pre class="json">${text}</pre>`;
}
