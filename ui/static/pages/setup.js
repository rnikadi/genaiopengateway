import { html, useState, useEffect, api, useSession, useLoad, Loading, PageHeader, ErrorBox, Field, CopyButton, Tabs, providerTypes, priceText } from '../lib.js';

// The setup wizard (docs/quickstart.md): pick a provider from the
// catalog, paste a key, choose models, create a first organization, team
// and key, check a completion goes through, and copy a snippet. Each step
// writes the same objects the Configuration and Organizations pages do.

const slug = (s) => s.toLowerCase().replace(/[^a-z0-9._-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 63) || 'model';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// waitForModel polls until every replica's refresh has picked up a stored
// model, which takes up to store.refresh_interval.
async function waitForModel(name, tries = 20) {
  for (let i = 0; i < tries; i++) {
    const models = await api('GET', '/admin/v1/models');
    if (models.data.some((m) => m.name === name)) return;
    await sleep(500);
  }
  throw new Error(`The model ${name} did not appear within 10 seconds. Check the Configuration page for errors.`);
}

export function Setup() {
  const s = useSession();
  const state = useLoad(() => Promise.all([api('GET', '/admin/v1/catalog'), api('GET', '/admin/v1/providers')]));
  const [step, setStep] = useState(1);
  const [preset, setPreset] = useState(null);
  const [done, setDone] = useState({});
  if (!s.owner) return html`<h1>Owners only</h1><p class="muted">Setting up providers needs the owner role.</p>`;
  return html`
    <${PageHeader} title="Set up the gateway" sub="Add a provider, create a first key and send a test request. Everything here can be changed later on the Configuration and Organizations pages." />
    <ol class="steps">
      ${['Provider', 'Connect', 'Team and key', 'Test', 'Use it'].map((label, i) => html`<li key=${label} class=${step === i + 1 ? 'active' : step > i + 1 ? 'done' : ''}>${label}</li>`)}
    </ol>
    <${Loading} state=${state}>${([catalog, providers]) => {
      switch (step) {
        case 1:
          return html`<${PickProvider} catalog=${catalog} existing=${providers.data} onPick=${(p) => (setPreset(p), setStep(2))} />`;
        case 2:
          return html`<${ConfigureProvider} preset=${preset} existing=${providers.data} models=${s.catalog.models} onBack=${() => setStep(1)} onDone=${(d) => (setDone({ ...done, ...d }), setStep(3))} />`;
        case 3:
          return html`<${CreateKey} onDone=${(d) => (setDone({ ...done, ...d }), setStep(4))} />`;
        case 4:
          return html`<${TestCall} done=${done} onDone=${(d) => (setDone({ ...done, ...d }), setStep(5))} />`;
        default:
          return html`<${Snippets} done=${done} onFinish=${() => (s.refresh(), (location.hash = '#/'))} />`;
      }
    }}<//>`;
}

function PickProvider({ catalog, existing, onPick }) {
  return html`<section class="panel">
    <h2>Which provider do you want to use first?</h2>
    <p class="muted">Presets fill in the endpoint, the models and their list prices from catalog ${catalog.version}. You can add more providers afterwards.</p>
    ${existing.length > 0 && html`<p class="notice">Already configured: ${existing.map((p) => p.name).join(', ')}. A new provider is added beside them.</p>`}
    <div class="preset-grid">
      ${catalog.providers.map(
        (p) => html`<button type="button" class="preset" key=${p.id} onClick=${() => onPick(p)}>
          <strong>${p.name}</strong>
          <span class="muted small">${providerTypes[p.type] || p.type}${p.models.length ? ` · ${p.models.length} models` : ''}</span>
        </button>`,
      )}
    </div>
  </section>`;
}

function ConfigureProvider({ preset, existing, models, onBack, onDone }) {
  const taken = new Set(existing.map((p) => p.name));
  const [name, setName] = useState(taken.has(preset.id) ? `${preset.id}-2` : preset.id);
  const [baseURL, setBaseURL] = useState(preset.base_url || '');
  const [key, setKey] = useState('');
  const [region, setRegion] = useState(preset.region || '');
  const [project, setProject] = useState('');
  const [location, setLocation] = useState(preset.location || '');
  const firstChat = preset.models.find((m) => m.kind === 'chat');
  const [picked, setPicked] = useState(new Set(firstChat ? [firstChat.id] : []));
  const [custom, setCustom] = useState('');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const modelNames = new Set(models.map((m) => m.name));
  const toggle = (id) => setPicked((p) => (p.has(id) ? (p.delete(id), new Set(p)) : new Set(p.add(id))));

  const submit = async (e) => {
    e.preventDefault();
    setError(null);
    const chosen = preset.models.filter((m) => picked.has(m.id));
    for (const id of custom.split(',').map((x) => x.trim()).filter(Boolean)) chosen.push({ id, kind: 'chat', price: {} });
    if (!chosen.length) return setError(new Error('Choose at least one model.'));
    const spec = { type: preset.type };
    if (baseURL) spec.base_url = baseURL;
    if (key) spec.api_key = key;
    if (preset.type === 'bedrock') spec.region = region;
    if (preset.type === 'vertex') Object.assign(spec, { project, location });
    setBusy(true);
    try {
      await api('PUT', `/admin/v1/config/providers/${encodeURIComponent(name)}`, spec);
      const created = [];
      for (const m of chosen) {
        let vname = slug(m.id);
        if (modelNames.has(vname)) vname = slug(`${m.id}-${name}`);
        const target = { provider: name, model: m.id };
        if (m.price && Object.keys(m.price).length) target.price = m.price;
        await api('PUT', `/admin/v1/config/models/${encodeURIComponent(vname)}`, { targets: [target] });
        created.push({ name: vname, kind: m.kind });
      }
      onDone({ provider: name, models: created });
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };

  return html`<section class="panel">
    <h2>${preset.name}</h2>
    ${preset.note && html`<p class="notice">${preset.note}</p>`}
    <${ErrorBox} error=${error} />
    <form class="form" onSubmit=${submit}>
      <div class="row">
        <${Field} label="Provider name" hint="How the gateway refers to this upstream."><input required pattern="[a-z0-9][a-z0-9._-]{0,62}" value=${name} onInput=${(e) => setName(e.target.value)} /><//>
        ${preset.type !== 'vertex' && preset.type !== 'bedrock' && html`<${Field} label="Base URL"><input required type="url" value=${baseURL} onInput=${(e) => setBaseURL(e.target.value)} /><//>`}
        ${preset.type === 'bedrock' && html`<${Field} label="AWS region"><input required value=${region} onInput=${(e) => setRegion(e.target.value)} /><//>`}
        ${preset.type === 'vertex' && html`<${Field} label="Google Cloud project"><input required value=${project} onInput=${(e) => setProject(e.target.value)} /><//>`}
        ${preset.type === 'vertex' && html`<${Field} label="Location"><input required value=${location} onInput=${(e) => setLocation(e.target.value)} /><//>`}
      </div>
      ${!preset.no_key &&
      html`<${Field} label=${preset.key_optional ? 'API key (optional)' : 'API key'} hint=${html`Stored encrypted and never shown again.${preset.key_url ? html` <a href=${preset.key_url} target="_blank" rel="noopener noreferrer">Get a key</a>` : ''}`}>
        <input type="password" autocomplete="off" required=${!preset.key_optional} value=${key} onInput=${(e) => setKey(e.target.value)} />
      <//>`}
      ${preset.models.length > 0 &&
      html`<fieldset class="field"><legend>Models to offer</legend>
        <div class="model-picks">
          ${preset.models.map(
            (m) => html`<label class="check" key=${m.id}><input type="checkbox" checked=${picked.has(m.id)} onChange=${() => toggle(m.id)} />
              <code>${m.id}</code> <span class="muted small">${m.kind}${m.price && Object.keys(m.price).length ? ` · ${priceText(m.price)}` : ''}</span></label>`,
          )}
        </div>
        ${preset.pricing_url && html`<span class="muted small">List prices from the catalog; check <a href=${preset.pricing_url} target="_blank" rel="noopener noreferrer">${preset.name}'s pricing</a> for current ones.</span>`}
      </fieldset>`}
      ${(preset.custom_models || !preset.models.length) &&
      html`<${Field} label="Other model ids" hint="Comma separated, such as deployment names or locally pulled models. They are added as chat models without a price."><input value=${custom} onInput=${(e) => setCustom(e.target.value)} /><//>`}
      <div class="inline">
        <button type="button" class="secondary" onClick=${onBack}>Back</button>
        <button type="submit" disabled=${busy}>${busy ? 'Saving…' : 'Add provider and models'}</button>
      </div>
    </form>
  </section>`;
}

function CreateKey({ onDone }) {
  const [org, setOrg] = useState('My organization');
  const [team, setTeam] = useState('Default team');
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const submit = async (e) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const orgs = await api('GET', '/admin/v1/orgs?limit=500');
      const o = orgs.data.find((x) => x.name.toLowerCase() === org.trim().toLowerCase()) || (await api('POST', '/admin/v1/orgs', { name: org }));
      const t = await api('POST', `/admin/v1/orgs/${encodeURIComponent(o.id)}/teams`, { name: team });
      const k = await api('POST', `/admin/v1/teams/${encodeURIComponent(t.id)}/keys`, { name: 'first-key' });
      onDone({ org: o, team: t, key: k.key });
    } catch (err) {
      setError(err);
    } finally {
      setBusy(false);
    }
  };
  return html`<section class="panel">
    <h2>Create a team and a key</h2>
    <p class="muted">Applications call the gateway with a virtual key that belongs to a team. Limits, budgets, allowed models and guardrails can be set on the organization, the team and the key later.</p>
    <${ErrorBox} error=${error} />
    <form class="form" onSubmit=${submit}>
      <div class="row">
        <${Field} label="Organization"><input required maxlength="200" value=${org} onInput=${(e) => setOrg(e.target.value)} /><//>
        <${Field} label="Team"><input required maxlength="200" value=${team} onInput=${(e) => setTeam(e.target.value)} /><//>
      </div>
      <div class="inline"><button type="submit" disabled=${busy}>${busy ? 'Creating…' : 'Create team and key'}</button></div>
    </form>
  </section>`;
}

function TestCall({ done, onDone }) {
  const chat = done.models?.find((m) => m.kind === 'chat');
  const [state, setState] = useState({ running: !!chat });
  useEffect(() => {
    if (!chat) return;
    let live = true;
    (async () => {
      try {
        await waitForModel(chat.name);
        const res = await fetch(`/admin/v1/playground/chat/completions?team_id=${encodeURIComponent(done.team.id)}`, {
          method: 'POST',
          credentials: 'same-origin',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ model: chat.name, messages: [{ role: 'user', content: 'Reply with one short sentence: the gateway works.' }], max_tokens: 40 }),
        });
        const data = await res.json().catch(() => null);
        if (!res.ok) throw new Error(data?.error?.message || res.statusText);
        if (live) setState({ reply: data.choices?.[0]?.message?.content ?? '', target: res.headers.get('X-Gateway-Target') });
      } catch (error) {
        if (live) setState({ error });
      }
    })();
    return () => {
      live = false;
    };
  }, []);
  return html`<section class="panel">
    <h2>Test a request</h2>
    ${!chat && html`<p class="muted">No chat model was added, so there is nothing to test here. Try the Playground for other model types.</p>`}
    ${state.running && html`<p class="muted">Calling <code>${chat.name}</code> as ${done.team.name}…</p>`}
    ${state.reply !== undefined && html`<p class="notice">It works. <code>${chat.name}</code> answered through <code>${state.target}</code>:</p><blockquote>${state.reply}</blockquote>`}
    ${state.error &&
    html`<${ErrorBox} error=${state.error} /><p class="muted">Check the key and base URL on the <a href="#/config?kind=providers">Configuration page</a>, then come back, or continue and fix it later.</p>`}
    <div class="inline"><button type="button" disabled=${state.running} onClick=${() => onDone({})}>Continue</button></div>
  </section>`;
}

function Snippets({ done, onFinish }) {
  const base = `${location.origin}/v1`;
  const model = done.models?.find((m) => m.kind === 'chat')?.name || done.models?.[0]?.name || 'MODEL';
  const key = done.key || 'YOUR_GATEWAY_KEY';
  const snippets = {
    curl: `curl ${base}/chat/completions \\
  -H "Authorization: Bearer ${key}" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"${model}","messages":[{"role":"user","content":"Hello"}]}'`,
    Python: `from openai import OpenAI

client = OpenAI(base_url="${base}", api_key="${key}")
reply = client.chat.completions.create(
    model="${model}",
    messages=[{"role": "user", "content": "Hello"}],
)
print(reply.choices[0].message.content)`,
    Node: `import OpenAI from "openai";

const client = new OpenAI({ baseURL: "${base}", apiKey: "${key}" });
const reply = await client.chat.completions.create({
  model: "${model}",
  messages: [{ role: "user", content: "Hello" }],
});
console.log(reply.choices[0].message.content);`,
    'MCP clients': `{
  "mcpServers": {
    "gateway": {
      "url": "${location.origin}/mcp/VIRTUAL_SERVER",
      "headers": { "Authorization": "Bearer ${key}" }
    }
  }
}`,
  };
  const [tab, setTab] = useState('curl');
  return html`<section class="panel">
    <h2>Call the gateway</h2>
    ${done.key && html`<p class="notice">This is the only time the key is shown. Copy it now; you can rotate or revoke it on the Virtual keys page.</p>
      <div class="secret"><code>${done.key}</code><${CopyButton} text=${done.key} /></div>`}
    <p class="muted">Any OpenAI SDK works with the gateway's base URL and the key. The Anthropic SDK takes <code>${location.origin}</code> as its base URL.</p>
    <${Tabs} tabs=${Object.fromEntries(Object.keys(snippets).map((k) => [k, k]))} value=${tab} onChange=${setTab} />
    ${tab === 'MCP clients' && html`<p class="muted small">Add MCP servers and a virtual server on the Configuration page first, then replace VIRTUAL_SERVER with its name.</p>`}
    <pre class="snippet">${snippets[tab]}</pre>
    <div class="inline"><${CopyButton} text=${snippets[tab]} label="Copy snippet" /><button type="button" onClick=${onFinish}>Go to the overview</button></div>
  </section>`;
}
