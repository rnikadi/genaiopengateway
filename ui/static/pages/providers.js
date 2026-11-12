import { html, api, useLoad, Loading, Table, PageHeader, Health, providerTypes } from '../lib.js';
import { useConfigObjects, DeletedObjects } from './config.js';

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

// Providers lists the upstreams with their health over the last hour, and
// lets owners add, change and delete them here (docs/admin-ui.md).
export function Providers({ params }) {
  const state = useLoad(() => api('GET', '/admin/v1/providers'));
  const cfg = useConfigObjects('providers', { params, onChange: state.reload });
  return html`
    <${PageHeader} title="Providers" sub="The upstream services models call. Health covers the last hour across every replica. Credentials are never shown.">
      ${cfg.owner && html`<a class="button secondary" href="#/setup">Add from a preset</a>`}
      ${cfg.newButton}
    <//>
    <${Loading} state=${state}>${(d) => html`<${Table}
      rowKey=${(p) => p.name}
      rows=${d.data}
      empty=${cfg.owner ? html`No providers yet. <a href="#/setup">Add one from a preset</a>: the endpoint, models and prices are filled in for you.` : 'No providers are configured.'}
      columns=${[
        { label: 'Provider', render: (p) => html`<strong>${p.name}</strong> ${cfg.source(p.name)}<div class="muted small">${providerTypes[p.type] || p.type}${p.residency ? html` · <span class="badge" title="Data residency">${p.residency}</span>` : ''}</div>` },
        { label: 'Endpoint', render: (p) => html`<code class="small">${where(p)}</code>` },
        { label: 'Auth', render: (p) => html`${authNames[p.auth] || p.auth}${p.header_names?.length ? html`<div class="muted small">Headers: ${p.header_names.join(', ')}</div>` : ''}` },
        { label: 'Models', render: (p) => (p.models.length ? html`<span class="chips">${p.models.map((m) => html`<a class="badge" href=${`#/models?edit=${encodeURIComponent(m)}`} key=${m}>${m}</a>`)}</span>` : html`<span class="muted small">Not used by any model</span>`) },
        { label: 'Last hour', render: (p) => html`<${Health} h=${p.health} />` },
        ...(cfg.owner ? [{ label: html`<span class="sr-only">Actions</span>`, render: (p) => cfg.actions(p.name) }] : []),
      ]}
    />`}<//>
    <${DeletedObjects} cfg=${cfg} />
    ${cfg.dialogs}`;
}
