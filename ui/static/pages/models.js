import { html, api, useLoad, Loading, Table, PageHeader, Health, endpointNames, providerTypes } from '../lib.js';

const price = (p) => (p && (p.input_per_mtok || p.output_per_mtok) ? `$${p.input_per_mtok} in · $${p.output_per_mtok} out${p.cached_input_per_mtok ? ` · $${p.cached_input_per_mtok} cached` : ''}` : 'No price');

// silent marks a target that served nothing while a later one did: it is
// failing over, since usage records only the target that answered.
const silent = (targets, i) => !targets[i].health?.requests && targets.slice(i + 1).some((t) => t.health?.requests);

export function Models() {
  const state = useLoad(() => api('GET', '/admin/v1/models'));
  return html`
    <${PageHeader} title="Models" sub="Virtual models from the gateway's configuration. Targets are tried in order; prices are per million tokens; health covers the last hour." />
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(m) => m.name}
      rows=${d.data}
      empty="No models are configured."
      columns=${[
        { label: 'Model', render: (m) => html`<strong>${m.name}</strong><div class="chips">${m.endpoints.map((e) => html`<span class="badge" key=${e}>${endpointNames[e] || e}</span>`)}</div>` },
        {
          label: 'Targets',
          render: (m) => html`<ol class="targets">${m.targets.map(
            (t, i) => html`<li key=${i}>
              <code>${t.provider}/${t.model}</code> <span class="muted small">${providerTypes[t.provider_type] || t.provider_type} · ${price(t.price)}</span>
              <div>${silent(m.targets, i) ? html`<span class="badge bad" title="A later target answered instead: this one is failing over">Not answering</span>` : html`<${Health} h=${t.health} />`}</div>
              ${t.endpoints.length === 0 && html`<div class="badge warn">Serves no endpoint for this model</div>`}
            </li>`,
          )}</ol>`,
        },
      ]}
    />`}<//>`;
}
