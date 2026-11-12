// Browser test of sign-in through a real identity provider
// (docs/identity-providers.md). test/idp/run.sh starts a gateway set up
// for the identity provider and runs this. It signs in through the
// identity provider's own login page, checks who the gateway thinks
// signed in and with which role, and signs out. With SAML single logout
// it also checks that the identity provider's session ended.
import { chromium } from 'playwright';
import assert from 'node:assert/strict';

const env = (name, fallback) => {
  const v = process.env[name] ?? fallback;
  if (v === undefined) throw new Error(`${name} is not set`);
  return v;
};
const base = env('GATEWAY_URL');
const idp = env('IDP');
const protocol = env('PROTOCOL');
const user = env('IDP_USER');
const password = env('IDP_PASSWORD');
const email = env('IDP_EMAIL', user).toLowerCase();
const singleLogout = process.env.EXPECT_SINGLE_LOGOUT === '1';

// Each identity provider's login page, as of late 2026. When one changes
// its page, this is the place to update.
const logins = {
  async keycloak(page) {
    await page.fill('#username', user);
    await page.fill('#password', password);
    await page.click('#kc-login');
  },
  async okta(page) {
    // Okta Identity Engine asks for the user first, then the password.
    await page.fill('input[name="identifier"]', user);
    await page.click('input[type="submit"]');
    await page.fill('input[name="credentials.passcode"]', password);
    await page.click('input[type="submit"]');
  },
  async entra(page) {
    await page.fill('input[name="loginfmt"]', user);
    await page.click('#idSIButton9');
    await page.fill('input[name="passwd"]', password);
    await page.click('#idSIButton9');
    // "Stay signed in?" appears for some tenants.
    const stay = page.locator('#idBtn_Back');
    if (await stay.isVisible({ timeout: 5000 }).catch(() => false)) await stay.click();
  },
  async google(page) {
    await page.fill('input[type="email"]', user);
    await page.click('#identifierNext');
    await page.fill('input[type="password"]', password);
    await page.click('#passwordNext');
  },
  async ping(page) {
    // PingOne's hosted sign-on page.
    await page.fill('#username', user);
    await page.fill('#password', password);
    await page.click('button[type="submit"]');
  },
};
const login = logins[idp];
if (!login) throw new Error(`no login steps for ${idp}; known: ${Object.keys(logins).join(', ')}`);

const browser = await chromium.launch(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {});
const page = await browser.newPage();
const step = async (name, fn) => {
  process.stdout.write(`- ${idp} ${protocol}: ${name} … `);
  try {
    await fn();
  } catch (e) {
    if (process.env.SCREENSHOTS) await page.screenshot({ path: `${process.env.SCREENSHOTS}/${idp}-${protocol}-failure.png`, fullPage: true });
    console.log('failed at', page.url());
    throw e;
  }
  console.log('ok');
};
const onGateway = (u) => u.toString().startsWith(base + '/ui/');

await step('start sign-in at the gateway', async () => {
  await page.goto(base + '/ui/');
  await page.click('text=Sign in with single sign-on');
  await page.waitForURL((u) => !u.toString().startsWith(base), { timeout: 30000 });
});

await step('sign in on the identity provider’s page', async () => {
  await login(page);
  await page.waitForURL(onGateway, { timeout: 60000 });
  await page.waitForSelector('#main-nav', { timeout: 30000 });
});

await step('the gateway knows the user and the role', async () => {
  const me = await page.evaluate(async () => (await fetch('/admin/v1/me')).json());
  assert.equal((me.operator?.email || '').toLowerCase(), email, JSON.stringify(me));
  assert.ok(me.operator.grants?.some((g) => g.role === 'owner'), 'mapped group gives owner: ' + JSON.stringify(me.operator.grants));
});

await step('sign out', async () => {
  await page.click('text=Sign out');
  await page.waitForSelector('text=Sign in with single sign-on', { timeout: 60000 });
  assert.ok(onGateway(page.url()), 'back on the gateway: ' + page.url());
  const me = await page.evaluate(async () => (await fetch('/admin/v1/me')).status);
  assert.equal(me, 401, 'the gateway session ended');
});

if (singleLogout) {
  await step('single logout ended the identity provider’s session too', async () => {
    await page.click('text=Sign in with single sign-on');
    await page.waitForURL((u) => !u.toString().startsWith(base), { timeout: 30000 });
    // Without single logout the identity provider would sign the user
    // straight back in; with it, its login page asks again.
    await page.waitForLoadState('networkidle');
    assert.ok(!onGateway(page.url()), 'the identity provider asked to sign in again');
  });
}

await browser.close();
console.log(`${idp} ${protocol}: passed`);
