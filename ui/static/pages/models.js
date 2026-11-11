import { html, api, useSession, useLoad, Loading, Table, PageHeader, Health, endpointNames, providerTypes, priceText } from '../lib.js';


// silent marks a target that served nothing while a later one did: it is
// failing over, since usage records only the target that answered.
const strategyNames = { ordered: 'Ordered', weighted: 'Weighted', round_robin: 'Round robin', least_latency: 'Least latency', lowest_cost: 'Lowest cost' };

// Circuit shows a target whose breaker has taken it out of rotation.
function Circuit({ c }) {
  if (!c || c.state === 'closed') return '';
  const until = c.open_until ? new Date(c.open_until).toLocaleTimeString() : '';
  return c.state === 'open'
    ? html`<span class="badge bad" title=${`${c.failures} failures in a row; probed again at ${until}`}>Circuit open</span>`
    : html`<span class="badge warn" title="The next request probes this target">Probing</span>`;
}

const silent = (targets, i) => !targets[i].health?.requests && targets.slice(i + 1).some((t) => t.health?.requests);

export function Models() {
  const s = useSession();
  const state = useLoad(() => api('GET', '/admin/v1/models'));
  return html`
    <${PageHeader} title="Models" sub="Virtual models from the gateway's configuration. The strategy chooses the first target and the rest are fallbacks; health covers the last hour. Endpoints are those the targets' provider types support: an embedding model on an OpenAI-compatible provider still lists chat.">
      ${s.owner && html`<a class="button secondary" href="#/config?kind=models">Manage in Configuration</a>`}
    <//>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(m) => m.name}
      rows=${d.data}
      empty=${html`No models are configured. Owners add them with the <a href="#/setup">setup wizard</a> or on the <a href="#/config?kind=models">Configuration page</a>.`}
      columns=${[
        { label: 'Model', render: (m) => html`<strong>${m.name}</strong> <span class="muted small">${strategyNames[m.strategy] || m.strategy}${m.sticky ? ', sticky per user' : ''}${m.retries?.attempts ? ` · ${m.retries.attempts} ${m.retries.attempts === 1 ? 'retry' : 'retries'}` : ''}</span><div class="chips">${m.endpoints.map((e) => html`<span class="badge" key=${e}>${endpointNames[e] || e}</span>`)}</div>` },
        {
          label: 'Targets',
          render: (m) => html`<ol class="targets">${m.targets.map(
            (t, i) => html`<li key=${i}>
              <code>${t.provider}/${t.model}</code> <span class="muted small">${providerTypes[t.provider_type] || t.provider_type} · ${priceText(t.price)}</span>
              ${m.strategy === 'weighted' && html`<span class="muted small"> · weight ${t.weight || 1}</span>`}
              <div><${Circuit} c=${t.circuit} /> ${silent(m.targets, i) ? html`<span class="badge bad" title="A later target answered instead: this one is failing over">Not answering</span>` : html`<${Health} h=${t.health} />`}</div>
              ${t.endpoints.length === 0 && html`<div class="badge warn">Serves no endpoint for this model</div>`}
            </li>`,
          )}</ol>`,
        },
        ...(s.owner ? [{ label: '', render: (m) => html`<a class="button secondary" href=${`#/config?kind=models&edit=${encodeURIComponent(m.name)}`}>Edit</a>` }] : []),
      ]}
    />`}<//>`;
}
