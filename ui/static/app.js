// The admin UI shell: sign-in, navigation and hash routing
// (docs/admin-ui.md).
import { render } from './vendor/htm-preact.js';
import { html, useState, useEffect, api, Session, ErrorBox, APIError } from './lib.js';
import { Overview } from './pages/overview.js';
import { Orgs, Org, Team } from './pages/tenants.js';
import { Keys } from './pages/keys.js';
import { Usage } from './pages/usage.js';
import { MCP } from './pages/mcp.js';
import { Models } from './pages/models.js';
import { Audit } from './pages/audit.js';
import { Requests, RequestBody } from './pages/requests.js';
import { Guardrails } from './pages/guardrails.js';
import { Providers } from './pages/providers.js';
import { Alerts } from './pages/alerts.js';
import { Settings } from './pages/settings.js';
import { Playground } from './pages/playground.js';
import { Config } from './pages/config.js';
import { Jobs } from './pages/jobs.js';
import { Setup } from './pages/setup.js';
import { Directory } from './pages/directory.js';
import { Agents } from './pages/agents.js';
import { Search, useSearchShortcut } from './search.js';

const routes = [
  [/^\/$/, Overview, 'overview'],
  [/^\/orgs$/, Orgs, 'orgs'],
  [/^\/orgs\/([^/]+)$/, Org, 'orgs'],
  [/^\/teams\/([^/]+)$/, Team, 'orgs'],
  [/^\/keys$/, Keys, 'keys'],
  [/^\/usage$/, Usage, 'usage'],
  [/^\/requests$/, Requests, 'requests'],
  [/^\/requests\/([^/]+)$/, RequestBody, 'requests'],
  [/^\/jobs$/, Jobs, 'jobs'],
  [/^\/guardrails$/, Guardrails, 'guardrails'],
  [/^\/mcp$/, MCP, 'mcp'],
  [/^\/models$/, Models, 'models'],
  [/^\/providers$/, Providers, 'providers'],
  [/^\/agents$/, Agents, 'agents'],
  [/^\/alerts$/, Alerts, 'alerts'],
  [/^\/settings(?:\/([a-z-]+))?$/, Settings, 'settings', 'owner'],
  [/^\/playground$/, Playground, 'playground', 'playground.use'],
  [/^\/config$/, Config, 'settings', 'owner'],
  [/^\/audit$/, Audit, 'audit'],
  [/^\/setup$/, Setup, 'overview', 'owner'],
  [/^\/directory$/, Directory, 'directory', 'owner'],
];

// The menu groups pages by the job people come to do (docs/admin-ui.md).
// Each entry is [section, href, label, permission]; a group with nothing
// the operator may open is left out.
const nav = [
  ['', [['overview', '#/', 'Home']]],
  ['Access', [
    ['orgs', '#/orgs', 'Organizations'],
    ['keys', '#/keys', 'Keys'],
    ['directory', '#/directory', 'People', 'owner'],
  ]],
  ['AI services', [
    ['models', '#/models', 'Models'],
    ['providers', '#/providers', 'Providers'],
    ['mcp', '#/mcp', 'MCP'],
    ['agents', '#/agents', 'Agents', 'agents'],
    ['guardrails', '#/guardrails', 'Guardrails'],
  ]],
  ['Monitor', [
    ['usage', '#/usage', 'Usage and cost'],
    ['requests', '#/requests', 'Requests'],
    ['jobs', '#/jobs', 'Jobs'],
    ['alerts', '#/alerts', 'Alerts'],
    ['audit', '#/audit', 'Audit log'],
  ]],
  ['', [['settings', '#/settings', 'Settings', 'owner']]],
];

function parseHash() {
  const raw = location.hash.replace(/^#/, '') || '/';
  const [path, query = ''] = raw.split('?');
  return { path, params: new URLSearchParams(query) };
}

function useRoute() {
  const [route, setRoute] = useState(parseHash());
  useEffect(() => {
    const on = () => setRoute(parseHash());
    window.addEventListener('hashchange', on);
    return () => window.removeEventListener('hashchange', on);
  }, []);
  return route;
}

function SignIn({ onSignedIn, expired }) {
  const [methods, setMethods] = useState(null);
  const [token, setToken] = useState('');
  const [error, setError] = useState(null);
  useEffect(() => {
    fetch('auth/config', { credentials: 'same-origin' })
      .then((r) => (r.ok ? r.json() : Promise.reject(new APIError(r.status, 'error', 'The admin UI is not available on this gateway.'))))
      .then(setMethods, setError);
  }, []);
  const submit = async (e) => {
    e.preventDefault();
    setError(null);
    try {
      await api('POST', 'auth/token', { token });
      onSignedIn();
    } catch (err) {
      setError(err.status === 401 ? new Error('That admin token is not valid.') : err);
    }
  };
  const returnTo = encodeURIComponent('/ui/' + location.hash);
  return html`<main class="signin">
    <div class="signin-box">
      <h1><span class="logo" aria-hidden="true"></span>GenAIOpenGateway</h1>
      ${expired && html`<p class="notice">Your session ended. Sign in again.</p>`}
      <${ErrorBox} error=${error} />
      ${methods?.oidc && html`<a class="button wide" href=${'auth/login?return=' + returnTo}>Sign in with single sign-on</a>`}
      ${methods?.saml && html`<a class="button wide" href=${'auth/saml/login?return=' + returnTo}>Sign in with single sign-on</a>`}
      ${methods?.token &&
      html`<form class="form" onSubmit=${submit}>
        <label class="field"><span>Admin token</span>
          <input type="password" autocomplete="current-password" required value=${token} onInput=${(e) => setToken(e.target.value)} />
        </label>
        <button type="submit" class="wide">Sign in</button>
        <p class="muted small">Configure <code>sso.oidc</code> to sign people in with your identity provider instead.</p>
      </form>`}
      ${methods && !methods.oidc && !methods.saml && !methods.token && html`<p class="muted">No sign-in method is configured. Set <code>admin.token</code>, <code>sso.oidc</code> or <code>sso.saml</code>.</p>`}
    </div>
  </main>`;
}

function Shell({ session, onSignOut }) {
  const route = useRoute();
  const [searching, setSearching] = useState(false);
  const [menu, setMenu] = useState(false);
  useSearchShortcut(() => setSearching(true));
  let Page = null;
  let section = '';
  let args = [];
  let needs;
  for (const [re, component, sec, role] of routes) {
    const m = route.path.match(re);
    if (m) {
      Page = component;
      section = sec;
      needs = role;
      args = m.slice(1).filter((a) => a !== undefined).map(decodeURIComponent);
      break;
    }
  }
  const op = session.me.operator;
  const owner = op.grants.some((g) => g.role === 'owner');
  // owner pages need the owner role; others, a grant with that permission.
  const may = (need) =>
    !need || owner || (need === 'agents' ? (session.me.agents || []).length > 0 : need !== 'owner' && op.grants.some((g) => (g.permissions || []).includes(need)));
  const groups = nav.map(([title, items]) => [title, items.filter(([, , , role]) => may(role))]).filter(([, items]) => items.length);
  const pages = groups.flatMap(([, items]) => items.map(([, href, label]) => [label, href]));
  if (may('playground.use')) pages.push(['Playground', '#/playground']);
  if (owner) pages.push(['Setup wizard', '#/setup']);
  // Move focus to the page's heading after navigating, so screen readers
  // announce the new page and keyboard users start at its top.
  useEffect(() => {
    const h = document.querySelector('main h1');
    if (h && document.activeElement?.tagName !== 'INPUT') {
      h.setAttribute('tabindex', '-1');
      h.focus({ preventScroll: true });
    }
    document.title = (h?.textContent ? h.textContent + ' · ' : '') + 'GenAIOpenGateway';
    setMenu(false);
  }, [route.path]);
  return html`<${Session.Provider} value=${session}>
    <a class="skip" href="#main" onClick=${(e) => { e.preventDefault(); document.getElementById('main')?.focus(); }}>Skip to content</a>
    <div class="layout">
      <aside class="sidebar">
        <div class="brand-row">
          <a class="brand" href="#/"><span class="logo" aria-hidden="true"></span>GenAIOpenGateway</a>
          <button type="button" class="secondary menu-toggle" aria-expanded=${menu} aria-controls="main-nav" onClick=${() => setMenu(!menu)}>Menu</button>
        </div>
        <nav aria-label="Main" id="main-nav" class=${menu ? 'open' : ''}>${groups.map(([title, items]) => html`<div class="nav-group" key=${title || items[0][0]}>
          ${title && html`<span class="nav-title" aria-hidden="true">${title}</span>`}
          <ul role="list" aria-label=${title || undefined}>${items.map(([key, href, label]) => html`<li key=${key}><a href=${href} class=${key === section ? 'active' : ''} aria-current=${key === section ? 'page' : undefined}>${label}</a></li>`)}</ul>
        </div>`)}</nav>
        <div class="who">
          <span class="who-name" title=${op.email || op.subject}>${op.name || op.email || op.subject}</span>
          <span class="muted small">${roleLabel(op.grants, session.orgNames)}</span>
          <button class="link" onClick=${onSignOut}>Sign out</button>
        </div>
      </aside>
      <div class="main-col">
        <header class="topbar">
          <button type="button" class="search-button secondary" onClick=${() => setSearching(true)} aria-keyshortcuts="Control+K">
            <span aria-hidden="true">⌕</span> Search <kbd>Ctrl K</kbd>
          </button>
          ${may('playground.use') && html`<a class="button secondary" href="#/playground">Playground</a>`}
        </header>
        <main class="content" id="main" tabindex="-1">
          ${!Page
            ? html`<h1>Not found</h1><p><a href="#/">Go home</a></p>`
            : !may(needs)
              ? html`<h1>${needs === 'owner' ? 'Owners only' : 'Not available'}</h1><p class="muted">${needs === 'owner' ? 'This page needs the owner role.' : 'Your role does not include this page.'} <a href="#/">Go home</a></p>`
              : html`<${Page} key=${location.hash} args=${args} params=${route.params} />`}
        </main>
      </div>
    </div>
    ${searching && html`<${Search} pages=${pages} onClose=${() => setSearching(false)} />`}
  <//>`;
}

function roleLabel(grants, orgNames) {
  return grants
    .map((g) => {
      const org = orgNames[g.org_id] || g.org_id;
      if (g.team_id) return `${g.role} of ${g.team_name || 'a team'} in ${org}`;
      return g.org_id ? `${g.role} of ${org}` : g.role === 'owner' ? 'owner' : `${g.role} of all organizations`;
    })
    .join(', ');
}

// loadSession fetches who is signed in and the catalog of models and MCP
// servers that policy forms offer.
async function loadSession() {
  const [me, models, vs, orgs] = await Promise.all([
    api('GET', '/admin/v1/me'),
    api('GET', '/admin/v1/models'),
    api('GET', '/admin/v1/mcp/virtual-servers'),
    api('GET', '/admin/v1/orgs?limit=500'),
  ]);
  const orgNames = Object.fromEntries(orgs.data.map((o) => [o.id, o.name]));
  return { me, catalog: { models: models.data, virtualServers: vs.data }, orgNames };
}

function App() {
  const [state, setState] = useState({ loading: true });
  const load = (expired = false) =>
    loadSession().then(
      (session) => setState({ session }),
      (error) => setState(error.status === 401 ? { signedOut: true, expired } : { error }),
    );
  useEffect(() => {
    load();
    const out = () => setState({ signedOut: true, expired: true });
    window.addEventListener('gog:signed-out', out);
    return () => window.removeEventListener('gog:signed-out', out);
  }, []);
  const signOut = async () => {
    try {
      await api('POST', 'auth/logout');
    } finally {
      setState({ signedOut: true });
    }
  };
  if (state.loading) return html`<p class="muted center">Loading…</p>`;
  if (state.signedOut) return html`<${SignIn} expired=${state.expired} onSignedIn=${() => load()} />`;
  if (state.error) return html`<main class="signin"><div class="signin-box"><${ErrorBox} error=${state.error} /></div></main>`;
  return html`<${Shell} session=${{ ...state.session, refresh: () => load() }} onSignOut=${signOut} />`;
}

render(html`<${App} />`, document.getElementById('app'));
