import { expect, test } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// Live Console UI + actual metadata PostgreSQL. No Compute is provisioned.
// Keep registration/password fixtures in PRIVATE_DIR, redact screenshot inputs,
// and disable traces/video/HAR in the shared Playwright configuration.
test('Console onboarding: one-time invite, UI signup, isolation, revoke and existing account', async ({
  page,
  context,
  baseURL,
  browser,
}, info) => {
  test.setTimeout(180_000);
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!adminFile || !privateDir || !baseURL) throw new Error('Live Console settings required');
  const attempt = process.env.NEON_E2E_ATTEMPT || Date.now().toString();
  const username = 'inv_' + Date.now().toString();
  const password = randomBytes(30).toString('base64url');
  const checks: string[] = [];
  const orgs: string[] = [];
  const invites: string[] = [];
  let result = 'running';
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  await writeFile(
    path.join(privateDir, attempt + '-account.json'),
    JSON.stringify({ username, password }),
    { mode: 0o600, flag: 'wx' },
  );
  const save = () =>
    writeFile(
      info.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          checks,
          organization_ids: orgs,
          invitation_ids: invites,
          compute_created: false,
          secrets_in_report: false,
        },
        null,
        2,
      ) + '\n',
    );
  await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
  await save();
  async function record(name: string) {
    checks.push(name);
    await save();
  }
  async function shot(name: string) {
    await page.screenshot({
      path: info.outputPath(name + '.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
  }
  async function org(name: string) {
    await page.goto('/#/organization');
    await page.getByLabel('组织名称', { exact: true }).fill(name);
    const response = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === '/api/v1/organizations' && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建组织', exact: true }).click();
    const res = await response;
    expect(res.status()).toBe(201);
    const id = (await res.json()).id;
    orgs.push(id);
    await expect(page.getByRole('heading', { name, exact: true })).toBeVisible();
    return id as string;
  }
  async function invite(orgId: string, name: string, role = 'viewer') {
    await page.getByLabel('受邀账号').fill(name);
    await page.getByLabel('邀请角色').selectOption(role);
    const response = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/organizations/${orgId}/invitations` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建邀请', exact: true }).click();
    const res = await response;
    expect(res.status()).toBe(201);
    const value = await res.json();
    invites.push(value.id);
    expect((await page.getByLabel('一次性邀请凭据').inputValue()) === value.token).toBe(true);
    const request = res.request();
    const replay = await context.request.post(res.url(), {
      headers: {
        'X-CSRF-Token': request.headers()['x-csrf-token'],
        'Idempotency-Key': request.headers()['idempotency-key'],
      },
      data: JSON.parse(request.postData()!),
    });
    expect(replay.status()).toBe(200);
    const replayValue = await replay.json();
    expect(replayValue.id).toBe(value.id);
    expect(replayValue.token).toBeUndefined();
    expect(replayValue.secret_available).toBe(false);
    const list = await context.request.get(res.url());
    expect(list.status()).toBe(200);
    expect((await list.text()).includes(value.token)).toBe(false);
    await shot('invitation-' + invites.length);
    await page.getByRole('button', { name: '已保存，关闭凭据', exact: true }).click();
    return value;
  }
  const memberContext = await browser.newContext({ baseURL });
  const member = await memberContext.newPage();
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    const first = await org('Invite UI ' + attempt);
    const firstInvite = await invite(first, username);
    await record('admin_ui_creates_account_bound_invitation_once_and_replay_redacts_secret');
    await member.goto('/#/projects');
    await member.getByRole('link', { name: '受邀注册', exact: true }).click();
    await member.getByLabel('受邀用户名').fill(username);
    await member.getByLabel('邀请凭据', { exact: true }).fill(firstInvite.token);
    await member.getByLabel('设置密码', { exact: true }).fill(password);
    await member.getByLabel('确认密码', { exact: true }).fill(password);
    const registration = member.waitForResponse(
      (r) => new URL(r.url()).pathname === '/auth/signup' && r.request().method() === 'POST',
    );
    await member.getByRole('button', { name: '注册并加入组织', exact: true }).click();
    expect((await registration).status()).toBe(201);
    await expect(member.getByRole('status')).toContainText('注册成功');
    await record('ui_new_account_registers_with_own_password');
    const consumed = await memberContext.request.post(baseURL + '/auth/signup', {
      data: { username, password, token: firstInvite.token },
    });
    expect(consumed.status()).toBe(422);
    await member.getByRole('link', { name: '返回登录', exact: true }).click();
    await member.getByLabel('用户名').fill(username);
    await member.getByLabel('密码', { exact: true }).fill(password);
    await member.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(member.getByRole('button', { name: '切换组织', exact: true })).toContainText(
      'Invite UI ' + attempt,
    );
    const outsider = await memberContext.request.get(
      baseURL + '/api/v1/organizations/local/projects',
    );
    expect(outsider.status()).toBe(404);
    const noAdmin = await memberContext.request.get(
      baseURL + `/api/v1/organizations/${first}/invitations`,
    );
    expect(noAdmin.status()).toBe(403);
    await member.goto('/#/organization');
    await expect(member.getByRole('button', { name: '创建邀请', exact: true })).toHaveCount(0);
    await record('viewer_has_no_admin_invitation_UI_and_other_organization_returns_404');

    const second = await org('Existing UI ' + attempt);
    const secondInvite = await invite(second, username, 'collaborator');
    const noReset = await memberContext.request.post(baseURL + '/auth/signup', {
      data: { username, password: 'Different-Password-123', token: secondInvite.token },
    });
    expect(noReset.status()).toBe(409);
    await member.getByLabel('待接受邀请凭据').fill(secondInvite.token);
    const accept = member.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === '/api/v1/invitations/accept' &&
        r.request().method() === 'POST',
    );
    await member.getByRole('button', { name: '接受组织邀请', exact: true }).click();
    expect((await accept).status()).toBe(201);
    await expect(member.getByRole('button', { name: '切换组织', exact: true })).toContainText(
      'Existing UI ' + attempt,
    );
    await record('existing_account_accepts_as_self_password_cannot_be_reset');

    const revokedName = 'revoked_' + Date.now().toString();
    const revoked = await invite(second, revokedName);
    const revoke = page.waitForResponse(
      (r) => r.url().endsWith('/invitations/' + revoked.id) && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '撤销邀请 ' + revokedName, exact: true }).click();
    expect((await revoke).status()).toBe(200);
    const denied = await memberContext.request.post(baseURL + '/auth/signup', {
      data: { username: revokedName, password, token: revoked.token },
    });
    expect(denied.status()).toBe(422);
    await record('revoked_invitation_cannot_create_account');

    await page.getByRole('button', { name: '刷新成员', exact: true }).click();
    await expect(
      page.getByRole('button', { name: '移除成员 ' + username, exact: true }),
    ).toBeVisible();
    page.once('dialog', (d) => d.accept());
    const removed = page.waitForResponse(
      (r) =>
        r.url().includes(`/organizations/${second}/members/`) && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '移除成员 ' + username, exact: true }).click();
    expect((await removed).status()).toBe(200);
    const after = await memberContext.request.get(
      baseURL + `/api/v1/organizations/${second}/projects`,
    );
    expect(after.status()).toBe(404);
    await record('member_removal_immediately_revokes_organization_access');
    await shot('invitation-history');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    await shot('failure');
    throw error;
  } finally {
    await save();
    await memberContext.close();
  }
});
