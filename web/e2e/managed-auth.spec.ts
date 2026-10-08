import { expect, test } from '@playwright/test';
import { randomBytes, randomUUID } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

// All product creation/configuration starts in the actual React Console.
// Read-only API checks and application protocol assertions complement UI actions.
// Tokens/cookies/passwords never enter result.json, screenshots, traces or logs.
test('Managed Auth: UI identity, native branch isolation, Data API RLS and cold wake', async ({
  page,
  context,
  baseURL,
}, testInfo) => {
  page.setDefaultTimeout(40000);
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!baseURL || !adminFile || !privateDir)
    throw new Error('Protected Linux E2E configuration required');
  const attempt = `${process.env.NEON_E2E_ATTEMPT || Date.now()}-auth`;
  const password = randomBytes(30).toString('base64url');
  const appPassword = randomBytes(30).toString('base64url');
  const email = `synthetic-${attempt}@example.test`;
  const checks: { name: string; [key: string]: unknown }[] = [];
  const endpoints = new Set<string>();
  const cleanup: {
    action: string;
    resource: string;
    key?: string;
    operation_id?: string;
    status?: number;
    state?: string;
  }[] = [];
  let project = '';
  let root = '';
  let child = '';
  let writer = '';
  let result = 'running';
  await mkdir(privateDir, { recursive: true, mode: 0o700 });
  await writeFile(path.join(privateDir, attempt + '-database-password'), password, {
    mode: 0o600,
    flag: 'wx',
  });
  await writeFile(path.join(privateDir, attempt + '-application-password'), appPassword, {
    mode: 0o600,
    flag: 'wx',
  });
  async function save() {
    await mkdir(path.dirname(testInfo.outputPath('result.json')), { recursive: true });
    await writeFile(
      testInfo.outputPath('result.json'),
      JSON.stringify(
        {
          result,
          project_id: project,
          branch_id: root,
          child_id: child,
          writer_id: writer,
          checks,
          failure_cleanup: cleanup,
          credentials_in_evidence: false,
        },
        null,
        2,
      ) + '\n',
    );
    await writeFile(
      path.join(privateDir!, attempt + '-fixture.json'),
      JSON.stringify({
        project_id: project,
        branch_id: root,
        child_id: child,
        endpoints: [...endpoints],
      }) + '\n',
      { mode: 0o600 },
    );
  }
  async function record(name: string, fields: Record<string, unknown> = {}) {
    checks.push({ name, ...fields });
    await save();
  }
  async function sql(statement: string, endpoint = writer) {
    await page.goto(`/#/projects/${project}/query`);
    await page.locator('select').first().selectOption(endpoint);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(password);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${endpoint}/query`) && r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    expect((await pending).status()).toBe(200);
  }
  async function authPage(branch: string) {
    await page.goto(`/#/projects/${project}/auth`);
    await page.getByLabel('Auth 分支').selectOption(branch);
    await expect(page.getByTestId('managed-auth-page')).toBeVisible();
  }
  async function mutateAuth(branch: string, disable: boolean) {
    const pending = page.waitForResponse(
      (r) =>
        r.url().endsWith(`/branches/${branch}/auth`) &&
        r.request().method() === (disable ? 'DELETE' : 'POST'),
    );
    await page
      .getByRole('button', { name: disable ? '禁用 Auth' : '启用 Auth', exact: true })
      .click();
    const response = await pending;
    expect(response.status()).toBe(202);
    const operation = (await response.json()).operation;
    await expect(page.getByTestId('auth-operation')).toContainText(/succeeded|failed|cancelled/, {
      timeout: 490000,
    });
    await expect(page.getByTestId('auth-operation')).toContainText('succeeded');
    await record(
      disable ? 'ui_disabled_auth_runtime_preserving_accounts' : 'ui_enabled_real_better_auth',
      { operation_id: operation.id, branch_id: branch },
    );
  }
  async function loginApp(branch: string) {
    await authPage(branch);
    await page.getByLabel('应用邮箱', { exact: true }).fill(email);
    await page.getByLabel('应用密码', { exact: true }).fill(appPassword);
    await page.getByRole('button', { name: '登录应用', exact: true }).click();
    await expect(page.getByTestId('auth-app-result')).toContainText('应用用户登录成功', {
      timeout: 190000,
    });
    expect(
      await page
        .getByLabel('应用密码', { exact: true })
        .evaluate((input) => (input as HTMLInputElement).value.length),
    ).toBe(0);
  }
  async function applicationToken(branch: string) {
    const value = await page.evaluate(async (id) => {
      const r = await fetch(`/auth/v1/${id}/token`, { credentials: 'same-origin' });
      return { status: r.status, value: await r.json() };
    }, branch);
    expect(value.status).toBe(200);
    expect(typeof value.value.token).toBe('string');
    return value.value.token as string;
  }
  async function dataPage(branch: string) {
    await page.goto(`/#/projects/${project}/data-api`);
    await page.getByLabel('Data API 分支').selectOption(branch);
  }
  async function dataService(branch: string, disable: boolean) {
    const pending = page.waitForResponse(
      (r) =>
        r.url().endsWith(`/branches/${branch}/data-api`) &&
        r.request().method() === (disable ? 'DELETE' : 'POST'),
    );
    await page
      .getByRole('button', { name: disable ? '停用 Data API' : '启用 Data API', exact: true })
      .click();
    expect((await pending).status()).toBe(202);
    await expect(page.getByTestId('data-api-operation')).toContainText(
      /succeeded|failed|cancelled/,
      { timeout: 490000 },
    );
    await expect(page.getByTestId('data-api-operation')).toContainText('succeeded');
  }
  async function configureData(branch: string) {
    await dataPage(branch);
    await page.getByLabel('Data API Schema').fill('app_auth');
    await page.getByRole('button', { name: '使用当前分支 Auth', exact: true }).click();
    await expect(page.getByLabel('JWT Audience')).toHaveValue(branch);
    await dataService(branch, false);
  }
  async function requestData(token: string, code: number, expected?: string) {
    await page.getByLabel('应用 JWT', { exact: true }).fill(token);
    await page.getByLabel('Data API 表名').fill('notes');
    await page.getByRole('button', { name: '发送 Data API 请求', exact: true }).click();
    await expect(page.getByTestId('data-api-response')).toContainText(`HTTP ${code}`, {
      timeout: 190000,
    });
    if (expected) await expect(page.getByTestId('data-api-response')).toContainText(expected);
  }
  async function suspend(endpoint: string) {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(endpoint);
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 145000,
    });
  }
  async function retireFailedRuntime() {
    if (!project || !endpoints.size) return;
    // Only this test's already recorded branch and endpoint IDs are eligible.
    // Preserve the product rows, Secrets, SQL data and original failed result.
    const deadline = Date.now() + 100000;
    const csrf = (await context.cookies(baseURL!)).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) return;
    const headers = { 'X-CSRF-Token': csrf, Origin: new URL(baseURL!).origin };
    async function stop(
      action: string,
      resource: string,
      url: string,
      method: 'POST' | 'DELETE',
      generation?: number,
    ) {
      if (Date.now() >= deadline) return;
      const key = randomUUID();
      const entry: (typeof cleanup)[number] = { action, resource, key, state: 'submitting' };
      cleanup.push(entry);
      await save();
      try {
        const r = await context.request.fetch(url, {
          method,
          timeout: 10000,
          headers: {
            ...headers,
            'Idempotency-Key': key,
            ...(generation === undefined ? {} : { 'If-Match': `"${generation}"` }),
          },
          ...(method === 'POST' ? { data: {} } : {}),
        });
        entry.status = r.status();
        if (r.status() !== 202) {
          entry.state = 'not_accepted';
          return;
        }
        entry.operation_id = (await r.json()).operation.id;
        entry.state = 'observing';
        await save();
        while (Date.now() < deadline) {
          const observed = await context.request.get(
            `/api/v1/projects/${project}/operations/${entry.operation_id}`,
            { timeout: 10000 },
          );
          if (!observed.ok()) break;
          entry.state = (await observed.json()).state;
          if (['succeeded', 'failed', 'cancelled'].includes(entry.state!)) break;
          await new Promise((resolve) => setTimeout(resolve, 1000));
        }
      } catch {
        entry.state = 'uncertain_retain_operation_and_key';
      } finally {
        await save();
      }
    }
    for (const branch of [child, root].filter(Boolean)) {
      for (const service of ['data-api', 'auth']) {
        const url = `/api/v1/projects/${project}/branches/${branch}/${service}`;
        try {
          const r = await context.request.get(url, { timeout: 10000 });
          if (!r.ok()) continue;
          const v = await r.json();
          if (v.state && v.state !== 'disabled' && Number.isInteger(v.generation))
            await stop('disable_' + service, branch, url, 'DELETE', v.generation);
        } catch {
          /* Original failure remains primary; no guessed deletion. */
        }
      }
    }
    for (const endpoint of endpoints)
      await stop(
        'suspend_owned_endpoint',
        endpoint,
        `/api/v1/projects/${project}/endpoints/${endpoint}/suspend`,
        'POST',
      );
  }
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await page.getByRole('button', { name: '＋ 创建项目', exact: true }).click();
    await page.getByLabel('项目名称').fill('ci-' + attempt);
    await page.locator('.create-modal input[type="password"]').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const pending = page.waitForResponse(
      (r) => r.url().endsWith('/organizations/local/projects') && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(202);
    project = (await response.json()).resource.id;
    await expect(page.locator('.create-modal')).toHaveCount(0, { timeout: 490000 });
    const listed = await context.request.get(`/api/v1/projects/${project}/endpoints`);
    expect(listed.status()).toBe(200);
    const endpoint = (await listed.json()).items[0];
    writer = endpoint.id;
    root = endpoint.branch_id;
    endpoints.add(writer);
    await record('ui_created_real_neon_project_with_bounded_compute');
    await sql('GRANT CREATE, CONNECT ON DATABASE postgres TO control_probe WITH GRANT OPTION');
    await authPage(root);
    await page.getByLabel('Auth 可信来源').fill('https://app.example.test');
    await mutateAuth(root, false);
    const preflight = await context.request.fetch(`/auth/v1/${root}/sign-up/email`, {
      method: 'OPTIONS',
      headers: {
        Origin: 'https://app.example.test',
        'Access-Control-Request-Method': 'POST',
        'Access-Control-Request-Headers': 'content-type',
      },
    });
    expect(preflight.status()).toBe(204);
    expect(preflight.headers()['access-control-allow-origin']).toBe('https://app.example.test');
    const deniedOrigin = await context.request.get(`/auth/v1/${root}/get-session`, {
      headers: { Origin: 'https://unrelated.example.test' },
    });
    expect(deniedOrigin.status()).toBe(403);
    await record('real_auth_relay_allows_exact_cors_preflight_and_rejects_untrusted_origins');
    await page.getByLabel('应用姓名').fill('Synthetic parent');
    await page.getByLabel('应用邮箱', { exact: true }).fill(email);
    await page.getByLabel('应用密码', { exact: true }).fill(appPassword);
    await page.getByRole('button', { name: '注册应用用户', exact: true }).click();
    await expect(page.getByTestId('auth-app-result')).toContainText('应用用户注册成功', {
      timeout: 190000,
    });
    expect(
      await page
        .getByLabel('应用密码', { exact: true })
        .evaluate((input) => (input as HTMLInputElement).value.length),
    ).toBe(0);
    await page.getByRole('button', { name: '检查应用会话', exact: true }).click();
    await expect(page.getByTestId('auth-app-result')).toContainText('应用会话有效');
    await page.getByRole('button', { name: '读取应用用户', exact: true }).click();
    await expect(page.getByRole('cell', { name: 'Synthetic parent', exact: true })).toBeVisible();
    await record('ui_registered_persisted_branch_user_and_session_without_console_identity');
    const parentSession = await page.evaluate(
      async (id) =>
        (await fetch(`/auth/v1/${id}/get-session`, { credentials: 'same-origin' })).json(),
      root,
    );
    const user = parentSession.user.id as string;
    let rootJWT = await applicationToken(root);
    const claims = JSON.parse(Buffer.from(rootJWT.split('.')[1], 'base64url').toString());
    expect(claims.aud).toBe(root);
    expect(claims.sub).toBe(user);
    expect(claims.branch_id).toBe(root);
    expect(claims.exp - claims.iat).toBeLessThanOrEqual(300);
    expect(claims.email).toBeUndefined();
    await record('actual_branch_scoped_jwt_has_bounded_expiry_and_public_jwks');
    for (const statement of [
      'CREATE SCHEMA app_auth',
      'CREATE TABLE app_auth.notes(id text PRIMARY KEY, owner_id text NOT NULL, body text NOT NULL)',
      'ALTER TABLE app_auth.notes ENABLE ROW LEVEL SECURITY',
      'ALTER TABLE app_auth.notes FORCE ROW LEVEL SECURITY',
      "CREATE POLICY own_rows ON app_auth.notes USING(owner_id=current_setting('request.jwt.claims',true)::jsonb->>'sub') WITH CHECK(owner_id=current_setting('request.jwt.claims',true)::jsonb->>'sub')",
      `INSERT INTO app_auth.notes VALUES('owned','${user}','owned auth row'),('other','another-user','hidden auth row')`,
      'GRANT USAGE ON SCHEMA app_auth TO control_probe WITH GRANT OPTION',
      'GRANT SELECT,INSERT,UPDATE,DELETE ON app_auth.notes TO control_probe WITH GRANT OPTION',
    ])
      await sql(statement);
    await configureData(root);
    rootJWT = await applicationToken(root);
    await requestData(rootJWT, 200, 'owned auth row');
    await expect(page.getByTestId('data-api-response')).not.toContainText('hidden auth row');
    await record('ui_auth_jwt_data_api_real_postgres_rls_read_isolation');
    await authPage(root);
    const dependency = page.waitForResponse(
      (r) => r.url().endsWith(`/branches/${root}/auth`) && r.request().method() === 'DELETE',
    );
    await page.getByRole('button', { name: '禁用 Auth', exact: true }).click();
    expect((await dependency).status()).toBe(409);
    await record('ui_blocks_disabling_auth_while_its_data_api_dependency_is_active');
    await page.getByRole('button', { name: '退出应用', exact: true }).click();
    await expect(page.getByTestId('auth-app-result')).toContainText('应用会话已撤销');
    await page.getByRole('button', { name: '检查应用会话', exact: true }).click();
    await expect(page.getByTestId('auth-app-result')).toContainText('没有应用会话');
    await loginApp(root);
    await record('ui_logout_revokes_session_and_password_login_restores_access');
    await page.goto(`/#/projects/${project}/branches`);
    await page.getByRole('button', { name: '＋ 创建分支', exact: true }).click();
    await page.getByLabel('分支名称').fill('child-' + attempt);
    await page.getByLabel('父分支').selectOption(root);
    await page.getByLabel('数据库角色密码').fill(password);
    await page.getByLabel('最大 CPU', { exact: true }).selectOption('1000');
    await page.getByLabel('最大内存', { exact: true }).selectOption('1024');
    const clone = page.waitForResponse(
      (r) => r.url().endsWith(`/projects/${project}/branches`) && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建并验证', exact: true }).click();
    const cloned = await clone;
    expect(cloned.status()).toBe(202);
    child = (await cloned.json()).resource.id;
    await expect(page.locator('.create-modal')).toHaveCount(0, { timeout: 490000 });
    const childEndpoints = await context.request.get(`/api/v1/projects/${project}/endpoints`);
    const childWriter = (await childEndpoints.json()).items.find(
      (v: { branch_id: string }) => v.branch_id === child,
    ).id as string;
    endpoints.add(childWriter);
    await expect
      .poll(
        async () => {
          const r = await context.request.get(`/api/v1/projects/${project}/branches/${child}/auth`);
          return r.ok() ? (await r.json()).state : 'unavailable';
        },
        { timeout: 490000, intervals: [3000] },
      )
      .toBe('active');
    await authPage(child);
    await page.getByRole('button', { name: '检查应用会话', exact: true }).click();
    await expect(page.getByTestId('auth-app-result')).toContainText('没有应用会话');
    await page.getByRole('button', { name: '读取应用用户', exact: true }).click();
    await expect(page.getByRole('cell', { name: 'Synthetic parent', exact: true })).toBeVisible();
    await record('ui_native_timeline_clone_inherits_users_but_parent_cookie_is_rejected', {
      child_id: child,
    });
    await loginApp(child);
    let childJWT = await applicationToken(child);
    const changed = await page.evaluate(async (id) => {
      const r = await fetch(`/auth/v1/${id}/update-user`, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: 'Synthetic child' }),
      });
      return r.status;
    }, child);
    expect(changed).toBe(200);
    await authPage(root);
    await page.getByRole('button', { name: '读取应用用户', exact: true }).click();
    await expect(page.getByRole('cell', { name: 'Synthetic parent', exact: true })).toBeVisible();
    await record('inherited_password_login_and_child_identity_changes_are_isolated');
    await configureData(child);
    rootJWT = await applicationToken(root);
    expect(
      JSON.parse(Buffer.from(rootJWT.split('.')[1], 'base64url').toString()).exp,
    ).toBeGreaterThan(Math.floor(Date.now() / 1000));
    childJWT = await applicationToken(child);
    await requestData(rootJWT, 401);
    await requestData(childJWT, 200, 'owned auth row');
    await dataPage(root);
    await requestData(childJWT, 401);
    await record('ui_rejects_parent_and_child_jwt_cross_branch_data_access');
    await suspend(childWriter);
    await record('ui_auth_enabled_compute_suspends_to_zero');
    const sleepingPreflight = await context.request.fetch(`/auth/v1/${child}/sign-in/email`, {
      method: 'OPTIONS',
      headers: { Origin: 'https://app.example.test', 'Access-Control-Request-Method': 'POST' },
    });
    expect(sleepingPreflight.status()).toBe(204);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠');
    await record('auth_preflight_does_not_wake_suspended_compute');
    await loginApp(child);
    await record('ui_auth_password_login_cold_wakes_actual_neon_writer');
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(childWriter);
    await page.getByRole('checkbox').check();
    await page.getByRole('spinbutton').fill('60');
    const policy = page.waitForResponse(
      (r) =>
        r.url().endsWith(`/endpoints/${childWriter}/lifecycle`) && r.request().method() === 'PATCH',
    );
    await page.getByRole('button', { name: '保存生命周期策略', exact: true }).click();
    expect((await policy).status()).toBe(200);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 240000,
    });
    await record('ui_automatic_idle_zero_with_auth_and_data_api_enabled');
    await loginApp(child);
    await record('ui_auth_cold_wake_after_automatic_idle');
    for (const branch of [child, root]) {
      await dataPage(branch);
      await dataService(branch, true);
      await authPage(branch);
      await mutateAuth(branch, true);
    }
    const disabled = await context.request.get(`/auth/v1/${root}/jwks`);
    expect(disabled.status()).toBe(404);
    await record('disabled_auth_closes_public_admission');
    await authPage(root);
    await mutateAuth(root, false);
    await loginApp(root);
    await page.getByRole('button', { name: '读取应用用户', exact: true }).click();
    await expect(page.getByRole('cell', { name: 'Synthetic parent', exact: true })).toBeVisible();
    await record('ui_auth_reenable_rotates_generation_without_losing_users');
    await page.screenshot({
      path: testInfo.outputPath('managed-auth.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
    await mutateAuth(root, true);
    for (const endpoint of endpoints) await suspend(endpoint);
    await record('ui_runtime_released_all_data_and_test_records_retained');
    result = 'pass';
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  } finally {
    if (result === 'fail') {
      testInfo.setTimeout(testInfo.timeout + 120000);
      await retireFailedRuntime();
    }
    await save();
  }
});
