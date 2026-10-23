import { html, api, useLoad, Loading, Table, PageHeader, KV, yesNo, num, policySummary } from '../lib.js';

const roleTarget = (r) => (r.group ? `Group ${r.group}` : `Email ${r.email}`);

// Settings shows the gateway's own configuration, for owners
// (docs/admin-ui-config.md). Secrets appear only as set or not set.
export function Settings() {
  const state = useLoad(() => api('GET', '/admin/v1/settings'));
  return html`
    <${PageHeader} title="Settings" sub="How this gateway is configured. Change these in the configuration file; secrets are shown only as set or not set." />
    <${Loading} state=${state}>${(d) => html`
      <section class="panel"><h2>Gateway</h2><${KV} items=${[
        ['Version', d.version],
        ['TLS on the gateway', yesNo(d.tls)],
        ['Metrics listener', d.metrics.listen || 'Main listener'],
        ['Admin token', d.admin_token ? 'Set' : 'Not set'],
        ['Request size limit', `${num(Math.round(d.limits.max_request_bytes / 1048576))} MiB`],
      ]} /></section>
      <div class="grid2">
        <section class="panel"><h2>Store</h2><${KV} items=${[
          ['Driver', d.store.driver],
          ['Snapshot refresh', d.store.refresh_interval],
          ['Usage retention', d.store.usage_retention || 'Forever'],
        ]} /></section>
        <section class="panel"><h2>Rate limits and budgets</h2><${KV} items=${[
          ['Limiter', d.limiter.driver === 'valkey' ? 'Valkey (shared by replicas)' : 'In memory (this replica only)'],
          ['If the limiter is down: rate limits', d.limiter.fail_open_rate_limits ? 'Allow requests' : 'Refuse requests'],
          ['If the limiter is down: budgets', d.limiter.fail_open_budgets ? 'Allow requests' : 'Refuse requests'],
        ]} /></section>
      </div>
      <div class="grid2">
        <section class="panel"><h2>Body logging</h2><${KV} items=${[
          ['Enabled', yesNo(d.body_logging.enabled)],
          ['Retention', d.body_logging.retention],
          ['Largest body kept', d.body_logging.max_body_bytes ? `${num(Math.round(d.body_logging.max_body_bytes / 1024))} KiB` : ''],
          ['Redaction detectors', d.body_logging.detectors?.length ? d.body_logging.detectors.join(', ') : 'All built-in'],
          ['Custom patterns', num(d.body_logging.custom_patterns)],
        ]} /></section>
        <section class="panel"><h2>Encryption</h2><${KV} items=${[
          ['Encryption key', d.secrets.encryption_key ? 'Set' : 'Not set'],
          ['Previous keys still accepted', num(d.secrets.previous_keys)],
        ]} /><p class="muted small">Encrypts logged bodies and users' MCP credentials. Rotate by moving the key to <code>secrets.previous_keys</code>.</p></section>
      </div>
      <section class="panel"><h2>Sign-in</h2>
        ${d.sso.oidc
          ? html`<${KV} items=${[
              ['Identity provider', d.sso.oidc.issuer],
              ['Client id', d.sso.oidc.client_id],
              ['Groups claim', d.sso.oidc.groups_claim],
              ['Session length', d.sso.session_ttl],
            ]} />
            <h3>Roles</h3>
            <${Table} rows=${d.sso.roles || []} rowKey=${(r) => roleTarget(r) + r.role + r.org} empty="No role mappings: nobody can sign in with single sign-on." columns=${[
              { label: 'Who', render: roleTarget },
              { label: 'Role', render: (r) => r.role },
              { label: 'Organization', render: (r) => r.org || 'All' },
            ]} />`
          : html`<p class="muted">Single sign-on is off; operators sign in with the admin token. Configure <code>sso.oidc</code> to sign people in with your identity provider.</p>`}
      </section>
      <section class="panel"><h2>MCP</h2><${KV} items=${[
        ['Public URL', d.mcp.public_url || 'Not set'],
        ['Catalog refresh', d.mcp.refresh_interval],
        ['OAuth issuer for MCP clients', d.mcp.oauth?.issuer || 'Not configured: clients use gateway keys'],
        ['OAuth audience', d.mcp.oauth?.audience],
        ['Team claim', d.mcp.oauth?.team_claim],
      ]} /></section>
      <section class="panel"><h2>Keys from the configuration file</h2>
        <p class="muted small">Declared in <code>keys</code>, read-only, outside any organization. The secrets are never shown.</p>
        <${Table} rows=${d.static_keys} rowKey=${(k) => k.name} empty="None." columns=${[
          { label: 'Name', render: (k) => html`<strong>${k.name}</strong>` },
          { label: 'Policy', render: (k) => policySummary(k) || 'No limits' },
        ]} />
      </section>`}<//>`;
}
