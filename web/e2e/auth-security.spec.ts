import { expect, test, request, type Response } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';

test('Auth local account security starts in UI and isolates passwords and owned sessions', async ({
  page,
  context,
  baseURL,
}, info) => {
  const file = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!file || !admin || !baseURL) throw new Error('Protected Linux owner fixture required');
  const fixture = JSON.parse(await readFile(file, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    role: string;
    database: string;
    password_file: string;
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^ep_[a-f0-9]{16}$/.test(fixture.writer_id) ||
    fixture.database !== 'owner_transfer_v1' ||
    fixture.role !== 'owner_new_v1'
  )
    throw new Error('Exact retained owner scope required');
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  const control = `/api/v1/projects/${project}/branches/${branch}/auth`,
    app = `/auth/v1/${branch}`;
  const checks: string[] = [],
    operations: string[] = [];
  let result = 'running';
  const password = randomBytes(24).toString('base64url'),
    nextPassword = randomBytes(24).toString('base64url');
  const email = 'security-' + Date.now() + '@example.test';
  const peer = await request.newContext({
    baseURL,
    extraHTTPHeaders: { Origin: new URL(baseURL).origin, 'User-Agent': 'Neon-Auth-security-peer' },
  });
  const outsider = await request.newContext({
    baseURL,
    extraHTTPHeaders: {
      Origin: new URL(baseURL).origin,
      'User-Agent': 'Neon-Auth-security-outsider',
    },
  });
  async function save(check?: string) {
    if (check) checks.push(check);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks,
        operations,
        project_id: project,
        branch_id: branch,
        database: fixture.database,
        production_qualified: false,
        credentials_in_evidence: false,
      }) + '\n',
    );
  }
  async function wait(r: Response) {
    expect(r.status()).toBe(202);
    const a = await r.json();
    operations.push(a.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${a.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (op.state === 'failed' || op.state === 'cancelled')
            throw new Error(`Original Auth operation failed ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 300000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function appAction(button: string, action: string) {
    const pending = page.waitForResponse((r) => new URL(r.url()).pathname === app + '/' + action);
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function login(value: string) {
    await page.getByLabel('应用邮箱', { exact: true }).fill(email);
    await page.getByLabel('应用密码', { exact: true }).fill(value);
    return appAction('登录应用', 'sign-in/email');
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标 Endpoint').selectOption(writer);
    await page.getByLabel('数据库角色', { exact: true }).fill(fixture.role);
    await page.getByLabel('目标数据库', { exact: true }).fill(fixture.database);
    await page.getByLabel('执行模式').selectOption('single');
    await page.getByLabel('查询操作').selectOption('query');
    await page
      .getByLabel('SQL 查询')
      .fill(
        `GRANT CREATE, CONNECT ON DATABASE "${fixture.database}" TO control_probe WITH GRANT OPTION`,
      );
    await page
      .getByLabel('数据库角色密码', { exact: true })
      .fill((await readFile(fixture.password_file, 'utf8')).trim());
    const grant = page.waitForResponse(
      (r) => new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/query`,
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    expect((await grant).status()).toBe(200);
    await save('database_owner_explicit_sql_grant_from_workbench');
    await page.goto(`/#/projects/${project}/auth`);
    await page.getByLabel('Auth 数据库', { exact: true }).fill(fixture.database);
    const enabling = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    await wait(await enabling);
    await page.getByLabel('应用姓名', { exact: true }).fill('Local security user');
    await page.getByLabel('应用邮箱', { exact: true }).fill(email);
    await page.getByLabel('应用密码', { exact: true }).fill(password);
    expect((await appAction('注册应用用户', 'sign-up/email')).status()).toBe(200);
    await expect(page.getByTestId('auth-security-panel')).toBeVisible();
    await save('ui_native_auth_enable_signup_and_security_panel');
    expect((await peer.post(app + '/sign-in/email', { data: { email, password } })).status()).toBe(
      200,
    );
    const peerSession = await (await peer.get(app + '/get-session')).json();
    expect(peerSession.user.email).toBe(email);
    expect((await appAction('查看应用会话', 'list-sessions')).status()).toBe(200);
    await expect(page.getByTestId('auth-security-panel')).toContainText('Neon-Auth-security-peer');
    expect(
      (await page.getByTestId('auth-security-panel').innerText()).includes(
        peerSession.session.token,
      ),
    ).toBe(false);
    await save('ui_lists_actual_sessions_without_rendering_bearer_session_tokens');
    await page.getByLabel('当前应用密码', { exact: true }).fill('incorrect-current-password');
    await page.getByLabel('新应用密码', { exact: true }).fill(nextPassword);
    const wrong = await appAction('更新密码并撤销其他会话', 'change-password');
    expect(wrong.status()).toBeGreaterThanOrEqual(400);
    expect((await (await peer.get(app + '/get-session')).json()).user.email).toBe(email);
    await save('wrong_current_password_rejected_without_revoking_existing_session');
    await page.getByLabel('当前应用密码', { exact: true }).fill(password);
    await page.getByLabel('新应用密码', { exact: true }).fill(nextPassword);
    expect((await appAction('更新密码并撤销其他会话', 'change-password')).status()).toBe(200);
    expect((await (await peer.get(app + '/get-session')).json()) === null).toBe(true);
    expect((await peer.get(app + '/token')).status()).toBe(401);
    expect((await login(password)).status()).toBe(401);
    expect((await login(nextPassword)).status()).toBe(200);
    await save(
      'ui_password_change_old_password_denied_new_password_valid_peer_session_and_token_mint_revoked',
    );
    // Honor the real sign-in limiter before an explicit new peer login.
    await new Promise((resolve) => setTimeout(resolve, 11000));
    expect(
      (
        await peer.post(app + '/sign-in/email', { data: { email, password: nextPassword } })
      ).status(),
    ).toBe(200);
    expect((await appAction('查看应用会话', 'list-sessions')).status()).toBe(200);
    const selected = page
      .getByTestId('auth-security-panel')
      .locator('tbody tr')
      .filter({ hasText: 'Neon-Auth-security-peer' });
    const revoke = page.waitForResponse(
      (r) => new URL(r.url()).pathname === app + '/revoke-session',
    );
    await selected.getByRole('button', { name: '撤销此会话', exact: true }).click();
    expect((await revoke).status()).toBe(200);
    expect((await (await peer.get(app + '/get-session')).json()) === null).toBe(true);
    await save('ui_selected_owned_session_revoked_current_session_preserved');
    const otherEmail = 'outsider-' + Date.now() + '@example.test';
    expect(
      (
        await outsider.post(app + '/sign-up/email', {
          data: { email: otherEmail, name: 'Other application user', password: nextPassword },
        })
      ).status(),
    ).toBe(200);
    const otherSession = await (await outsider.get(app + '/get-session')).json();
    const denied = await context.request.post(app + '/revoke-session', {
      headers: { Origin: new URL(baseURL).origin },
      data: { token: otherSession.session.token },
    });
    expect(denied.status()).toBeGreaterThanOrEqual(400);
    expect((await (await outsider.get(app + '/get-session')).json()).user.email).toBe(otherEmail);
    await save('authenticated_application_user_cannot_revoke_another_users_session');
    expect(
      (
        await peer.post(app + '/sign-in/email', { data: { email, password: nextPassword } })
      ).status(),
    ).toBe(200);
    expect((await appAction('撤销其他应用会话', 'revoke-other-sessions')).status()).toBe(200);
    expect((await (await peer.get(app + '/get-session')).json()) === null).toBe(true);
    expect((await appAction('检查应用会话', 'get-session')).status()).toBe(200);
    await expect(page.getByTestId('auth-app-session')).toContainText(email);
    await save('ui_revoke_other_sessions_preserves_current_application_identity');
    await page.screenshot({ path: info.outputPath('auth-security.png'), fullPage: true });
    expect((await appAction('退出应用', 'sign-out')).status()).toBe(200);
    const disabling = page.waitForResponse(
      (r) => new URL(r.url()).pathname === control && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '禁用 Auth', exact: true }).click();
    await wait(await disabling);
    await page.goto(`/#/projects/${project}/compute`);
    const state = await context.request.get(`/api/v1/projects/${project}/endpoints/${writer}`);
    if ((await state.json()).state === 'active') {
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
        timeout: 150000,
      });
    }
    await save('owned_auth_runtime_retired_application_rows_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  } finally {
    await peer.dispose();
    await outsider.dispose();
  }
});
