import { html, api, useLoad, useSession, Loading, Table, PageHeader, Health, endpointNames, providerTypes, priceText } from '../lib.js';
import { useConfigObjects, DeletedObjects } from './config.js';


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

export function Models({ params }) {
  const state = useLoad(() => api('GET', '/admin/v1/models'));
  const cfg = useConfigObjects('models', { params, onChange: state.reload });
  const s = useSession();
  return html`
    <${PageHeader} title="Models" sub="The model names clients ask for. The strategy picks the first target and the rest are fallbacks; health covers the last hour.">
      ${s.has('playground.use') && html`<a class="button secondary" href="#/playground">Try in the playground</a>`}
      ${cfg.newButton}
    <//>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(m) => m.name}
      rows=${d.data}
      empty=${cfg.owner ? html`No models yet. <a href="#/setup">Add a provider from a preset</a> to get its models, or add one with New model.` : 'No models are configured.'}
      columns=${[
        { label: 'Model', render: (m) => html`<strong>${m.name}</strong> ${cfg.source(m.name)}<div class="chips">${m.endpoints.map((e) => html`<span class="badge" key=${e}>${endpointNames[e] || e}</span>`)}</div><div class="muted small">${strategyNames[m.strategy] || m.strategy}${m.sticky ? ', sticky per user' : ''}${m.retries?.attempts ? ` · ${m.retries.attempts} ${m.retries.attempts === 1 ? 'retry' : 'retries'}` : ''}</div>` },
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
        ...(cfg.owner ? [{ label: html`<span class="sr-only">Actions</span>`, render: (m) => cfg.actions(m.name) }] : []),
      ]}
    />`}<//>
    <${DeletedObjects} cfg=${cfg} />
    ${cfg.dialogs}`;
}
