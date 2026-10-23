import { html, api, useLoad, Loading, Table, PageHeader } from '../lib.js';

const price = (p) => (p && (p.input_per_mtok || p.output_per_mtok) ? `$${p.input_per_mtok} in · $${p.output_per_mtok} out${p.cached_input_per_mtok ? ` · $${p.cached_input_per_mtok} cached` : ''}` : 'No price');

export function Models() {
  const state = useLoad(() => api('GET', '/admin/v1/models'));
  return html`
    <${PageHeader} title="Models" sub="Virtual models from the gateway's configuration. Targets are tried in order; prices are per million tokens." />
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(m) => m.name}
      rows=${d.data}
      empty="No models are configured."
      columns=${[
        { label: 'Model', render: (m) => html`<strong>${m.name}</strong>` },
        { label: 'Targets', render: (m) => html`<ol class="targets">${m.targets.map((t, i) => html`<li key=${i}><code>${t.provider}/${t.model}</code> <span class="muted small">${price(t.price)}</span></li>`)}</ol>` },
      ]}
    />`}<//>`;
}
