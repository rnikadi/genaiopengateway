// Accessibility check of the admin UI (WCAG 2.2 AA rules in axe-core), in
// the light and dark themes. quickstart.sh runs it after the setup wizard,
// so the pages have data. It fails on any violation.
import { chromium } from 'playwright';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';

const base = process.env.GATEWAY_URL || 'http://127.0.0.1:8080';
const token = JSON.parse(readFileSync(process.env.BOOTSTRAP_FILE, 'utf8')).admin_token;
const axe = readFileSync(createRequire(import.meta.url).resolve('axe-core/axe.min.js'), 'utf8');
const routes = ['/', '/orgs', '/keys', '/models', '/providers', '/mcp', '/mcp?tab=servers', '/agents', '/guardrails?tab=definitions', '/alerts', '/alerts?tab=webhooks',
  '/settings', '/settings/limits', '/settings/data', '/settings/cache', '/settings/notifications', '/settings/mcp', '/settings/startup',
  '/usage', '/requests', '/requests?tab=refused', '/explain', '/jobs', '/audit', '/setup', '/playground', '/directory'];

const browser = await chromium.launch(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {});
let failed = 0;
const check = async (page, what) => {
  // The gateway's CSP forbids inline scripts, which is why the context
  // bypasses it to inject axe.
  await page.addScriptTag({ content: axe });
  const violations = await page.evaluate(async () => (await window.axe.run(document, { runOnly: ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa'] })).violations);
  for (const v of violations) {
    failed++;
    console.log(`FAIL ${what}: ${v.id} (${v.impact}) ${v.help}`);
    for (const n of v.nodes.slice(0, 3)) console.log(`  ${n.target.join(' ')}: ${(n.failureSummary || '').split('\n').slice(1).join(' ')}`);
  }
};
for (const scheme of ['light', 'dark']) {
  const ctx = await browser.newContext({ colorScheme: scheme, bypassCSP: true, viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(base + '/ui/');
  await page.waitForSelector('input[type=password]');
  await check(page, `${scheme} sign-in`);
  await page.fill('input[type=password]', token);
  await page.click('button[type=submit]');
  await page.waitForSelector('.sidebar');
  for (const r of routes) {
    await page.goto(base + '/ui/#' + r);
    await page.waitForSelector('main h1');
    await page.waitForLoadState('networkidle');
    await check(page, `${scheme} ${r}`);
  }
  await page.keyboard.press('Control+k');
  await page.waitForSelector('.modal.search');
  await check(page, `${scheme} search`);
  await ctx.close();
}
await browser.close();
if (failed) {
  console.log(`${failed} accessibility violations`);
  process.exit(1);
}
console.log(`No accessibility violations on ${routes.length + 2} views in both themes.`);
