import { html, useState, useEffect, api, qs, useSession, PageHeader, ErrorBox, Tabs, usd, num } from '../lib.js';

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

// modes are the playground's tabs, with the endpoint each calls.
const modes = {
  chat: { label: 'Chat', endpoint: 'chat.completions' },
  image: { label: 'Image', endpoint: 'images.generations' },
  speech: { label: 'Text to speech', endpoint: 'audio.speech' },
  transcribe: { label: 'Transcription', endpoint: 'audio.transcriptions' },
};

// call posts to a playground endpoint as the team and returns the
// response with what the gateway reported about it.
async function call(path, team, init) {
  const res = await fetch(`/admin/v1/playground/${path}` + qs({ team_id: team }), { method: 'POST', credentials: 'same-origin', ...init });
  const meta = { target: res.headers.get('X-Gateway-Target'), guardrails: res.headers.get('X-Gog-Guardrails'), request: res.headers.get('X-Request-Id'), status: res.status };
  if (!res.ok) {
    const data = await res.json().catch(() => null);
    throw Object.assign(new Error(data?.error?.message || res.statusText), { meta });
  }
  return { res, meta };
}

const json = (body) => ({ headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });

// targetPrice is the configured price of the target that answered.
function targetPrice(catalog, model, target) {
  return catalog.models.find((m) => m.name === model)?.targets.find((t) => `${t.provider}/${t.model}` === target)?.price;
}

// Playground calls a model as a team, with the team's policies, through
// the gateway's own request path (docs/playground.md).
export function Playground() {
  const s = useSession();
  const writable = Object.keys(s.orgNames).filter((id) => s.canWrite(id));
  const [org, setOrg] = useState(writable[0] || '');
  const [teams, setTeams] = useState([]);
  const [team, setTeam] = useState('');
  const [mode, setMode] = useState('chat');
  // Every model on an openai provider serves every tab, so each tab
  // keeps its own choice.
  const [picked, setPicked] = useState({});
  const setModel = (name) => setPicked((p) => ({ ...p, [mode]: name }));
  const [error, setError] = useState(null);
  const [policy, setPolicy] = useState({ org: null });

  useEffect(() => {
    if (!org) return;
    // Switching organizations quickly must not show the previous one's teams.
    let current = true;
    Promise.all([api('GET', `/admin/v1/orgs/${encodeURIComponent(org)}`), api('GET', `/admin/v1/orgs/${encodeURIComponent(org)}/teams?limit=500`)]).then(
      ([o, t]) => {
        if (!current) return;
        setTeams(t.data);
        setTeam(t.data[0]?.id || '');
        setPolicy({ org: o });
      },
      (err) => current && setError(err),
    );
    return () => {
      current = false;
    };
  }, [org]);
  const teamObj = teams.find((t) => t.id === team);
  const allowed = (m) => [policy.org, teamObj].every((p) => !p?.allowed_models?.length || p.allowed_models.includes(m.name));
  const models = s.catalog.models.filter((m) => m.endpoints?.includes(modes[mode].endpoint) && allowed(m));
  // The tab's choice while the team may use it, otherwise the first model.
  const model = models.some((m) => m.name === picked[mode]) ? picked[mode] : models[0]?.name || '';

  if (!writable.length) {
    return html`<${PageHeader} title="Playground" /><p class="muted">The playground spends a team's budget, so it needs the owner role or the admin role of an organization.</p>`;
  }
  const Panel = { chat: ChatPanel, image: ImagePanel, speech: SpeechPanel, transcribe: TranscribePanel }[mode];
  return html`
    <${PageHeader} title="Playground" sub="Call a model as a team: the team's allowed models, limits, budget and guardrails apply, and the call is metered under the team and audited." />
    <${Tabs} tabs=${Object.fromEntries(Object.entries(modes).map(([k, v]) => [k, v.label]))} value=${mode} onChange=${setMode} />
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
    </div>
    <${ErrorBox} error=${error} />
    ${!teams.length && html`<p class="muted">This organization has no teams yet. Create one to use the playground.</p>`}
    ${teams.length > 0 && !models.length && html`<p class="muted">No model this team may use serves ${modes[mode].label.toLowerCase()}.</p>`}
    <${Panel} key=${mode} team=${team} model=${model} catalog=${s.catalog} />`;
}

// Meta shows what the gateway reported about the last call.
function Meta({ meta, children }) {
  if (!meta) return null;
  return html`<p class="muted small meta">
    ${meta.target && html`Answered by <code>${meta.target}</code> · `}
    ${children}
    ${meta.cost !== null && meta.cost !== undefined && html`${usd(meta.cost)} · `}
    ${meta.guardrails && html`Guardrails: <code>${meta.guardrails}</code> · `}
    Request <code>${meta.request}</code>
  </p>`;
}

function ChatPanel({ team, model, catalog }) {
  const [system, setSystem] = useState('');
  const [input, setInput] = useState('');
  const [stream, setStream] = useState(true);
  const [messages, setMessages] = useState([]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [meta, setMeta] = useState(null);

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
      const init = json(body);
      init.headers.Accept = stream ? 'text/event-stream' : 'application/json';
      const { res, meta: m } = await call('chat/completions', team, init);
      let text = '';
      let usage = null;
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
      setMeta({ ...m, usage, cost: cost(catalog, model, m.target, usage) });
    } catch (err) {
      setMessages(history);
      setError(err);
      if (err.meta) setMeta(err.meta);
    } finally {
      setBusy(false);
    }
  };

  return html`
    <section class="panel">
      <div class="actions">
        <label class="inline check"><input type="checkbox" checked=${stream} onChange=${(e) => setStream(e.target.checked)} /> <span>Stream</span></label>
        ${messages.length > 0 && html`<button class="secondary" onClick=${() => { setMessages([]); setMeta(null); setError(null); }}>New conversation</button>`}
      </div>
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
      <${Meta} meta=${meta}>${meta?.usage && html`${num(meta.usage.prompt_tokens)} in · ${num(meta.usage.completion_tokens)} out · `}<//>
    </section>`;
}

// useRun runs one playground call at a time and keeps its outcome.
function useRun() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState(null);
  const [meta, setMeta] = useState(null);
  const run = async (fn) => {
    setBusy(true);
    setError(null);
    setMeta(null);
    try {
      setMeta(await fn());
    } catch (err) {
      setError(err);
      if (err.meta) setMeta(err.meta);
    } finally {
      setBusy(false);
    }
  };
  return { busy, error, meta, run };
}

const imageSizes = ['auto', '1024x1024', '1536x1024', '1024x1536'];

function ImagePanel({ team, model, catalog }) {
  const [prompt, setPrompt] = useState('');
  const [size, setSize] = useState('auto');
  const [n, setN] = useState(1);
  const [images, setImages] = useState([]);
  const { busy, error, meta, run } = useRun();
  const submit = (e) => {
    e.preventDefault();
    run(async () => {
      setImages([]);
      const { res, meta: m } = await call('images/generations', team, json({ model, prompt, size, n: Number(n) }));
      const data = await res.json();
      const shown = (data.data || []).map((d) => (d.b64_json ? `data:image/${data.output_format || 'png'};base64,${d.b64_json}` : d.url)).filter(Boolean);
      setImages(shown);
      const p = targetPrice(catalog, model, m.target);
      const u = data.usage;
      const tokens = p && u ? ((u.input_tokens || 0) * (p.input_per_mtok || 0) + (u.output_tokens || 0) * (p.output_per_mtok || 0)) / 1e6 : 0;
      return { ...m, images: shown.length, cost: p ? shown.length * (p.per_image || 0) + tokens + (p.per_request || 0) : null };
    });
  };
  return html`
    <section class="panel">
      <form onSubmit=${submit}>
        <label class="field"><span>Prompt</span><textarea rows="3" required value=${prompt} onInput=${(e) => setPrompt(e.target.value)} placeholder="Describe the image"></textarea></label>
        <div class="actions">
          <label class="inline"><span>Size</span><select value=${size} onChange=${(e) => setSize(e.target.value)}>${imageSizes.map((v) => html`<option value=${v}>${v}</option>`)}</select></label>
          <label class="inline"><span>Images</span><select value=${n} onChange=${(e) => setN(e.target.value)}>${[1, 2, 3, 4].map((v) => html`<option value=${v}>${v}</option>`)}</select></label>
          <button type="submit" disabled=${busy || !team || !model}>${busy ? 'Generating…' : 'Generate'}</button>
        </div>
      </form>
      <${ErrorBox} error=${error} />
      <div class="gallery">${images.map((src, i) => html`<img key=${i} src=${src} alt=${`Generated image ${i + 1}`} />`)}</div>
      <${Meta} meta=${meta}>${meta?.images > 0 && html`${num(meta.images)} images · `}<//>
    </section>`;
}

const voices = ['alloy', 'ash', 'ballad', 'coral', 'echo', 'fable', 'nova', 'onyx', 'sage', 'shimmer', 'verse'];

function SpeechPanel({ team, model, catalog }) {
  const [input, setInput] = useState('');
  const [voice, setVoice] = useState('alloy');
  const [audio, setAudio] = useState('');
  const { busy, error, meta, run } = useRun();
  useEffect(() => () => audio && URL.revokeObjectURL(audio), [audio]);
  const submit = (e) => {
    e.preventDefault();
    run(async () => {
      setAudio('');
      const { res, meta: m } = await call('audio/speech', team, json({ model, input, voice, response_format: 'mp3' }));
      setAudio(URL.createObjectURL(await res.blob()));
      const p = targetPrice(catalog, model, m.target);
      const chars = [...input].length;
      return { ...m, chars, cost: p ? (chars * (p.per_mchars || 0)) / 1e6 + (p.per_request || 0) : null };
    });
  };
  return html`
    <section class="panel">
      <form onSubmit=${submit}>
        <label class="field"><span>Text</span><textarea rows="4" required value=${input} onInput=${(e) => setInput(e.target.value)} placeholder="Text to speak"></textarea></label>
        <div class="actions">
          <label class="inline"><span>Voice</span><select value=${voice} onChange=${(e) => setVoice(e.target.value)}>${voices.map((v) => html`<option value=${v}>${v}</option>`)}</select></label>
          <button type="submit" disabled=${busy || !team || !model}>${busy ? 'Speaking…' : 'Speak'}</button>
        </div>
      </form>
      <${ErrorBox} error=${error} />
      ${audio && html`<audio controls autoplay src=${audio}></audio>`}
      <${Meta} meta=${meta}>${meta?.chars > 0 && html`${num(meta.chars)} characters · `}<//>
    </section>`;
}

function TranscribePanel({ team, model, catalog }) {
  const [file, setFile] = useState(null);
  const [text, setText] = useState('');
  const { busy, error, meta, run } = useRun();
  const submit = (e) => {
    e.preventDefault();
    if (!file) return;
    run(async () => {
      setText('');
      const form = new FormData();
      form.append('model', model);
      form.append('file', file, file.name);
      const { res, meta: m } = await call('audio/transcriptions', team, { body: form });
      const data = await res.json();
      setText(data.text ?? '');
      const seconds = data.usage?.type === 'duration' ? data.usage.seconds : data.duration;
      const p = targetPrice(catalog, model, m.target);
      return { ...m, seconds, cost: p && seconds ? (seconds / 60) * (p.per_audio_minute || 0) + (p.per_request || 0) : null };
    });
  };
  return html`
    <section class="panel">
      <form onSubmit=${submit}>
        <label class="field"><span>Audio file</span><input type="file" accept="audio/*,video/mp4,video/webm" required onChange=${(e) => setFile(e.target.files[0] || null)} /></label>
        <button type="submit" disabled=${busy || !team || !model || !file}>${busy ? 'Transcribing…' : 'Transcribe'}</button>
      </form>
      <${ErrorBox} error=${error} />
      ${text && html`<div class="msg assistant"><div class="msg-text">${text}</div></div>`}
      <${Meta} meta=${meta}>${meta?.seconds > 0 && html`${num(Math.round(meta.seconds))} s of audio · `}<//>
    </section>`;
}
