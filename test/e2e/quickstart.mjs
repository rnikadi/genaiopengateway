// Browser test of the zero-config start and the setup wizard: a new user
// gets a first completion through the gateway without writing YAML
// (docs/quickstart.md). quickstart.sh starts the gateway with no config
// file and a fake upstream, then runs this.
import { chromium } from 'playwright';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const base = process.env.GATEWAY_URL || 'http://127.0.0.1:8080';
const upstream = process.env.UPSTREAM_URL || 'http://127.0.0.1:18291/v1';
const token = JSON.parse(readFileSync(process.env.BOOTSTRAP_FILE, 'utf8')).admin_token;
const started = Date.now();

const browser = await chromium.launch(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {});
const page = await browser.newPage();
const errors = [];
page.on('pageerror', (e) => errors.push(String(e)));

// SCREENSHOTS=dir saves the page after each step, for docs and review.
let shot = 0;
const step = async (name, fn) => {
  process.stdout.write(`- ${name} … `);
  await fn();
  if (process.env.SCREENSHOTS) await page.screenshot({ path: `${process.env.SCREENSHOTS}/${++shot}.png`, fullPage: true });
  console.log('ok');
};

await step('sign in with the generated token', async () => {
  await page.goto(base + '/ui/');
  await page.fill('input[type=password]', token);
  await page.click('button[type=submit]');
  await page.waitForSelector('text=Set up your gateway');
});

await step('pick a preset and add a provider', async () => {
  await page.click('text=Start the setup wizard');
  await page.click('button.preset:has-text("vLLM")');
  if (process.env.SCREENSHOTS) await page.screenshot({ path: `${process.env.SCREENSHOTS}/presets.png`, fullPage: true });
  await page.fill('input[type=url]', upstream);
  await page.fill('input[type=password]', 'upstream-key');
  await page.getByLabel('Other model ids').fill('bench-upstream');
  await page.click('text=Add provider and models');
});

await step('create a team and key', async () => {
  await page.click('text=Create team and key');
});

await step('send a test request', async () => {
  await page.waitForSelector('text=It works.', { timeout: 20000 });
  assert.match(await page.textContent('blockquote'), /Hello/);
  await page.click('text=Continue');
});

let key;
await step('copy a snippet and call the gateway with it', async () => {
  key = (await page.textContent('.secret code')).trim();
  assert.match(await page.textContent('pre.snippet'), new RegExp(key));
  const res = await fetch(base + '/v1/chat/completions', {
    method: 'POST',
    headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
    body: JSON.stringify({ model: 'bench-upstream', messages: [{ role: 'user', content: 'Hello' }] }),
  });
  assert.equal(res.status, 200, await res.clone().text());
  assert.equal((await res.json()).choices[0].message.content, 'Hello.');
});

await browser.close();
assert.deepEqual(errors, [], 'page errors');
const seconds = Math.round((Date.now() - started) / 1000);
console.log(`first completion through the gateway ${seconds}s after opening the UI`);
assert.ok(seconds < 300, 'the quick start must take under 5 minutes');
