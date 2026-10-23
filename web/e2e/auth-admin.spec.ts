import { expect, test, type BrowserContext } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import path from 'node:path';

test('Application administrators: explicit grant, current roles, bans, sessions and UI impersonation', async ({
  page,
  context,
  browser,
  baseURL,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR,
    adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!fixtureFile || !privateDir || !adminFile || baseURL !== 'http://192.168.146.100:30788')
    throw Error('Protected exact lab fixture required');
  const f = JSON.parse(await readFile(fixtureFile, 'utf8'));
  if (
    f.project_id !== 'prj_6f3c51bd15b772e3' ||
    f.branch_id !== 'br_6f3c51bd15b772e3' ||
    f.database !== 'owner_transfer_v1'
  )
    throw Error('Retained source fixture required');
  const state: any = f.resume_file
    ? JSON.parse(await readFile(f.resume_file, 'utf8'))
    : {
        project_id: f.project_id,
        source_branch_id: f.branch_id,
        nonce: String(Date.now()),
        checks: [],
        running: '',
        operations: {},
        requests: {},
        password: randomBytes(30).toString('base64url'),
        member_password: randomBytes(30).toString('base64url'),
        managed_password: randomBytes(30).toString('base64url'),
        platform_password: randomBytes(30).toString('base64url'),
        users: {},
      };
  if (state.project_id !== f.project_id || state.source_branch_id !== f.branch_id || state.running)
    throw Error('Inspect original pending mutation before resuming');
  const root = '/api/v1/projects/' + f.project_id,
    authPath = () => root + '/branches/' + state.branch_id + '/auth',
    appPath = () => '/auth/v1/' + state.branch_id;
  let result = 'running',
    lastApp = 0;
  const extra: BrowserContext[] = [];
  await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
  async function save() {
    await writeFile(
      path.join(privateDir!, 'auth-admin-checkpoint.private.json'),
      JSON.stringify(state),
      { mode: 0o600 },
    );
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        checks: state.checks,
        project_id: state.project_id,
        branch_id: state.branch_id,
        writer_id: state.writer_id,
        operations: state.operations,
        root_auth_unchanged: state.root_auth_unchanged || false,
        credentials_exported: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function step(name: string, work: () => Promise<void>) {
    if (state.checks.includes(name)) return;
    state.running = name;
    await save();
    await work();
    state.checks.push(name);
    state.running = '';
    await save();
  }
  async function headers() {
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    return { 'X-CSRF-Token': csrf };
  }
  async function get(url: string) {
    const response = await context.request.get(url, { timeout: 190000 });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function observe(id: string) {
    await expect
      .poll(
        async () => {
          const op = await get(root + '/operations/' + id);
          if (['failed', 'cancelled'].includes(op.state))
            throw Error('Original operation failed: ' + id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function mutate(
    url: string,
    label: string,
    data?: object,
    version?: number,
    method = 'POST',
  ) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return get(root + '/operations/' + state.operations[label]);
    }
    state.requests[label] ??= {
      url,
      method,
      data,
      version,
      key: 'a03-admin-' + state.nonce + '-' + label,
    };
    await save();
    const request = state.requests[label];
    const response = await context.request.fetch(request.url, {
      method: request.method,
      headers: {
        ...(await headers()),
        'Idempotency-Key': request.key,
        ...(request.version === undefined ? {} : { 'If-Match': `"${request.version}"` }),
      },
      ...(request.data === undefined ? {} : { data: request.data }),
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    const value = await response.json();
    state.operations[label] = value.operation.id;
    await save();
    await observe(value.operation.id);
    return get(root + '/operations/' + value.operation.id);
  }
  async function visit() {
    await page.goto('/?auth_branch=' + state.branch_id + '#/projects/' + f.project_id + '/auth');
    await expect(page.getByLabel('Auth 分支', { exact: true })).toHaveValue(state.branch_id);
  }
  async function enableUI(label: string, admin: boolean) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      await visit();
      return;
    }
    await visit();
    await page.getByRole('button', { name: '刷新 Auth 状态', exact: true }).click();
    await expect(page.getByRole('button', { name: '启用 Auth', exact: true })).toBeEnabled();
    await page.getByLabel('Auth 数据库', { exact: true }).fill(f.database);
    await page.getByLabel('启用应用 TOTP 双重认证', { exact: true }).check();
    await page.getByLabel('迁移父分支双重认证', { exact: true }).check();
    await page.getByLabel('启用应用管理员接口', { exact: true }).setChecked(admin);
    if (admin)
      await page.getByLabel('应用管理员授权用户 ID', { exact: true }).fill(state.users.admin.id);
    await page.route('**' + authPath(), async (route) => {
      if (route.request().method() === 'POST') {
        const req = route.request();
        state.requests[label] = {
          url: authPath(),
          method: 'POST',
          data: JSON.parse(req.postData()!),
          version: Number(req.headers()['if-match'].replaceAll('"', '')),
          key: req.headers()['idempotency-key'],
        };
        await save();
      }
      await route.continue();
    });
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === authPath() && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '启用 Auth', exact: true }).click();
    const response = await waiting;
    expect(response.status()).toBe(202);
    const value = await response.json();
    state.operations[label] = value.operation.id;
    await save();
    await page.unroute('**' + authPath());
    await observe(value.operation.id);
    await expect(page.getByRole('button', { name: '禁用 Auth', exact: true })).toBeEnabled();
  }
  // UI writes also refresh a list. Stay inside the existing 30/minute limiter.
  async function pace() {
    const delay = lastApp + 4250 - Date.now();
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    lastApp = Date.now();
  }
  async function app(action: string, data?: object, client = context) {
    await pace();
    if (action === 'sign-in/email') {
      // The maintained library resets this special bucket after ten idle
      // seconds. Keep its limit; never retry an already committed admin write.
      if (Date.now() - (state.last_login_at || 0) >= 10500) state.login_burst_count = 0;
      if ((state.login_burst_count || 0) >= 3) {
        const delay = (state.last_login_at || 0) + 10500 - Date.now();
        if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
        state.login_burst_count = 0;
      }
      state.last_login_at = Date.now();
      state.login_burst_count = (state.login_burst_count || 0) + 1;
      await save();
      lastApp = Date.now();
    }
    const response = await client.request.fetch(appPath() + '/' + action, {
      method: data === undefined ? 'GET' : 'POST',
      headers: { Origin: baseURL! },
      ...(data === undefined ? {} : { data }),
      timeout: 190000,
    });
    const value = await response.json().catch(() => null);
    return { status: response.status(), value };
  }
  async function signIn(client = context, kind = 'admin') {
    const user = state.users[kind],
      password = kind === 'admin' ? state.password : state.member_password;
    expect((await app('sign-in/email', { email: user.email, password }, client)).status).toBe(200);
  }
  function panel() {
    return page.getByRole('region', { name: '应用管理员', exact: true });
  }
  async function press(name: string, action: string) {
    await pace();
    const waiting = page.waitForResponse(
      (r) => new URL(r.url()).pathname === appPath() + '/admin/' + action,
    );
    await panel().getByRole('button', { name, exact: true }).click();
    return waiting;
  }
  async function shot(name: string) {
    await page.screenshot({
      path: info.outputPath(name + '.png'),
      fullPage: true,
      mask: [page.locator('input[type="password"]')],
    });
  }
  try {
    await save();
    page.setDefaultTimeout(30000);
    await page.goto('/#/projects');
    await page.getByLabel('用户名', { exact: true }).fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (
      state.checks.includes('UI_explicit_existing_user_admin_grant_current_generation') &&
      !state.checks.includes('disabled_admin_management_preserves_bans_and_root_intent')
    ) {
      await signIn();
      await visit();
      expect((await press('读取管理员用户列表', 'list-users')).status()).toBe(200);
    }
    await step('one_owned_branch_created_root_Auth_intent_preserved', async () => {
      state.original_root_auth = await get(root + '/branches/' + f.branch_id + '/auth');
      expect(state.original_root_auth.state).toBe('disabled');
      const op = await mutate(root + '/branches', 'create-branch', {
        name: 'ci-auth-admin-' + state.nonce,
        parent_branch_id: f.branch_id,
        password: state.platform_password,
        function_copy_policy: 'omit',
        autoscaling: {
          min_cpu_milli: 1000,
          max_cpu_milli: 1000,
          min_memory_mib: 1024,
          max_memory_mib: 1024,
        },
      });
      state.branch_id = op.resource_id;
      state.writer_id = (await get(root + '/endpoints')).items.find(
        (e: any) => e.branch_id === state.branch_id && e.endpoint_type === 'read_write',
      )?.id;
      if (!state.writer_id) throw Error('Owned writer required');
    });
    await step('UI_admin_disabled_registration_fixture_and_default_user_role', async () => {
      await enableUI('enable-base', false);
      for (const kind of ['admin', 'member']) {
        if (state.users[kind]) continue;
        const email = 'a03-' + kind + '-' + state.nonce + '@example.test',
          password = kind === 'admin' ? state.password : state.member_password;
        const response = await app('sign-up/email', { email, password, name: 'Owned app ' + kind });
        expect(response.status).toBe(200);
        expect(response.value.user.role).toBe('user');
        state.users[kind] = { id: response.value.user.id, email };
        await save();
      }
      expect((await app('admin/list-users')).status).toBe(404);
    });
    await step('UI_explicit_existing_user_admin_grant_current_generation', async () => {
      await mutate(
        authPath(),
        'disable-base',
        undefined,
        (await get(authPath())).generation,
        'DELETE',
      );
      await enableUI('enable-admin', true);
      await signIn();
      const session = await app('get-session');
      expect(session.value.user.id).toBe(state.users.admin.id);
      expect(session.value.user.role).toBe('admin');
      await visit();
      expect((await press('读取管理员用户列表', 'list-users')).status()).toBe(200);
      await expect(panel().getByLabel('管理员目标用户')).toContainText(state.users.member.email);
      await shot('admin-policy');
    });
    await step('regular_application_user_and_Console_only_cookie_cannot_administer', async () => {
      const client = await browser.newContext({ baseURL });
      extra.push(client);
      await signIn(client, 'member');
      expect((await app('admin/list-users', undefined, client)).status).toBe(403);
      const consoleOnly = await browser.newContext({ baseURL });
      extra.push(consoleOnly);
      for (const cookie of await context.cookies())
        if (cookie.name.startsWith('neon_v2_')) await consoleOnly.addCookies([cookie]);
      expect((await app('admin/list-users', undefined, consoleOnly)).status).toBe(401);
    });
    await step('UI_create_and_update_only_owned_application_user', async () => {
      await visit();
      expect((await press('读取管理员用户列表', 'list-users')).status()).toBe(200);
      const p = panel();
      await p.getByLabel('管理员用户姓名', { exact: true }).fill('Owned managed account');
      await p
        .getByLabel('管理员用户邮箱', { exact: true })
        .fill('a03-managed-' + state.nonce + '@example.test');
      await p.getByLabel('管理员用户新密码', { exact: true }).fill(state.managed_password);
      const response = await press('创建应用用户', 'create-user');
      expect(response.status()).toBe(200);
      const value = await response.json();
      state.users.managed = { id: value.user.id, email: value.user.email };
      await save();
      await p.getByLabel('管理员目标用户').selectOption(value.user.id);
      await p.getByLabel('管理员用户姓名').fill('Updated owned account');
      expect((await press('更新应用姓名', 'update-user')).status()).toBe(200);
      const user = await app('admin/get-user?id=' + value.user.id);
      expect(user.value.name).toBe('Updated owned account');
    });
    await step('UI_role_grants_and_admin_impersonation_protection', async () => {
      const p = panel();
      await p.getByLabel('管理员目标用户').selectOption(state.users.managed.id);
      await p.getByLabel('管理员应用角色').selectOption('admin');
      expect((await press('设置应用角色', 'set-role')).status()).toBe(200);
      expect((await app('admin/impersonate-user', { userId: state.users.managed.id })).status).toBe(
        403,
      );
      await p.getByLabel('管理员应用角色').selectOption('user');
      expect((await press('设置应用角色', 'set-role')).status()).toBe(200);
    });
    await step('UI_impersonation_cookie_is_branch_scoped_and_restores_original_admin', async () => {
      const p = panel();
      await p.getByLabel('管理员目标用户').selectOption(state.users.member.id);
      const response = await press('开始身份代入（最多一小时）', 'impersonate-user');
      expect(response.status()).toBe(200);
      const value = await response.json();
      expect(value.session.impersonatedBy).toBe(state.users.admin.id);
      expect(new Date(value.session.expiresAt).getTime()).toBeLessThanOrEqual(Date.now() + 3602000);
      expect((await app('get-session')).value.user.id).toBe(state.users.member.id);
      expect((await app('admin/list-users')).status).toBe(403);
      expect((await press('结束身份代入', 'stop-impersonating')).status()).toBe(200);
      expect((await app('get-session')).value.user.id).toBe(state.users.admin.id);
      await shot('admin-impersonation-restored');
    });
    await step('UI_revoke_owned_sessions_and_ban_unban_controls', async () => {
      const client = await browser.newContext({ baseURL });
      extra.push(client);
      await signIn(client, 'member');
      const p = panel();
      await p.getByLabel('管理员目标用户').selectOption(state.users.member.id);
      expect((await press('撤销目标全部会话', 'revoke-user-sessions')).status()).toBe(200);
      expect((await app('get-session', undefined, client)).value).toBeNull();
      await signIn(client, 'member');
      await p.getByLabel('管理员封禁原因').fill('Owned functional case');
      expect((await press('封禁应用用户', 'ban-user')).status()).toBe(200);
      expect((await app('get-session', undefined, client)).value).toBeNull();
      expect(
        (
          await app(
            'sign-in/email',
            { email: state.users.member.email, password: state.member_password },
            client,
          )
        ).status,
      ).toBe(403);
      expect((await press('解除应用封禁', 'unban-user')).status()).toBe(200);
      await signIn(client, 'member');
    });
    await step('UI_password_change_and_explicit_owned_user_removal', async () => {
      const p = panel();
      await p.getByLabel('管理员目标用户').selectOption(state.users.managed.id);
      if (!state.managed_password_changed) {
        await p.getByLabel('管理员用户新密码').fill(state.member_password);
        expect((await press('设置应用密码', 'set-user-password')).status()).toBe(200);
        state.managed_password_changed = true;
        await save();
      }
      const client = await browser.newContext({ baseURL });
      extra.push(client);
      if (!state.old_managed_password_rejected)
        expect(
          (
            await app(
              'sign-in/email',
              { email: state.users.managed.email, password: state.managed_password },
              client,
            )
          ).status,
        ).toBe(401);
      state.old_managed_password_rejected = true;
      await save();
      expect(
        (
          await app(
            'sign-in/email',
            { email: state.users.managed.email, password: state.member_password },
            client,
          )
        ).status,
      ).toBe(200);
      await p.getByLabel('确认删除目标应用用户').check();
      expect((await press('删除目标应用用户', 'remove-user')).status()).toBe(200);
      expect((await app('get-session', undefined, client)).value).toBeNull();
      expect((await app('admin/get-user?id=' + state.users.managed.id)).status).toBe(404);
    });
    await step('disabled_admin_management_preserves_bans_and_root_intent', async () => {
      expect(
        (
          await app('admin/ban-user', {
            userId: state.users.member.id,
            banReason: 'Owned retained disabled case',
          })
        ).status,
      ).toBe(200);
      await mutate(
        authPath(),
        'disable-admin',
        undefined,
        (await get(authPath())).generation,
        'DELETE',
      );
      await enableUI('enable-without-admin', false);
      expect((await app('admin/list-users')).status).toBe(404);
      expect(
        (
          await app('sign-in/email', {
            email: state.users.member.email,
            password: state.member_password,
          })
        ).status,
      ).toBe(403);
      await mutate(
        authPath(),
        'final-disable',
        undefined,
        (await get(authPath())).generation,
        'DELETE',
      );
      const source = await get(root + '/branches/' + f.branch_id + '/auth');
      expect({ generation: source.generation, state: source.state, spec: source.spec }).toEqual({
        generation: state.original_root_auth.generation,
        state: state.original_root_auth.state,
        spec: state.original_root_auth.spec,
      });
      state.root_auth_unchanged = true;
    });
    await step('owned_Auth_disabled_and_writer_returned_to_zero', async () => {
      expect((await get(authPath())).state).toBe('disabled');
      await mutate(root + '/endpoints/' + state.writer_id + '/suspend', 'suspend', {});
      await expect
        .poll(async () => (await get(root + '/endpoints/' + state.writer_id)).observed_state, {
          timeout: 180000,
          intervals: [2000, 5000],
        })
        .toBe('suspended');
    });
    result = 'pass';
    await save();
  } finally {
    await save();
    for (const client of extra) await client.close();
  }
});
