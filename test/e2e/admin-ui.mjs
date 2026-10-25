// Browser test of the admin UI against the Docker Compose setup with Dex:
//
//   cd deploy/compose && docker compose -f compose.yaml -f compose.sso.yaml up -d --build
//   cd test/e2e && npm ci && npx playwright install chromium && node admin-ui.mjs
//
// It signs in as each Dex user and checks what each role can do.
import { chromium } from 'playwright';
import assert from 'node:assert/strict';

const base = process.env.GATEWAY_URL || 'http://localhost:8080';
const adminToken = process.env.GATEWAY_ADMIN_TOKEN || 'local-admin-token-change-me-0000000000';
const run = Date.now().toString(36);

async function adminAPI(method, path, body) {
  const res = await fetch(base + path, {
    method,
    headers: { Authorization: `Bearer ${adminToken}`, 'Content-Type': 'application/json' },
    body: body && JSON.stringify(body),
  });
  assert.ok(res.ok, `${method} ${path}: ${res.status} ${await res.clone().text()}`);
  return res.status === 204 ? null : res.json();
}

// admin@example.com is mapped to the organization "acme", which must exist
// before that user signs in.
const orgs = await adminAPI('GET', '/admin/v1/orgs?limit=500');
const acme = orgs.data.find((o) => o.name === 'acme') ?? (await adminAPI('POST', '/admin/v1/orgs', { name: 'acme' }));

const browser = await chromium.launch(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {});
const failures = [];

async function signIn(email) {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  page.on('pageerror', (e) => failures.push(`${email}: ${e}`));
  await page.goto(base + '/ui/');
  await page.click('text=Sign in with single sign-on');
  await page.fill('input[name=login]', email);
  await page.fill('input[name=password]', 'password');
  await page.click('button[type=submit]');
  await page.waitForSelector('.sidebar');
  assert.equal(await page.textContent('.who-name'), email.split('@')[0]);
  return { ctx, page };
}

const step = async (name, fn) => {
  process.stdout.write(`- ${name} … `);
  await fn();
  console.log('ok');
};

// The page's own fetch, as a script on the page would send it.
const pageFetch = (page, method, path, { body = '{}', type = 'application/json' } = {}) =>
  page.evaluate(
    async ([method, path, body, type]) => (await fetch(path, { method, headers: { 'Content-Type': type }, body: method === 'GET' ? undefined : body })).status,
    [method, path, body, type],
  );

const orgName = `e2e-${run}`;

await step('owner manages tenants and keys', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  await page.click('text=Organizations');
  await page.click('text=New organization');
  await page.fill('.modal input[required]', orgName);
  await page.click('.modal button[type=submit]');
  await page.waitForSelector(`h1:has-text("${orgName}")`);
  await page.click('text=New team');
  await page.fill('.modal input[required]', 'platform');
  await page.click('.modal button[type=submit]');
  await page.waitForSelector('h1:has-text("platform")');
  await page.click('text=New key');
  await page.fill('.modal input[required]', 'ci');
  await page.click('.modal button[type=submit]');
  const key = await page.textContent('.secret code');
  assert.match(key, /^sk-gog-/);
  await page.click('text=Done');
  assert.equal(await page.locator('.secret').count(), 0, 'the key is shown only once');

  // The new key works, and stops working once revoked.
  const models = () => fetch(base + '/v1/models', { headers: { Authorization: `Bearer ${key}` } }).then((r) => r.status);
  assert.equal(await models(), 200);
  await page.click('button:has-text("Revoke")');
  await page.click('.modal button.danger');
  await page.waitForSelector('.badge:has-text("Revoked")');
  assert.equal(await models(), 401);
  // With its only key revoked, the team can be deleted.
  await page.click('.page-header button.danger');
  await page.click('.modal button.danger');
  await page.waitForSelector(`h1:has-text("${orgName}")`);
  await page.waitForSelector('text=No teams yet');
  await ctx.close();
});

await step('owner turns on a guardrail and sees it block', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  const org = (await adminAPI('GET', '/admin/v1/orgs?limit=500')).data.find((o) => o.name === orgName);
  await page.goto(`${base}/ui/#/orgs/${org.id}`);
  await page.click('button:has-text("Edit policy")');
  await page.check('.modal label.check:has-text("no-pii") input');
  await page.click('.modal button[type=submit]');
  await page.waitForSelector('.modal', { state: 'detached' });
  assert.deepEqual((await adminAPI('GET', `/admin/v1/orgs/${org.id}`)).guardrails, ['no-pii']);

  const team = await adminAPI('POST', `/admin/v1/orgs/${org.id}/teams`, { name: 'guarded' });
  const { id: keyID, key } = await adminAPI('POST', `/admin/v1/teams/${team.id}/keys`, { name: 'guarded' });
  const res = await fetch(base + '/v1/chat/completions', {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: 'chat-default', messages: [{ role: 'user', content: 'Mail jane@example.com' }] }),
  });
  assert.equal(res.status, 400);
  assert.equal((await res.json()).error.code, 'guardrail_blocked');

  // Events are written in batches; the page shows the block once it lands.
  await page.goto(`${base}/ui/#/guardrails?org=${org.id}`);
  for (let i = 0; i < 20 && !(await page.locator('td:has-text("no-pii")').count()); i++) {
    await page.waitForTimeout(500);
    await page.reload();
  }
  await page.waitForSelector('td .badge:has-text("block")');
  assert.equal(await page.locator('text=jane@example.com').count(), 0, 'matched text is never shown');
  await adminAPI('POST', `/admin/v1/keys/${keyID}/revoke`); // so cleanup can delete the team
  await ctx.close();
});

await step('owner calls a model as a team in the playground', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  const org = (await adminAPI('GET', '/admin/v1/orgs?limit=500')).data.find((o) => o.name === orgName);
  await page.click('nav >> text=Playground');
  // The Overview has an organization picker too: pick on the playground.
  await page.waitForSelector('h1:has-text("Playground")');
  await page.selectOption('label:has-text("Organization") select', org.id);
  try {
    await page.waitForSelector('label:has-text("Team") option:has-text("guarded")', { state: 'attached', timeout: 10000 });
  } catch (err) {
    // Say what the page shows instead, so a failure in CI can be diagnosed.
    const shown = await page.evaluate(() => ({
      org: document.querySelector('label.inline select')?.value,
      teams: [...document.querySelectorAll('label.inline')].map((l) => l.textContent),
      errors: [...document.querySelectorAll('.notice.error, .error')].map((e) => e.textContent),
      text: document.querySelector('main')?.innerText.slice(0, 600),
    }));
    console.log('playground shows', JSON.stringify(shown), 'expected org', org.id);
    throw err;
  }
  await page.selectOption('label:has-text("Team") select', { label: 'guarded' });
  await page.fill('textarea[placeholder=Message]', 'Mail jane@example.com');
  await page.click('button:has-text("Send")');
  // The team's guardrails apply in the playground as they do for its keys.
  await page.waitForSelector('.notice.error:has-text("no-pii")');
  const audit = await adminAPI('GET', `/admin/v1/audit?org_id=${org.id}`);
  assert.equal(audit.data[0].action, 'playground.chat');
  await ctx.close();
});

await step('owner approves an MCP tool', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  await page.click('text=MCP');
  await page.waitForSelector('text=Servers');
  if (await page.locator('.attention').count()) {
    const before = await page.locator('.attention tbody tr').count();
    await page.click('.attention button:has-text("Review")');
    await page.click('text=Approve this definition');
    await page.waitForSelector('.modal', { state: 'detached' });
    await page.waitForFunction((n) => (document.querySelectorAll('.attention tbody tr').length || 0) < n, before);
  }
  await ctx.close();
});

await step('owner sees everything the gateway is configured with', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  await page.click('nav >> text=Providers');
  await page.waitForSelector('td:has-text("openai")');
  assert.ok(await page.locator('td:has-text("Anthropic")').count(), 'provider types are shown');
  await page.click('nav >> text=Models');
  // chat-default is on the Providers page too, so wait for what only Models shows.
  await page.waitForSelector('td strong:has-text("chat-default") + .chips .badge:has-text("Chat completions")');
  await page.click('nav >> text=Guardrails');
  await page.click('[role=tab]:has-text("Definitions")');
  await page.waitForSelector('td code:has-text("no-pii")');
  await page.click('nav >> text=Alerts');
  await page.waitForSelector('h2:has-text("Webhooks")');
  await page.click('nav >> text=Settings');
  await page.waitForSelector('dt:has-text("Version")');
  assert.ok(await page.locator('text=Identity provider').count(), 'sign-in settings are shown');
  const html = await page.content();
  for (const secret of [adminToken, 'local-dex-client-secret']) assert.ok(!html.includes(secret), 'no secret on the settings page');
  await ctx.close();
});

await step('owner turns on the response cache and clears it', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  const org = (await adminAPI('GET', '/admin/v1/orgs?limit=500')).data.find((o) => o.name === orgName);
  const team = await adminAPI('POST', `/admin/v1/orgs/${org.id}/teams`, { name: 'cached' });
  await page.goto(`${base}/ui/#/teams/${team.id}`);
  await page.click('button:has-text("Edit policy")');
  await page.selectOption('.modal label:has-text("Response cache") select', 'exact');
  await page.fill('.modal label:has-text("Reuse answers for") input', '10m');
  await page.click('.modal button[type=submit]');
  await page.waitForSelector('.modal', { state: 'detached' });
  assert.deepEqual((await adminAPI('GET', `/admin/v1/teams/${team.id}`)).cache, { mode: 'exact', ttl: '10m' });
  await page.waitForSelector('text=Exact cache 10m');

  // The compose stack has no live upstream, so hits and savings are
  // checked by the Go and SDK conformance tests; here the team's cache
  // is cleared from its page.
  await page.goto(`${base}/ui/#/teams/${team.id}`);
  await page.click('button:has-text("Clear cache")');
  await page.click('.modal button:has-text("Clear cache")');
  await page.waitForSelector('text=Cleared 0 cached answers.');
  const audit = await adminAPI('GET', `/admin/v1/audit?org_id=${org.id}`);
  assert.equal(audit.data[0].action, 'cache.clear');
  await ctx.close();
});

await step('owner opens batches and video jobs', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  await page.click('nav >> text=Jobs');
  await page.waitForSelector('[role=tab][aria-selected=true]:has-text("Batches")');
  await page.waitForSelector('text=No batches yet');
  await page.selectOption('label:has-text("Status") select', 'completed');
  await page.waitForSelector('text=No batches yet');
  await page.click('[role=tab]:has-text("Video jobs")');
  await page.waitForSelector('text=No video jobs yet');
  await ctx.close();
});

await step('owner changes a gateway setting and resets it', async () => {
  // Start from the file's settings, even after an earlier run failed here.
  if ((await adminAPI('GET', '/admin/v1/settings')).fields.some((f) => f.source === 'stored')) {
    await adminAPI('DELETE', '/admin/v1/config/settings/gateway');
  }
  const { ctx, page } = await signIn('owner@example.com');
  await page.click('nav >> text=Settings');
  const field = (path) => page.locator(`[data-setting="${path}"]`);
  await field('limits.max_realtime_session').locator('.badge:has-text("Default")').waitFor();
  assert.ok(await field('limits.max_realtime_session').locator('text=Default: 30m').count(), 'the default is shown');
  await field('limits.max_realtime_session').locator('input').fill('45m');
  await page.locator('section:has(h2:has-text("Request limits")) button[type=submit]').click();
  await field('limits.max_realtime_session').locator('.badge:has-text("Set here")').waitFor();
  const stored = (await adminAPI('GET', '/admin/v1/settings')).fields.find((f) => f.path === 'limits.max_realtime_session');
  assert.deepEqual([stored.value, stored.source], ['45m', 'stored']);
  // A bad value is refused with the reason, and nothing is saved.
  await field('mcp.refresh_interval').locator('input').fill('soon');
  await page.locator('section:has(h2:has-text("MCP")) button[type=submit]').click();
  await page.waitForSelector('section:has(h2:has-text("MCP")) .notice.error:has-text("invalid duration")');
  await field('limits.max_realtime_session').locator('button:has-text("Reset")').click();
  await field('limits.max_realtime_session').locator('.badge:has-text("Default")').waitFor();
  assert.ok(!(await adminAPI('GET', '/admin/v1/settings')).stored.limits, 'the reset removes the stored value');
  await ctx.close();
});

await step('owner adds a guardrail with the form', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  const name = `e2e-regex-${run}`;
  await page.click('nav >> text=Configuration');
  await page.click('[role=tab]:has-text("Guardrails")');
  await page.click('button:has-text("New guardrail")');
  await page.fill('.modal label:has-text("Name") input', name);
  await page.selectOption('.modal [data-setting="guardrails.type"] select', 'regex');
  await page.fill('.modal [data-setting="guardrails.patterns"] textarea', 'ticket-[0-9]+');
  await page.click('.modal button[type=submit]');
  await page.waitForSelector(`td code:has-text("${name}")`);
  const obj = (await adminAPI('GET', '/admin/v1/config')).data.find((o) => o.name === name);
  assert.deepEqual(obj.spec, { type: 'regex', action: 'block', stages: ['input'], patterns: ['ticket-[0-9]+'] });
  await adminAPI('DELETE', `/admin/v1/config/guardrails/${name}`);
  await ctx.close();
});

await step('owner adds a provider and a model from the UI', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  const provider = `e2e-${run}`;
  const model = `e2e-chat-${run}`;
  await page.click('nav >> text=Configuration');
  await page.waitForSelector('td code:has-text("openai")');
  assert.ok(await page.locator('.badge:has-text("From config file")').count(), 'file objects are labelled');
  await page.click('button:has-text("New provider")');
  await page.fill('.modal label:has-text("Name") input', provider);
  await page.fill('.modal label:has-text("Base URL") input', 'http://127.0.0.1:9/v1');
  await page.click('.modal button[type=submit]');
  await page.waitForSelector(`td code:has-text("${provider}")`);
  await page.click('[role=tab]:has-text("Models")');
  await page.click('button:has-text("New model")');
  await page.fill('.modal label:has-text("Name") input', model);
  await page.selectOption('.modal label:has-text("provider") select', provider);
  await page.fill('.modal label:has-text("Upstream model") input', 'm');
  await page.click('.modal button[type=submit]');
  await page.waitForSelector(`td code:has-text("${model}")`);
  // A model on a provider that does not exist is refused, with the reason.
  await page.click('button:has-text("New model")');
  await page.fill('.modal label:has-text("Name") input', `broken-${run}`);
  await page.click('.modal [role=tab]:has-text("Edit as JSON")');
  await page.fill('.modal textarea', '{"targets": [{"provider": "missing", "model": "m"}]}');
  await page.click('.modal button[type=submit]');
  await page.waitForSelector('.modal .notice.error:has-text("missing")');
  await page.click('.modal button:has-text("Cancel")');
  // The new model is live on this replica.
  await page.click('nav >> text=Models');
  await page.waitForSelector(`td strong:has-text("${model}")`);
  await adminAPI('DELETE', `/admin/v1/config/models/${model}`);
  await adminAPI('DELETE', `/admin/v1/config/providers/${provider}`);
  const audit = await adminAPI('GET', '/admin/v1/audit?limit=4');
  assert.deepEqual(audit.data.map((a) => a.action), ['config.delete', 'config.delete', 'config.create', 'config.create']);
  await ctx.close();
});

await step('viewer reads but cannot change anything', async () => {
  const { ctx, page } = await signIn('viewer@example.com');
  await page.click('text=Organizations');
  await page.click(`a:has-text("${orgName}")`);
  await page.waitForSelector('text=Teams');
  for (const label of ['Edit policy', 'Delete', 'New team']) {
    assert.equal(await page.locator(`button:has-text("${label}")`).count(), 0, `viewer sees "${label}"`);
  }
  assert.equal(await pageFetch(page, 'POST', '/admin/v1/orgs', { body: '{"name":"nope"}' }), 403);
  assert.equal(await pageFetch(page, 'GET', '/admin/v1/usage'), 200);
  assert.equal(await page.locator('nav >> text=Settings').count(), 0, 'settings are for owners');
  assert.equal(await pageFetch(page, 'GET', '/admin/v1/settings'), 403);
  assert.equal(await pageFetch(page, 'GET', '/admin/v1/config'), 403);
  assert.equal(await pageFetch(page, 'GET', '/admin/v1/providers'), 200);
  await ctx.close();
});

await step('organization admin sees and changes only their organization', async () => {
  const { ctx, page } = await signIn('admin@example.com');
  await page.click('text=Organizations');
  await page.waitForSelector('table');
  const names = await page.locator('tbody tr td:first-child').allTextContents();
  assert.deepEqual(names, ['acme']);
  assert.equal(await page.locator('button:has-text("New organization")').count(), 0);
  assert.equal(await pageFetch(page, 'POST', `/admin/v1/orgs/${acme.id}/teams`, { body: `{"name":"team-${run}"}` }), 201);
  const other = (await adminAPI('GET', '/admin/v1/orgs?limit=500')).data.find((o) => o.name === orgName);
  assert.equal(await pageFetch(page, 'GET', `/admin/v1/orgs/${other.id}`), 403);
  assert.equal(await pageFetch(page, 'GET', '/admin/v1/usage'), 403, 'usage across all organizations');
  await ctx.close();
});

await step('cookie requests need JSON from the same origin', async () => {
  const { ctx, page } = await signIn('owner@example.com');
  assert.equal(await pageFetch(page, 'POST', '/admin/v1/orgs', { body: '{"name":"csrf"}', type: 'text/plain' }), 403);
  await ctx.close();
});

await step('signing out ends the session', async () => {
  const { ctx, page } = await signIn('viewer@example.com');
  await page.click('text=Sign out');
  await page.waitForSelector('text=Sign in with single sign-on');
  assert.equal(await pageFetch(page, 'GET', '/admin/v1/me'), 401);
  await ctx.close();
});

await browser.close();
// Clean up what the test created.
const created = (await adminAPI('GET', '/admin/v1/orgs?limit=500')).data.find((o) => o.name === orgName);
if (created) {
  for (const t of (await adminAPI('GET', `/admin/v1/orgs/${created.id}/teams`)).data) await adminAPI('DELETE', `/admin/v1/teams/${t.id}`);
  await adminAPI('DELETE', `/admin/v1/orgs/${created.id}`);
}
assert.deepEqual(failures, [], 'errors on the page');
console.log('admin UI: all checks passed');
