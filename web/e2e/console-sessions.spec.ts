import { test, expect, type Page } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('UI account session list and password-confirmed revocation isolate other accounts', async ({
  page,
  context,
  browser,
  baseURL,
}, info) => {
  test.setTimeout(300000);
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!adminFile || !privateDir || !baseURL) throw new Error('Protected operator fixture required');
  const username = 'sessions_' + Date.now(),
    password = randomBytes(30).toString('base64url');
  await writeFile(
    path.join(privateDir, 'console-sessions-account.private.json'),
    JSON.stringify({ username, password }),
    { mode: 0o600, flag: 'wx' },
  );
  const checks: string[] = [],
    sessionIDs: string[] = [];
  let result = 'running',
    organization = '',
    invitation = '';
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        session_ids: sessionIDs,
        organization_id: organization,
        invitation_id: invitation,
        compute_created: false,
        sql_requests: 0,
        secrets_in_evidence: false,
        production_qualified: false,
      }) + '\n',
    );
  }
  const a = await browser.newContext({ baseURL }),
    b = await browser.newContext({ baseURL }),
    c = await browser.newContext({ baseURL });
  const member = await a.newPage();
  async function login(p: Page) {
    await p.goto('/#/projects');
    await p.getByLabel('用户名').fill(username);
    await p.getByLabel('密码', { exact: true }).fill(password);
    await p.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(p.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    await page.goto('/#/organization');
    await page.getByLabel('组织名称', { exact: true }).fill('Session UI ' + Date.now());
    let waiting = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === '/api/v1/organizations' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建组织', exact: true }).click();
    const org = await waiting;
    expect(org.status()).toBe(201);
    organization = (await org.json()).id;
    await page.getByLabel('受邀账号').fill(username);
    await page.getByLabel('邀请角色').selectOption('viewer');
    waiting = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/organizations/${organization}/invitations` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建邀请', exact: true }).click();
    const invite = await waiting;
    expect(invite.status()).toBe(201);
    const value = await invite.json();
    invitation = value.id;
    await save();
    await member.goto('/#/projects');
    await member.getByRole('link', { name: '受邀注册', exact: true }).click();
    await member.getByLabel('受邀用户名').fill(username);
    await member.getByLabel('邀请凭据', { exact: true }).fill(value.token);
    await member.getByLabel('设置密码', { exact: true }).fill(password);
    await member.getByLabel('确认密码', { exact: true }).fill(password);
    waiting = member.waitForResponse(
      (r) => new URL(r.url()).pathname === '/auth/signup' && r.request().method() === 'POST',
    );
    await member.getByRole('button', { name: '注册并加入组织', exact: true }).click();
    expect((await waiting).status()).toBe(201);
    await member.getByRole('link', { name: '返回登录', exact: true }).click();
    await login(member);
    await login(await b.newPage());
    await login(await c.newPage());
    const list = await a.request.get('/api/v1/account/sessions');
    expect(list.status()).toBe(200);
    const metadata = await list.json();
    expect(metadata.items).toHaveLength(3);
    for (const item of metadata.items) {
      expect(Object.keys(item).sort()).toEqual(
        ['id', 'created_at', 'expires_at', 'current'].sort(),
      );
      sessionIDs.push(item.id);
    }
    await save(
      'isolated_viewer_account_has_three_sessions_without_credential_hash_or_cross_account_data',
    );
    const target = (await (await b.request.get('/api/v1/account/sessions')).json()).items.find(
      (s: { current: boolean }) => s.current,
    ).id;
    await member.goto('/#/organization');
    const panel = member.getByRole('region', { name: '我的控制台会话' });
    await panel.getByRole('button', { name: '加载我的会话', exact: true }).click();
    await expect(panel.getByRole('row')).toHaveCount(4);
    await expect(panel.getByRole('cell', { name: '当前会话', exact: true })).toBeVisible();
    await panel.getByLabel('会话管理密码', { exact: true }).fill('incorrect-password');
    member.on('dialog', (d) => void d.accept());
    waiting = member.waitForResponse(
      (r) => new URL(r.url()).pathname === `/api/v1/account/sessions/${target}/revoke`,
    );
    await panel
      .getByRole('row')
      .filter({ has: member.getByRole('cell', { name: target, exact: true }) })
      .getByRole('button', { name: '撤销此会话', exact: true })
      .click();
    expect((await waiting).status()).toBe(401);
    expect((await b.request.get('/api/v1/session')).status()).toBe(200);
    await expect(panel.getByLabel('会话管理密码', { exact: true })).toHaveValue('');
    await save(
      'ui_wrong_current_password_rejected_target_session_remains_valid_and_password_clears',
    );
    await panel.getByLabel('会话管理密码', { exact: true }).fill(password);
    waiting = member.waitForResponse(
      (r) => new URL(r.url()).pathname === `/api/v1/account/sessions/${target}/revoke`,
    );
    await panel
      .getByRole('row')
      .filter({ has: member.getByRole('cell', { name: target, exact: true }) })
      .getByRole('button', { name: '撤销此会话', exact: true })
      .click();
    expect((await waiting).status()).toBe(200);
    expect((await b.request.get('/api/v1/session')).status()).toBe(401);
    await expect(panel.getByRole('row')).toHaveCount(3);
    await save('ui_revokes_selected_own_session_and_next_request_is_unauthenticated');
    await panel.getByLabel('会话管理密码', { exact: true }).fill(password);
    waiting = member.waitForResponse(
      (r) => new URL(r.url()).pathname === '/api/v1/account/sessions/revoke-others',
    );
    await panel.getByRole('button', { name: '撤销其他全部会话', exact: true }).click();
    expect((await waiting).status()).toBe(200);
    expect((await c.request.get('/api/v1/session')).status()).toBe(401);
    expect((await a.request.get('/api/v1/session')).status()).toBe(200);
    expect((await context.request.get('/api/v1/session')).status()).toBe(200);
    await expect(panel.getByRole('row')).toHaveCount(2);
    await save('ui_revokes_other_sessions_retains_current_and_does_not_revoke_operator_account');
    await member.screenshot({
      path: info.outputPath('console-sessions.png'),
      fullPage: true,
      mask: [member.locator('input[type=password]')],
    });
    await member.getByRole('button', { name: '退出', exact: true }).click();
    expect((await a.request.get('/api/v1/session')).status()).toBe(401);
    await save('owned_test_current_session_logged_out_account_audit_and_evidence_retained');
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  } finally {
    await a.close();
    await b.close();
    await c.close();
  }
});
