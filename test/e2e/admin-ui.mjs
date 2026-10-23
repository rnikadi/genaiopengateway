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
