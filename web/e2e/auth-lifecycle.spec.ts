import { expect, test } from '@playwright/test';
import { randomBytes } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

test('Auth retained lifecycle: leaf deletion, project recovery and explicit new issuer', async ({
  page,
  context,
  baseURL,
}, info) => {
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  if (!privateDir || !adminFile || baseURL !== 'http://192.168.146.100:30788')
    throw Error('Exact protected lab inputs required');
  page.setDefaultTimeout(30000);
  page.setDefaultNavigationTimeout(60000);
  const fixtureFile = process.env.NEON_E2E_SQL_SCRIPT_FIXTURE;
  const fixture = fixtureFile ? JSON.parse(await readFile(fixtureFile, 'utf8')) : {};
  const state: any = fixture.resume_file
    ? JSON.parse(await readFile(fixture.resume_file, 'utf8'))
    : {
        nonce: String(Date.now()),
        checks: [],
        operations: {},
        requests: {},
        running: '',
        platform_password: randomBytes(30).toString('base64url'),
        password: randomBytes(30).toString('base64url'),
      };
  if (state.running || (state.project_id && !/^prj_[0-9a-f]{16}$/.test(state.project_id)))
    throw Error('Inspect the original pending mutation before resuming');
  state.project_name ??= 'ci-auth-lifecycle-' + state.nonce;
  state.email ??= 'auth-lifecycle-' + state.nonce + '@example.test';
  let result = 'running';
  const base = () => '/api/v1/projects/' + state.project_id;
  const authPath = (branch = state.branch_id) => base() + '/branches/' + branch + '/auth';
  const bounds = {
    min_cpu_milli: 1000,
    max_cpu_milli: 1000,
    min_memory_mib: 1024,
    max_memory_mib: 1024,
  };
  await mkdir(privateDir, { recursive: true });
  await mkdir(path.dirname(info.outputPath('result.json')), { recursive: true });
  async function save() {
    await writeFile(
      path.join(privateDir!, 'auth-lifecycle-checkpoint.private.json'),
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
        child_branch_id: state.child_branch_id,
        child_writer_id: state.child_writer_id,
        operations: state.operations,
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
  async function get(url: string) {
    const response = await context.request.get(url, { timeout: 190000 });
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function observe(id: string) {
    await expect
      .poll(
        async () => {
          const op = await get(base() + '/operations/' + id);
          if (['failed', 'cancelled'].includes(op.state))
            throw Error('Inspect original operation: ' + id + ' ' + op.error_code);
          return op.state;
        },
        { timeout: 490000, intervals: [2000, 5000] },
      )
      .toBe('succeeded');
  }
  async function mutate(url: string, label: string, data: object) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return get(base() + '/operations/' + state.operations[label]);
    }
    const csrf = (await context.cookies()).find((c) => c.name === 'neon_v2_csrf')?.value;
    if (!csrf) throw Error('Console CSRF required');
    state.requests[label] ??= { url, data, key: 'auth-lifecycle-' + state.nonce + '-' + label };
    await save();
    const request = state.requests[label];
    const response = await context.request.post(request.url, {
      headers: { 'X-CSRF-Token': csrf, 'Idempotency-Key': request.key },
      data: request.data,
      timeout: 190000,
    });
    expect(response.status()).toBe(202);
    const value = await response.json();
    state.operations[label] = value.operation.id;
    if (label === 'create-project') state.project_id = value.resource.id;
    await save();
    await observe(value.operation.id);
    return get(base() + '/operations/' + value.operation.id);
  }
  async function visitAuth(branch = state.branch_id) {
    await page.goto('/?auth_branch=' + branch + '#/projects/' + state.project_id + '/auth');
    await expect(page.getByLabel('Auth 分支', { exact: true })).toHaveValue(branch);
  }
  async function authUI(label: string, disable = false, branch = state.branch_id) {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return;
    }
    await visitAuth(branch);
    if (!disable) await page.getByLabel('Auth 可信来源').fill(baseURL!);
    const waiting = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === authPath(branch) &&
        r.request().method() === (disable ? 'DELETE' : 'POST'),
    );
    await page
      .getByRole('button', { name: disable ? '禁用 Auth' : '启用 Auth', exact: true })
      .click();
    const response = await waiting;
    expect(response.status()).toBe(202);
    state.operations[label] = (await response.json()).operation.id;
    await save();
    await observe(state.operations[label]);
    await expect(page.getByTestId('auth-operation')).toContainText('succeeded', {
      timeout: 490000,
    });
  }
  async function app(action: string, branch = state.branch_id, data?: object) {
    const response = await context.request.fetch('/auth/v1/' + branch + '/' + action, {
      method: data === undefined ? 'GET' : 'POST',
      headers: { Origin: baseURL! },
      ...(data === undefined ? {} : { data }),
      timeout: 190000,
    });
    return { status: response.status(), value: await response.json().catch(() => null) };
  }
  async function sql(statement: string) {
    await page.goto('/#/projects/' + state.project_id + '/query');
    await page.locator('select').first().selectOption(state.writer_id);
    await page.getByLabel('SQL 查询').fill(statement);
    await page.locator('input[type="password"]').fill(state.platform_password);
    const waiting = page.waitForResponse(
      (r) =>
        r.url().endsWith('/endpoints/' + state.writer_id + '/query') &&
        r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const response = await waiting;
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function lifecycle() {
    await page.goto('/#/projects/' + state.project_id + '/lifecycle');
    await expect(page.getByTestId('project-lifecycle')).toBeVisible();
  }
  async function lifecycleUI(label: string, button: string, name: string, branch = '') {
    if (state.operations[label]) {
      await observe(state.operations[label]);
      return;
    }
    await lifecycle();
    const target = branch
      ? page.getByTestId('lifecycle-' + branch)
      : page.getByTestId('project-lifecycle');
    await target.getByRole('button', { name: button, exact: true }).click();
    await page.getByLabel('资源名称确认').fill(name);
    const waiting = page.waitForResponse(
      (r) =>
        r.request().method() !== 'GET' &&
        new URL(r.url()).pathname.startsWith(base()) &&
        !r.url().includes('/operations/'),
    );
    await page.getByRole('button', { name: '确认执行', exact: true }).click();
    const response = await waiting;
    expect([200, 202]).toContain(response.status());
    if (response.status() === 202) {
      state.operations[label] = (await response.json()).operation.id;
      await save();
      await observe(state.operations[label]);
    }
    await expect(page.locator('.lifecycle-modal')).not.toBeVisible({ timeout: 490000 });
  }
  async function passwordLogin(branch = state.branch_id) {
    await visitAuth(branch);
    await page.getByLabel('应用邮箱', { exact: true }).fill(state.email);
    await page.getByLabel('应用密码', { exact: true }).fill(state.password);
    const waiting = page.waitForResponse((r) =>
      r.url().endsWith('/auth/v1/' + branch + '/sign-in/email'),
    );
    await page.getByRole('button', { name: '登录应用', exact: true }).click();
    expect((await waiting).status()).toBe(200);
    await expect(page.getByTestId('auth-app-result')).toContainText('应用用户登录成功');
    const current = await app('get-session', branch);
    expect(current.status).toBe(200);
    expect(current.value.user.id).toBe(state.user_id);
  }
  try {
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    if (
      state.checks.includes('UI_Auth_account_session_and_JWKS_before_deletion') &&
      !state.checks.includes(
        'UI_explicit_new_generation_rejects_old_session_and_preserves_user_password_and_SQL',
      )
    ) {
      expect(state.original_cookie_values).toHaveLength(1);
      await context.addCookies([
        {
          name: 'neon_app_' + state.branch_id + '.session_token',
          value: state.original_cookie_values[0],
          domain: new URL(baseURL).hostname,
          path: '/auth/v1/' + state.branch_id,
          httpOnly: true,
          secure: false,
          sameSite: 'Lax',
        },
      ]);
    }
    await step('new_owned_project_and_durable_SQL_fixture', async () => {
      await mutate('/api/v1/organizations/local/projects', 'create-project', {
        name: state.project_name,
        password: state.platform_password,
        autoscaling: bounds,
      });
      const endpoints = await get(base() + '/endpoints');
      expect(endpoints.items).toHaveLength(1);
      state.writer_id = endpoints.items[0].id;
      state.branch_id = endpoints.items[0].branch_id;
      await save();
      await sql('GRANT CREATE, CONNECT ON DATABASE postgres TO control_probe WITH GRANT OPTION');
      await sql(
        "CREATE TABLE public.auth_lifecycle_probe AS SELECT 1 AS id,'retained-auth-lifecycle'::text AS marker",
      );
    });
    await step('UI_Auth_account_session_and_JWKS_before_deletion', async () => {
      await authUI('enable-root');
      await visitAuth();
      await page.getByLabel('应用姓名').fill('Owned lifecycle user');
      await page.getByLabel('应用邮箱', { exact: true }).fill(state.email);
      await page.getByLabel('应用密码', { exact: true }).fill(state.password);
      const waiting = page.waitForResponse((r) =>
        r.url().endsWith('/auth/v1/' + state.branch_id + '/sign-up/email'),
      );
      await page.getByRole('button', { name: '注册应用用户', exact: true }).click();
      expect((await waiting).status()).toBe(200);
      const current = await app('get-session');
      expect(current.status).toBe(200);
      state.user_id = current.value.user.id;
      state.original_session_token = current.value.session.token;
      state.original_cookie_values = (await context.cookies())
        .filter((c) => c.name.startsWith('neon_app_'))
        .map((c) => c.value);
      const jwt = await app('token');
      expect(jwt.status).toBe(200);
      state.original_JWT = jwt.value.token;
      const keys = await app('jwks');
      expect(keys.status).toBe(200);
      state.original_jwk_kids = keys.value.keys.map((k: any) => k.kid);
      expect(state.original_jwk_kids.length).toBeGreaterThan(0);
      state.original_auth_spec = (await get(authPath())).spec;
      await save();
    });
    await step('owned_physical_child_uses_inherited_account', async () => {
      const created = await mutate(base() + '/branches', 'create-child', {
        name: 'auth-lifecycle-child-' + state.nonce,
        parent_branch_id: state.branch_id,
        password: state.platform_password,
        function_copy_policy: 'omit',
        autoscaling: bounds,
      });
      state.child_branch_id = created.resource_id;
      state.child_writer_id = (await get(base() + '/endpoints')).items.find(
        (e: any) => e.branch_id === state.child_branch_id && e.endpoint_type === 'read_write',
      )?.id;
      expect(state.child_writer_id).toBeTruthy();
      await save();
      const inherited = await get(authPath(state.child_branch_id));
      expect(inherited.state).toBe('active');
      expect(inherited.generation).toBe(1);
      expect(inherited.spec).toEqual(state.original_auth_spec);
      await passwordLogin(state.child_branch_id);
    });
    await step('UI_leaf_delete_closes_child_Auth_and_preserves_parent_session', async () => {
      await lifecycleUI(
        'delete-child',
        '删除分支',
        'auth-lifecycle-child-' + state.nonce,
        state.child_branch_id,
      );
      for (const action of ['get-session', 'jwks'])
        expect((await app(action, state.child_branch_id)).status).toBe(404);
      const parent = await app('get-session');
      expect(parent.status).toBe(200);
      expect(parent.value.user.id).toBe(state.user_id);
      expect(parent.value.session.token).toBe(state.original_session_token);
    });
    await step('UI_project_delete_retires_active_Auth_and_closes_original_cookie', async () => {
      const current = await get(base() + '/lifecycle');
      if (current.project.protected)
        await lifecycleUI('unprotect-project', '解除项目保护', state.project_name);
      const root = current.branches.find((b: any) => b.id === state.branch_id);
      if (root.protected)
        await lifecycleUI('unprotect-root', '解除分支保护', root.name, state.branch_id);
      await lifecycleUI('delete-project', '删除项目', state.project_name);
      for (const action of ['get-session', 'jwks', 'token'])
        expect((await app(action)).status).toBe(404);
    });
    await step('UI_retained_recovery_keeps_Auth_disabled_and_prior_leaf_deleted', async () => {
      await lifecycleUI('recover-project', '恢复项目', state.project_name);
      const auth = await get(authPath());
      expect(auth.state).toBe('disabled');
      expect(auth.generation).toBe(1);
      expect(auth.spec).toEqual(state.original_auth_spec);
      for (const action of ['get-session', 'jwks', 'token'])
        expect((await app(action)).status).toBe(404);
      expect(
        (await context.request.get(base() + '/branches/' + state.child_branch_id)).status(),
      ).toBe(404);
      expect((await app('get-session', state.child_branch_id)).status).toBe(404);
    });
    await step(
      'UI_explicit_new_generation_rejects_old_session_and_preserves_user_password_and_SQL',
      async () => {
        await authUI('reenable-root');
        const auth = await get(authPath());
        expect(auth.state).toBe('active');
        expect(auth.generation).toBe(2);
        const oldSession = await app('get-session');
        expect(oldSession.status).toBe(200);
        expect(oldSession.value).toBeNull();
        const keys = await app('jwks');
        expect(keys.status).toBe(200);
        expect(keys.value.keys.length).toBeGreaterThan(0);
        expect(keys.value.keys.every((k: any) => !state.original_jwk_kids.includes(k.kid))).toBe(
          true,
        );
        await passwordLogin();
        expect((await app('get-session')).value.session.token).not.toBe(
          state.original_session_token,
        );
        expect(
          JSON.stringify(await sql('SELECT marker FROM public.auth_lifecycle_probe WHERE id=1')),
        ).toContain('retained-auth-lifecycle');
      },
    );
    await step('owned_Auth_disabled_compute_zero_and_deletion_history_retained', async () => {
      await authUI('final-disable', true);
      if (!state.operations['final-suspend'])
        await mutate(base() + '/endpoints/' + state.writer_id + '/suspend', 'final-suspend', {});
      else await observe(state.operations['final-suspend']);
      expect((await get(authPath())).state).toBe('disabled');
      const lifecycle = await get(base() + '/lifecycle');
      expect(lifecycle.project.state).toBe('ready');
      expect(
        lifecycle.tombstones.some(
          (t: any) =>
            t.resource_id === state.child_branch_id &&
            !t.restored_at &&
            t.physical_gc_state === 'held',
        ),
      ).toBe(true);
      expect(
        lifecycle.tombstones.some(
          (t: any) =>
            t.resource_id === state.project_id && t.restored_at && t.physical_gc_state === 'held',
        ),
      ).toBe(true);
    });
    result = 'pass';
  } catch (error) {
    result = 'fail';
    throw error;
  } finally {
    await save();
  }
});
