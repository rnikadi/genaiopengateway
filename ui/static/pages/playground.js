import { html, useState, useEffect, api, qs, useSession, PageHeader, ErrorBox, usd, num } from '../lib.js';

// sse yields the JSON payloads of a server-sent event stream.
async function* sse(body) {
  const reader = body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buf += value;
    let i;
    while ((i = buf.search(/\r?\n\r?\n/)) >= 0) {
      const event = buf.slice(0, i);
      buf = buf.slice(i).replace(/^\r?\n\r?\n/, '');
      const data = event.split(/\r?\n/).filter((l) => l.startsWith('data:')).map((l) => l.slice(5).trim()).join('\n');
      if (data && data !== '[DONE]') yield JSON.parse(data);
    }
  }
}

// cost prices usage at the configured price of the target that answered.
function cost(catalog, model, target, u) {
  const t = catalog.models.find((m) => m.name === model)?.targets.find((t) => `${t.provider}/${t.model}` === target);
  if (!t?.price || !u) return null;
  const cached = u.prompt_tokens_details?.cached_tokens || 0;
  const p = t.price;
  return ((u.prompt_tokens - cached) * p.input_per_mtok + cached * (p.cached_input_per_mtok || p.input_per_mtok) + u.completion_tokens * p.output_per_mtok) / 1e6;
}

// Playground calls a model as a team, with the team's policies, through
// the gateway's own request path (docs/playground.md).
export function Playground() {
  const s = useSession();
  const writable = Object.keys(s.orgNames).filter((id) => s.canWrite(id));
  const [org, setOrg] = useState(writable[0] || '');
  const [teams, setTeams] = useState([]);
  const [team, setTeam] = useState('');
  const [model, setModel] = useState('');
  const [system, setSystem] = useState('');
  const [input, setInput] = useState('');
  const [stream, setStream] = useState(true);
  const [messages, setMessages] = useState([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [meta, setMeta] = useState(null);
  const [policy, setPolicy] = useState({ org: null, team: null });

  useEffect(() => {
    if (!org) return;
    // Switching organizations quickly must not show the previous one's teams.
    let current = true;
    Promise.all([api('GET', `/admin/v1/orgs/${encodeURIComponent(org)}`), api('GET', `/admin/v1/orgs/${encodeURIComponent(org)}/teams?limit=500`)]).then(
      ([o, t]) => {
        if (!current) return;
        setTeams(t.data);
        setTeam(t.data[0]?.id || '');
        setPolicy((p) => ({ ...p, org: o }));
      },
      (err) => current && setError(err),
    );
    return () => {
      current = false;
    };
  }, [org]);
  const teamObj = teams.find((t) => t.id === team);
  const allowed = (m) => [policy.org, teamObj].every((p) => !p?.allowed_models?.length || p.allowed_models.includes(m.name));
  const models = s.catalog.models.filter((m) => m.endpoints?.includes('chat.completions') && allowed(m));
  useEffect(() => {
    if (!models.some((m) => m.name === model)) setModel(models[0]?.name || '');
  }, [team, policy.org, models.length]);

  const send = async (e) => {
    e.preventDefault();
    if (!input.trim() || busy) return;
    const history = [...messages, { role: 'user', content: input }];
    setMessages([...history, { role: 'assistant', content: '' }]);
    setInput('');
    setBusy(true);
    setError(null);
    setMeta(null);
    const body = { model, messages: [...(system ? [{ role: 'system', content: system }] : []), ...history], stream };
    if (stream) body.stream_options = { include_usage: true };
    const reply = (content) => setMessages([...history, { role: 'assistant', content }]);
    try {
      const res = await fetch('/admin/v1/playground/chat/completions' + qs({ team_id: team }), {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', Accept: stream ? 'text/event-stream' : 'application/json' },
        body: JSON.stringify(body),
      });
      const m = { target: res.headers.get('X-Gateway-Target'), guardrails: res.headers.get('X-Gog-Guardrails'), request: res.headers.get('X-Request-Id'), status: res.status };
      let text = '';
      let usage = null;
      if (!res.ok) {
        const data = await res.json().catch(() => null);
        throw Object.assign(new Error(data?.error?.message || res.statusText), { meta: m });
      }
      if (stream && res.headers.get('Content-Type')?.includes('text/event-stream')) {
        for await (const chunk of sse(res.body)) {
          text += chunk.choices?.[0]?.delta?.content || '';
          if (chunk.usage) usage = chunk.usage;
          reply(text);
        }
      } else {
        const data = await res.json();
        text = data.choices?.[0]?.message?.content ?? '';
        usage = data.usage;
        reply(text);
      }
      setMeta({ ...m, usage, cost: cost(s.catalog, model, m.target, usage) });
    } catch (err) {
      setMessages(history);
      setError(err);
      if (err.meta) setMeta(err.meta);
    } finally {
      setBusy(false);
    }
  };

  if (!writable.length) {
    return html`<${PageHeader} title="Playground" /><p class="muted">The playground spends a team's budget, so it needs the owner role or the admin role of an organization.</p>`;
  }
  return html`
    <${PageHeader} title="Playground" sub="Call a model as a team: the team's allowed models, limits, budget and guardrails apply, and the call is metered under the team and audited." />
    <div class="filters">
      <label class="inline"><span>Organization</span>
        <select value=${org} onChange=${(e) => setOrg(e.target.value)}>${writable.map((id) => html`<option value=${id}>${s.orgNames[id]}</option>`)}</select>
      </label>
      <label class="inline"><span>Team</span>
        <select value=${team} onChange=${(e) => setTeam(e.target.value)}>${teams.map((t) => html`<option value=${t.id}>${t.name}</option>`)}</select>
      </label>
      <label class="inline"><span>Model</span>
        <select value=${model} onChange=${(e) => setModel(e.target.value)}>${models.map((m) => html`<option value=${m.name}>${m.name}</option>`)}</select>
      </label>
      <label class="inline check"><input type="checkbox" checked=${stream} onChange=${(e) => setStream(e.target.checked)} /> <span>Stream</span></label>
      ${messages.length > 0 && html`<button class="secondary" onClick=${() => { setMessages([]); setMeta(null); setError(null); }}>New conversation</button>`}
    </div>
    ${!teams.length && html`<p class="muted">This organization has no teams yet. Create one to use the playground.</p>`}
    <section class="panel">
      <label class="field"><span>System prompt</span><textarea rows="2" value=${system} onInput=${(e) => setSystem(e.target.value)} placeholder="Optional"></textarea></label>
      <div class="conversation">
        ${messages.map((m, i) => html`<div key=${i} class=${'msg ' + m.role}><span class="muted small">${m.role === 'user' ? 'You' : model}</span><div class="msg-text">${m.content || (busy ? '…' : '')}</div></div>`)}
      </div>
      <${ErrorBox} error=${error} />
      <form class="compose" onSubmit=${send}>
        <textarea rows="3" required value=${input} onInput=${(e) => setInput(e.target.value)} placeholder="Message"
          onKeyDown=${(e) => e.key === 'Enter' && (e.metaKey || e.ctrlKey) && send(e)}></textarea>
        <button type="submit" disabled=${busy || !team || !model}>${busy ? 'Sending…' : 'Send'}</button>
      </form>
      ${meta && html`<p class="muted small meta">
        ${meta.target && html`Answered by <code>${meta.target}</code> · `}
        ${meta.usage && html`${num(meta.usage.prompt_tokens)} in · ${num(meta.usage.completion_tokens)} out · `}
        ${meta.cost !== null && meta.cost !== undefined && html`${usd(meta.cost)} · `}
        ${meta.guardrails && html`Guardrails: <code>${meta.guardrails}</code> · `}
        Request <code>${meta.request}</code>
      </p>`}
    </section>`;
}
