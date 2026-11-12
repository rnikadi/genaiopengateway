// "Why was this denied?" (docs/why-denied.md): every check a request
// goes through for a key and a model or MCP tool, and which one says no.
import { html, useState, useEffect, api, qs, useLoad, useSession, PageHeader, Field, ErrorBox, Loading, when, endpointNames, OrgPicker, defaultOrg } from '../lib.js';

const marks = { pass: ['✓', 'good', 'Passes'], fail: ['✗', 'bad', 'Says no'], warn: ['!', 'warn', 'Close to a limit'], info: ['i', '', 'For information'] };
const actions = ['chat.completions', 'responses', 'messages', 'embeddings', 'images.generations', 'audio.speech', 'audio.transcriptions'];

export function Explain({ params }) {
  const s = useSession();
  const requestID = params.get('request_id') || '';
  const [form, setForm] = useState({
    key_id: params.get('key') || '', kind: params.get('server') ? 'mcp' : 'model',
    model: params.get('model') || '', action: 'chat.completions', server: params.get('server') || '', tool: params.get('tool') || '', user: '',
  });
  const [result, setResult] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const set = (k) => (e) => setForm((f) => ({ ...f, [k]: e.target.value }));

  const run = async (body) => {
    setBusy(true);
    setError(null);
    try {
      setResult(await api('POST', '/admin/v1/explain', body));
    } catch (err) {
      setResult(null);
      setError(err);
    }
    setBusy(false);
  };
  useEffect(() => {
    if (requestID) run({ request_id: requestID });
  }, [requestID]);
  useEffect(() => {
    if (!requestID && form.key_id && (form.model || form.server)) submit();
  }, []);

  const submit = (e) => {
    e?.preventDefault();
    const body = { key_id: form.key_id, user: form.user || undefined };
    if (form.kind === 'model') Object.assign(body, { model: form.model, action: form.action });
    else Object.assign(body, { server: form.server, tool: form.tool || undefined });
    run(body);
  };

  return html`<${PageHeader} title="Why was this denied?" sub="Check whether a key may use a model or an MCP tool now, and which policy, limit, Cedar rule or guardrail says no." />
    ${requestID ? html`<p><a href="#/explain">Check another key or model</a></p>`
    : html`<section class="panel"><form class="form" onSubmit=${submit}>
      <${KeyPicker} value=${form.key_id} onChange=${(v) => setForm((f) => ({ ...f, key_id: v }))} portal=${!s.has('keys.read')} />
      <fieldset class="choice"><legend>Check</legend>
        <label class="check"><input type="radio" name="kind" value="model" checked=${form.kind === 'model'} onChange=${set('kind')} /> A model</label>
        <label class="check"><input type="radio" name="kind" value="mcp" checked=${form.kind === 'mcp'} onChange=${set('kind')} /> An MCP server or tool</label>
      </fieldset>
      ${form.kind === 'model' ? html`<div class="grid2">
        <${ModelPicker} value=${form.model} onChange=${(v) => setForm((f) => ({ ...f, model: v }))} />
        <${Field} label="Endpoint"><select value=${form.action} onChange=${set('action')}>${actions.map((a) => html`<option key=${a} value=${a}>${endpointNames[a] || a}</option>`)}</select><//>
      </div>` : html`<div class="grid2">
        <${ServerPicker} value=${form.server} onChange=${(v) => setForm((f) => ({ ...f, server: v }))} />
        <${Field} label="Tool" hint="As the client sees it, prefix included. Empty checks the server only."><input value=${form.tool} onInput=${set('tool')} /><//>
      </div>`}
      <${Field} label="User" hint="Optional: the user the request is for, if your Cedar policies look at it."><input value=${form.user} onInput=${set('user')} /><//>
      <footer><button type="submit" disabled=${busy || !form.key_id || !(form.kind === 'model' ? form.model : form.server)}>Check</button></footer>
    </form></section>`}
    <${ErrorBox} error=${error} />
    ${busy && html`<p class="muted">Checking…</p>`}
    ${result && !busy && html`<${Result} r=${result} />`}`;
}

function Result({ r }) {
  return html`<section class="panel" aria-live="polite">
    <p class=${'verdict ' + (r.allowed ? 'good' : 'bad')}><strong>${r.allowed ? 'Allowed now.' : 'Denied.'}</strong> ${r.summary}</p>
    ${r.denial && html`<p class="muted small">Request <code>${r.denial.request_id}</code> was refused ${when(r.denial.ts)} with <code>${r.denial.code}</code>: ${r.denial.reason}. The checks below show how things stand now, which may have changed since.</p>`}
    <p class="muted small">Key <strong>${r.key.name}</strong> (<code>${r.key.prefix}…</code>)${r.key.team_name && html` in ${r.key.team_name}, ${r.key.org_name}`}${r.key.owner && html`, owned by ${r.key.owner}`}.</p>
    <ol class="checks-list">${r.checks.map((c) => {
      const [mark, cls, label] = marks[c.status] || marks.info;
      return html`<li key=${c.id} class=${'check-row ' + c.status}>
        <span class=${'badge ' + cls} aria-label=${label} title=${label}>${mark}</span>
        <div><strong>${c.label}</strong><div>${c.detail.charAt(0).toUpperCase() + c.detail.slice(1)}</div>
          ${c.fix && html`<div class="muted small">${c.fix}</div>`}
          ${c.link && (c.status === 'fail' || c.status === 'warn') && html`<a class="small" href=${c.link}>Go to the setting</a>`}
        </div>
      </li>`;
    })}</ol>
  </section>`;
}

// KeyPicker lists keys of an organization, or for developers their own.
function KeyPicker({ value, onChange, portal }) {
  const s = useSession();
  const [org, setOrg] = useState(defaultOrg(s));
  const state = useLoad(() => (portal ? api('GET', '/admin/v1/portal').then((d) => ({ data: d.teams.flatMap((t) => t.keys.map((k) => ({ ...k, team: t.name }))) }))
    : api('GET', '/admin/v1/keys' + qs({ org_id: org, limit: 500 }))), [org, portal]);
  return html`${!portal && html`<${OrgPicker} value=${org} onChange=${setOrg} />`}
    <${Loading} state=${state}>${(d) => html`<${Field} label="Key"><select required value=${value} onChange=${(e) => onChange(e.target.value)}>
      <option value="">Choose a key</option>
      ${value && !d.data.some((k) => k.id === value) && html`<option value=${value}>${value}</option>`}
      ${d.data.map((k) => html`<option key=${k.id} value=${k.id}>${k.name} (${k.prefix}…)${k.revoked_at ? ' revoked' : ''}</option>`)}
    </select><//>`}<//>`;
}

function ModelPicker({ value, onChange }) {
  const state = useLoad(() => api('GET', '/admin/v1/models'), []);
  return html`<${Field} label="Model" hint="Or type a name that is not configured."><input list="explain-models" required value=${value} onInput=${(e) => onChange(e.target.value)} />
    <datalist id="explain-models">${(state.data?.data || []).map((m) => html`<option key=${m.name} value=${m.name} />`)}</datalist><//>`;
}

function ServerPicker({ value, onChange }) {
  const state = useLoad(() => api('GET', '/admin/v1/mcp/virtual-servers'), []);
  return html`<${Field} label="MCP server"><input list="explain-servers" required value=${value} onInput=${(e) => onChange(e.target.value)} />
    <datalist id="explain-servers">${(state.data?.data || []).map((m) => html`<option key=${m.name} value=${m.name} />`)}</datalist><//>`;
}
