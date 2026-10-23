import { html, api, useLoad, Loading, Table, PageHeader, Health, providerTypes } from '../lib.js';

const authNames = {
  api_key: 'API key',
  none: 'None',
  application_default: 'Application Default Credentials',
  credentials_file: 'Credentials file',
  aws_default: 'AWS default chain',
};

// where says which upstream endpoint a provider calls.
const where = (p) =>
  p.type === 'vertex'
    ? `${p.project} · ${p.location}`
    : p.type === 'bedrock'
      ? p.region + (p.base_url ? ` · ${p.base_url}` : '')
      : p.base_url;

// Providers lists the upstreams from the gateway's configuration with
// their health over the last hour (docs/admin-ui-config.md).
export function Providers() {
  const state = useLoad(() => api('GET', '/admin/v1/providers'));
  return html`
    <${PageHeader} title="Providers" sub="Upstreams from the gateway's configuration. Health covers the last hour across every replica. Credentials are never shown." />
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(p) => p.name}
      rows=${d.data}
      empty="No providers are configured."
      columns=${[
        { label: 'Provider', render: (p) => html`<strong>${p.name}</strong><div class="muted small">${providerTypes[p.type] || p.type}</div>` },
        { label: 'Endpoint', render: (p) => html`<code class="small">${where(p)}</code>` },
        { label: 'Auth', render: (p) => html`${authNames[p.auth] || p.auth}${p.header_names?.length ? html`<div class="muted small">Headers: ${p.header_names.join(', ')}</div>` : ''}` },
        { label: 'Models', render: (p) => (p.models.length ? html`<span class="chips">${p.models.map((m) => html`<a class="badge" href="#/models" key=${m}>${m}</a>`)}</span>` : html`<span class="muted small">Not used by any model</span>`) },
        { label: 'Last hour', render: (p) => html`<${Health} h=${p.health} />` },
      ]}
    />`}<//>`;
}
