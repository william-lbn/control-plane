import { expect, test, type BrowserContext, type Page } from '@playwright/test';
import { createHmac, randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

function totp(secret: string, offset = 0) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567',
    bytes: number[] = [];
  let bits = 0,
    value = 0;
  for (const ch of secret) {
    value = (value << 5) | alphabet.indexOf(ch);
    bits += 5;
    if (bits >= 8) {
      bytes.push((value >> (bits - 8)) & 255);
      bits -= 8;
    }
  }
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000) + offset));
  const hash = createHmac('sha1', Buffer.from(bytes)).update(counter).digest(),
    at = hash[19] & 15;
  return String((hash.readUInt32BE(at) & 0x7fffffff) % 1000000).padStart(6, '0');
}

test('Console SSO: cross-site code flow, explicit binding, required policy, local MFA and retirement', async ({
  page,
  browser,
  baseURL,
}, info) => {
  test.setTimeout(240000);
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    fixtureFile = process.env.NEON_E2E_CONSOLE_SSO_FIXTURE,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!privateDir || !fixtureFile || !adminFile || baseURL !== 'http://192.168.146.100:30788')
    throw new Error('Protected exact local Console SSO fixture required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    organization_id: string;
    organization_name: string;
    username: string;
    password: string;
    idp_password: string;
    issuer: string;
    subject: string;
    resume_file?: string;
  };
  if (
    !/^org_[a-f0-9]{24}$/.test(f.organization_id) ||
    !/^sso_\d{14}$/.test(f.username) ||
    f.issuer !== 'http://192.168.146.101:31851'
  )
    throw new Error('Unowned SSO fixture');
  type State = {
    organization_id: string;
    username: string;
    checks: string[];
    running: string;
    binding_id: string;
    mfa_secret: string;
    recovery_codes: string[];
    used_recovery: number[];
  };
  const state: State = f.resume_file
    ? JSON.parse(await readFile(f.resume_file, 'utf8'))
    : {
        organization_id: f.organization_id,
        username: f.username,
        checks: [],
        running: '',
        binding_id: '',
        mfa_secret: '',
        recovery_codes: [],
        used_recovery: [],
      };
  if (state.organization_id !== f.organization_id || state.username !== f.username || state.running)
    throw new Error('Inspect original pending mutation before continuing this case');
  const orgPath = `/api/v1/organizations/${f.organization_id}`,
    policyPath = orgPath + '/sso';
  const contexts: BrowserContext[] = [];
  let result = 'running',
    ssoPage: Page | undefined,
    recoveryPage: Page | undefined;
  await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
  async function save() {
    await writeFile(
      path.join(privateDir!, 'console-sso-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          checks: state.checks,
          organization_id: state.organization_id,
          username: state.username,
          binding_id: state.binding_id,
          compute_created: false,
          sql_requests: 0,
          cross_site_issuer: true,
          external_IdP_certified: false,
          credentials_exported: false,
        },
        null,
        2,
      ) + '\n',
    );
  }
  async function step(name: string, run: () => Promise<void>) {
    if (state.checks.includes(name)) return;
    state.running = name;
    await save();
    await run();
    state.checks.push(name);
    state.running = '';
    await save();
  }
  async function csrf(p: Page) {
    return (await p.context().cookies(baseURL)).find((c) => c.name === 'neon_v2_csrf')?.value || '';
  }
  async function shot(p: Page, name: string) {
    await p.screenshot({
      path: info.outputPath(name + '.png'),
      fullPage: true,
      mask: [
        p.locator('input[type="password"]'),
        p.getByLabel('验证器密钥'),
        p.getByLabel('一次性恢复码'),
      ],
    });
  }
  async function fresh() {
    const c = await browser.newContext({ baseURL });
    contexts.push(c);
    const p = await c.newPage();
    p.setDefaultTimeout(20000);
    return p;
  }
  function recovery() {
    const n = [9, 8, 7, 6, 5, 4, 3, 2, 1, 0].find((i) => !state.used_recovery.includes(i));
    if (n === undefined) throw new Error('No unused owned recovery proof');
    state.used_recovery.push(n);
    return state.recovery_codes[n];
  }
  async function localLogin(p: Page) {
    await p.goto('/#/login');
    await p.getByLabel('用户名', { exact: true }).fill(f.username);
    await p.getByLabel('密码', { exact: true }).fill(f.password);
    if (state.mfa_secret) {
      await p.getByText('已启用双重验证？', { exact: true }).click();
      await p.getByLabel('使用恢复码登录', { exact: true }).setChecked(true);
      const code = recovery();
      await save();
      await p.getByLabel('登录验证码', { exact: true }).fill(code);
    }
    const response = p.waitForResponse(
      (r) => new URL(r.url()).pathname === '/auth/login' && r.request().method() === 'POST',
    );
    await p.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    expect((await response).status()).toBe(200);
    await p.goto('/#/organization');
  }
  async function idpLogin(p: Page) {
    await expect(p).toHaveURL(new RegExp('^' + f.issuer.replaceAll('.', '\\.') + '/authorize\\?'));
    await p.getByLabel('IdP 用户名', { exact: true }).fill(f.username);
    await p.getByLabel('IdP 密码', { exact: true }).fill(f.idp_password);
    await p.getByRole('button', { name: '在 IdP 登录', exact: true }).click();
    await p.waitForURL((u) => u.origin === baseURL && u.hash.length > 0);
  }
  async function ssoLogin(p: Page) {
    await p.goto('/#/login');
    await p.getByText('组织单点登录', { exact: true }).click();
    await p.getByLabel('SSO 组织 ID', { exact: true }).fill(f.organization_id);
    await p.getByRole('button', { name: '使用组织 SSO 登录', exact: true }).click();
    await idpLogin(p);
  }
  async function ensureSSO() {
    if (!ssoPage) {
      ssoPage = await fresh();
      await ssoLogin(ssoPage);
      if (state.mfa_secret) {
        const code = recovery();
        await save();
        await ssoPage.getByLabel('使用恢复码', { exact: true }).setChecked(true);
        await ssoPage.getByLabel('SSO 登录验证码', { exact: true }).fill(code);
        await ssoPage.getByRole('button', { name: '验证并进入控制台', exact: true }).click();
      }
      await expect(ssoPage).toHaveURL(/#\/organization$/);
    }
    return ssoPage;
  }
  async function uiPolicy(p: Page, mode: string, expected = 200) {
    await p.goto('/#/organization');
    const panel = p.getByRole('region', { name: '控制台组织 SSO' });
    await panel.getByLabel('SSO 当前控制台密码', { exact: true }).fill(f.password);
    await panel.getByLabel('SSO 控制台策略', { exact: true }).selectOption(mode);
    const waiting = p.waitForResponse(
      (r) => new URL(r.url()).pathname === policyPath && r.request().method() === 'PUT',
    );
    await panel.getByRole('button', { name: '保存 SSO 策略', exact: true }).click();
    const response = await waiting;
    expect(response.status()).toBe(expected);
    return response;
  }
  await save();
  try {
    await localLogin(page);
    await step('UI_optional_policy_original_key_recovers_committed_unknown_reply', async () => {
      let writes = 0,
        originalKey = '',
        originalBody = '';
      await page.route('**' + policyPath, async (route) => {
        if (route.request().method() !== 'PUT') {
          await route.continue();
          return;
        }
        writes++;
        const req = route.request();
        if (writes === 1) {
          originalKey = req.headers()['idempotency-key'];
          originalBody = req.postData()!;
          const actual = await route.fetch();
          expect(actual.status()).toBe(200);
          await route.fulfill({
            status: 502,
            contentType: 'application/json',
            body: JSON.stringify({ code: 'owned_reply_lost', message: '结果未确认' }),
          });
        } else {
          expect(
            req.headers()['idempotency-key'] === originalKey && req.postData() === originalBody,
          ).toBe(true);
          await route.continue();
        }
      });
      await uiPolicy(page, 'optional', 502);
      const panel = page.getByRole('region', { name: '控制台组织 SSO' });
      await expect(panel.getByLabel('SSO 当前控制台密码')).toBeDisabled();
      expect(writes).toBe(1);
      const waiting = page.waitForResponse(
        (r) => new URL(r.url()).pathname === policyPath && r.request().method() === 'PUT',
      );
      await panel.getByRole('button', { name: '继续确认原 SSO 策略提交', exact: true }).click();
      const replay = await waiting;
      expect(replay.status()).toBe(200);
      expect(replay.headers()['idempotent-replay']).toBe('true');
      expect(writes).toBe(2);
      await page.unroute('**' + policyPath);
      await expect(panel.getByRole('status')).toContainText('已保存');
      await shot(page, 'policy-original-key');
    });
    await step('UI_explicit_password_backed_binding_crosses_actual_IdP_origin', async () => {
      const panel = page.getByRole('region', { name: '控制台组织 SSO' });
      await panel.getByLabel('SSO 当前控制台密码').fill(f.password);
      await panel.getByRole('button', { name: '绑定当前账号的 SSO 身份', exact: true }).click();
      await idpLogin(page);
      await expect(page.getByRole('region', { name: '控制台组织 SSO' })).toContainText(f.subject);
      const response = await page.request.get(policyPath);
      expect(response.status()).toBe(200);
      const view = await response.json();
      const binding = view.bindings.find(
        (b: { subject: string; revoked_at: string | null }) =>
          b.subject === f.subject && !b.revoked_at,
      );
      expect(!!binding).toBe(true);
      state.binding_id = binding.id;
      await shot(page, 'explicit-binding');
    });
    await step('UI_fresh_SSO_login_issues_Strict_session_for_existing_member', async () => {
      const p = await ensureSSO(),
        response = await p.request.get('/api/v1/session');
      expect(response.status()).toBe(200);
      const user = (await response.json()).user;
      expect(user.username === f.username && user.sso_organization_id === f.organization_id).toBe(
        true,
      );
      const cookie = (await p.context().cookies(baseURL)).find((c) => c.name === 'neon_v2_session');
      expect(cookie?.httpOnly && cookie.sameSite === 'Strict').toBe(true);
      await shot(p, 'sso-session');
    });
    await step('UI_required_policy_blocks_local_org_access_preserves_proven_SSO', async () => {
      const p = await ensureSSO();
      await uiPolicy(p, 'required');
      expect((await page.request.get(orgPath)).status()).toBe(403);
      expect((await page.request.get('/api/v1/session')).status()).toBe(200);
      expect((await p.request.get(orgPath)).status()).toBe(200);
      await shot(p, 'required-policy');
    });
    await step('owned_existing_MFA_setup_preserves_current_SSO_session', async () => {
      const p = await ensureSSO(),
        headers = { 'X-CSRF-Token': await csrf(p) };
      const enrollment = await p.request.post('/api/v1/account/mfa/enroll', {
        headers,
        data: { password: f.password },
      });
      expect(enrollment.status()).toBe(200);
      state.mfa_secret = (await enrollment.json()).secret;
      await save();
      const confirmation = await p.request.post('/api/v1/account/mfa/confirm', {
        headers,
        data: { password: f.password, code: totp(state.mfa_secret) },
      });
      expect(confirmation.status()).toBe(200);
      state.recovery_codes = (await confirmation.json()).recovery_codes;
      await save();
      expect((await p.request.get('/api/v1/session')).status()).toBe(200);
    });
    await step('UI_OIDC_assertion_cannot_bypass_local_TOTP_challenge', async () => {
      const p = await fresh();
      await ssoLogin(p);
      await expect(p).toHaveURL(/#\/sso-mfa$/);
      expect((await p.request.get('/api/v1/session')).status()).toBe(401);
      await p.getByLabel('SSO 登录验证码').fill(totp(state.mfa_secret, 1));
      await p.getByRole('button', { name: '验证并进入控制台', exact: true }).click();
      await expect(p).toHaveURL(/#\/organization$/);
      expect((await p.request.get(orgPath)).status()).toBe(200);
      ssoPage = p;
      await shot(p, 'sso-mfa-complete');
    });
    await step('UI_OIDC_local_recovery_proof_is_consumed_once', async () => {
      const p = await fresh();
      await ssoLogin(p);
      await expect(p).toHaveURL(/#\/sso-mfa$/);
      const code = recovery();
      await save();
      await p.getByLabel('使用恢复码', { exact: true }).setChecked(true);
      await p.getByLabel('SSO 登录验证码').fill(code);
      await p.getByRole('button', { name: '验证并进入控制台', exact: true }).click();
      await expect(p).toHaveURL(/#\/organization$/);
      expect((await p.request.get(orgPath)).status()).toBe(200);
      recoveryPage = p;
      const mfa = await p.request.get('/api/v1/account/mfa');
      expect(mfa.status()).toBe(200);
      expect((await mfa.json()).recovery_codes_remaining).toBe(10 - state.used_recovery.length);
      const replay = await fresh();
      await ssoLogin(replay);
      await expect(replay).toHaveURL(/#\/sso-mfa$/);
      await replay.getByLabel('使用恢复码', { exact: true }).setChecked(true);
      await replay.getByLabel('SSO 登录验证码').fill(code);
      const waiting = replay.waitForResponse(
        (r) => new URL(r.url()).pathname === '/auth/sso/mfa' && r.request().method() === 'POST',
      );
      await replay.getByRole('button', { name: '验证并进入控制台', exact: true }).click();
      expect((await waiting).status()).toBe(401);
      expect((await replay.request.get('/api/v1/session')).status()).toBe(401);
    });
    await step('UI_policy_generation_retires_old_SSO_sessions', async () => {
      const p = await ensureSSO();
      await uiPolicy(p, 'optional');
      expect((await p.request.get(orgPath)).status()).toBe(200);
      if (recoveryPage)
        expect((await recoveryPage.request.get('/api/v1/session')).status()).toBe(401);
    });
    await step('UI_own_binding_revoke_closes_associated_SSO_session', async () => {
      const p = await ensureSSO(),
        panel = p.getByRole('region', { name: '控制台组织 SSO' });
      await panel.getByLabel('SSO 当前控制台密码').fill(f.password);
      const waiting = p.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === policyPath + '/bindings/' + state.binding_id &&
          r.request().method() === 'DELETE',
      );
      await panel.getByRole('button', { name: '撤销此 SSO 绑定', exact: true }).click();
      expect((await waiting).status()).toBe(200);
      await expect(p.getByRole('heading', { name: '登录控制台', exact: true })).toBeVisible();
      expect((await p.request.get('/api/v1/session')).status()).toBe(401);
    });
    await step('only_new_org_policy_disabled_existing_admin_access_preserved', async () => {
      const c = await browser.newContext({ baseURL });
      contexts.push(c);
      const login = await c.request.post('/auth/login', {
        data: { username: 'admin', password: (await readFile(adminFile, 'utf8')).trim() },
      });
      expect(login.status()).toBe(200);
      const cookies = await c.cookies(baseURL),
        csrf = cookies.find((x) => x.name === 'neon_v2_csrf')?.value || '';
      const view = await c.request.get(policyPath);
      expect(view.status()).toBe(200);
      const version = (await view.json()).version;
      const disabled = await c.request.put(policyPath, {
        headers: {
          'X-CSRF-Token': csrf,
          'If-Match': `"${version}"`,
          'Idempotency-Key': randomUUID(),
        },
        data: {
          provider_id: 'lab-oidc',
          mode: 'disabled',
          password: (await readFile(adminFile, 'utf8')).trim(),
        },
      });
      expect(disabled.status()).toBe(200);
      expect((await c.request.get('/api/v1/organizations/local')).status()).toBe(200);
    });
    result = 'pass';
    await save();
  } finally {
    await save();
    for (const c of contexts) await c.close();
  }
});
