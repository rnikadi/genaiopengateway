// Search finds a page, organization, key, model, MCP server or request
// and goes to it (docs/admin-ui.md). Ctrl K or / opens it from anywhere.
import { html, useState, useEffect, api, qs, useSession } from './lib.js';

const requestID = /^(req|chatcmpl|resp|msg)[-_][A-Za-z0-9_-]{6,}$/;

export function useSearchShortcut(open) {
  useEffect(() => {
    const onKey = (e) => {
      const typing = /^(INPUT|TEXTAREA|SELECT)$/.test(e.target.tagName) || e.target.isContentEditable;
      if ((e.key === 'k' && (e.ctrlKey || e.metaKey)) || (e.key === '/' && !typing)) {
        e.preventDefault();
        open();
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
}

export function Search({ pages, onClose }) {
  const s = useSession();
  const [q, setQ] = useState('');
  const [keys, setKeys] = useState([]);
  const [active, setActive] = useState(0);

  useEffect(() => {
    const onKey = (e) => e.key === 'Escape' && onClose();
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);
  // Keys are many, so they are searched on the server, after a pause.
  useEffect(() => {
    setActive(0);
    if (q.trim().length < 2 || !s.has('keys.read')) {
      setKeys([]);
      return;
    }
    const t = setTimeout(() => {
      api('GET', '/admin/v1/keys' + qs({ q: q.trim(), limit: 6 })).then((d) => setKeys(d.data || []), () => setKeys([]));
    }, 200);
    return () => clearTimeout(t);
  }, [q]);

  const term = q.trim().toLowerCase();
  const match = (text) => text && text.toLowerCase().includes(term);
  const results = [];
  if (term) {
    for (const [label, href] of pages) if (match(label)) results.push({ kind: 'Page', label, href });
    for (const [id, name] of Object.entries(s.orgNames)) if (match(name) || id === q.trim()) results.push({ kind: 'Organization', label: name, href: `#/orgs/${id}` });
    for (const m of s.catalog.models) if (match(m.name)) results.push({ kind: 'Model', label: m.name, href: `#/models?edit=${encodeURIComponent(m.name)}` });
    for (const v of s.catalog.virtualServers) if (match(v.name)) results.push({ kind: 'Virtual MCP server', label: v.name, href: `#/mcp?tab=virtual&edit=${encodeURIComponent(v.name)}` });
    for (const k of keys) results.push({ kind: 'Key', label: `${k.name} · ${k.prefix}…`, sub: s.orgNames[k.org_id], href: `#/keys?q=${encodeURIComponent(k.prefix)}` });
    if (requestID.test(q.trim()) && s.has('bodies.read')) results.push({ kind: 'Request', label: q.trim(), href: `#/requests/${encodeURIComponent(q.trim())}` });
  }
  const shown = results.slice(0, 12);
  const go = (r) => {
    location.hash = r.href.slice(1);
    onClose();
  };
  const onKeyDown = (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setActive((a) => Math.min(a + 1, shown.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === 'Enter' && shown[active]) {
      e.preventDefault();
      go(shown[active]);
    }
  };
  return html`<div class="overlay" onClick=${(e) => e.target === e.currentTarget && onClose()}>
    <div class="modal search" role="dialog" aria-modal="true" aria-label="Search">
      <input autofocus type="search" role="combobox" aria-expanded=${shown.length > 0} aria-controls="search-results" aria-activedescendant=${shown.length ? `search-${active}` : undefined}
        placeholder="Search pages, organizations, keys, models, MCP servers or a request id" value=${q}
        onInput=${(e) => setQ(e.target.value)} onKeyDown=${onKeyDown} ref=${(el) => el && !el.dataset.focused && (el.focus(), (el.dataset.focused = '1'))} />
      <ul id="search-results" role="listbox" class="search-results">
        ${shown.map((r, i) => html`<li key=${r.kind + r.href} id=${`search-${i}`} role="option" aria-selected=${i === active} class=${i === active ? 'active' : ''}
          onMouseEnter=${() => setActive(i)} onClick=${() => go(r)}>
          <span class="badge">${r.kind}</span> <span>${r.label}</span>${r.sub ? html` <span class="muted small">${r.sub}</span>` : ''}
        </li>`)}
      </ul>
      ${term && !shown.length && html`<p class="muted">Nothing matches "${q.trim()}".</p>`}
      <p class="muted small">Enter opens the highlighted result; Esc closes.</p>
    </div>
  </div>`;
}
