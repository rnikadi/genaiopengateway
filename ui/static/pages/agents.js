import { html, PageHeader, Table, useSession } from '../lib.js';
import { ConfigKind } from './config.js';

// Agents lists the A2A agents served through the gateway (docs/a2a.md).
// Owners add and change them here; others see their names and endpoints.
export function Agents({ params }) {
  const s = useSession();
  return html`
    <${PageHeader} title="Agents" sub="A2A agents that clients reach through the gateway at /a2a/{name}, with the same keys, policies and guardrails as models." />
    <section class="panel">${s.owner
      ? html`<${ConfigKind} kind="a2a_agents" params=${params} empty="No agents yet. Add one with its A2A endpoint." />`
      : html`<${Table} rowKey=${(a) => a} rows=${s.me.agents || []} empty="No agents are configured." columns=${[
          { label: 'Agent', render: (a) => html`<code>${a}</code>` },
          { label: 'Card', render: (a) => html`<code class="small">/a2a/${a}/.well-known/agent-card.json</code>` },
        ]} />`}</section>`;
}
