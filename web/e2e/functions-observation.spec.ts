import { expect, test, type Response } from '@playwright/test';
import { mkdir, readFile, writeFile } from 'node:fs/promises';

// Focused new monitoring slice on an explicitly retained owner fixture. No
// table rebuild, new grants, DML, source route mock or old full-suite replay.
test('Functions runtime monitoring: original identity, activity, stop and no SQL wake', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE;
  const adminFile = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const zipFile = process.env.NEON_E2E_FUNCTION_ZIP;
  const privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!fixtureFile || !adminFile || !zipFile || !privateDir)
    throw new Error('Protected Linux monitoring inputs required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    database_password_file: string;
    resume?: {
      function_id: string;
      checks: string[];
      operations: string[];
    };
  };
  if (
    !/^prj_[0-9a-f]{16}$/.test(fixture.project_id) ||
    !/^br_[0-9a-f]{16}$/.test(fixture.branch_id) ||
    !/^ep_[0-9a-f]{16}$/.test(fixture.writer_id)
  )
    throw new Error('Exact retained owner scope required');
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  const password = (await readFile(fixture.database_password_file, 'utf8')).trim();
  const base = `/api/v1/projects/${project}/branches/${branch}/functions`;
  const checks: string[] = [...(fixture.resume?.checks ?? [])],
    operations: string[] = [...(fixture.resume?.operations ?? [])];
  let result = 'running',
    functionID = fixture.resume?.function_id ?? '';
  if (fixture.resume && (!/^fnc_[a-f0-9]{16}$/.test(functionID) ||
    !checks.includes('ui_stop_clears_old_runtime_health')))
    throw new Error('Original stopped monitoring checkpoint required');
  async function save() {
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: project,
        branch_id: branch,
        writer_id: writer,
        function_id: functionID,
        operations,
        checks,
        production_qualified: false,
        automatic_zero_tested: false,
        credentials_in_evidence: false,
      }) + '\n',
    );
  }
  async function record(name: string) {
    checks.push(name);
    await save();
  }
  async function wait(r: Response) {
    expect(r.status()).toBe(202);
    const accepted = await r.json();
    operations.push(accepted.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const response = await context.request.get(
            `/api/v1/projects/${project}/operations/${accepted.operation.id}`,
          );
          expect(response.status()).toBe(200);
          const op = await response.json();
          if (op.state === 'failed' || op.state === 'cancelled')
            throw new Error(`Original monitoring operation failed: ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    return accepted;
  }
  async function observe() {
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/observe/runtime' && r.request().method() === 'GET',
    );
    await page.getByRole('button', { name: '观察运行状态', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(200);
    expect(r.headers()['cache-control']).toBe('no-store');
    return r.json();
  }
  async function action(name: string, suffix: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/observe' + suffix && r.request().method() === method,
    );
    await page.getByRole('button', { name, exact: true }).click();
    return wait(await pending);
  }
  async function zero() {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    const pending = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${writer}/suspend`) && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await wait(await pending);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
  }
  await mkdir(privateDir, { recursive: true });
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(adminFile, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    if (!fixture.resume) {
    // One explicit retained-data observation, not a repeat of the native write.
    await page.goto(`/#/projects/${project}/query`);
    await page.getByLabel('目标数据库', { exact: true }).fill('fn_native');
    await page.getByLabel('SQL 查询').fill('SELECT count(*)::integer FROM app.function_probe');
    await page.locator('input[type=password]').fill(password);
    const query = page.waitForResponse(
      (r) => r.url().endsWith(`/endpoints/${writer}/query`) && r.request().method() === 'POST',
      { timeout: 190000 },
    );
    await page.getByRole('button', { name: '▶ 执行 SQL', exact: true }).click();
    const q = await query;
    expect(q.status()).toBe(200);
    expect((await q.json()).rows).toEqual([[1]]);
    await zero();
    await record('retained_original_database_and_writer_zero_before_observation');
    await page.goto(`/#/projects/${project}/functions`);
    await page.getByLabel('函数名称', { exact: true }).fill('observe');
    await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
    await page.getByLabel('授权 Schema', { exact: true }).fill('app');
    const create = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
    functionID = (await wait(await create)).function_id;
    await expect(page.getByRole('button', { name: '查看函数 observe', exact: true })).toBeVisible();
    const before = await observe();
    expect(before.observed).toBe(false);
    expect(before.observation_source).toBe('metadata_only');
    expect(before.node_ready).toBeUndefined();
    await record('ui_unpublished_observation_has_no_guessed_health_or_sql_wake');
    await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zipFile);
    await page.getByLabel('环境变量 JSON', { exact: true }).fill('{"APP_MESSAGE":"monitoring"}');
    await action('部署函数版本', '/deployments');
    await expect(page.getByRole('button', { name: '调用函数', exact: true })).toBeEnabled({
      timeout: 30000,
    });
    const active = await observe();
    expect(active.function_id).toBe(functionID);
    expect(active.observed).toBe(true);
    expect(active.observation_source).toBe('authenticated_supervisor');
    expect(active.node_ready).toBe(true);
    expect(active.draining).toBe(false);
    expect(active.active_invocations).toBe(0);
    expect(active.background_counters_trusted).toBe(false);
    expect(active.instance_id).toMatch(/^fni_[a-f0-9]{16}$/);
    expect(active.vm_phase).toBe('Running');
    await expect(page.getByTestId('function-runtime-ready')).toHaveText('已认证 · 就绪');
    await record('ui_original_uid_boot_tls_hmac_runtime_observation');
    const repeated = await observe();
    expect(repeated.instance_id).toBe(active.instance_id);
    expect(repeated.last_activity).toBe(active.last_activity);
    expect(repeated.generation_token).toBe(active.generation_token);
    await record('management_observation_does_not_count_as_function_activity');
    await page.getByLabel('调用路径').fill('/');
    const invoke = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === base + '/observe/console-invoke' &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '调用函数', exact: true }).click();
    const invoked = await invoke;
    expect(invoked.status()).toBe(200);
    const body = await invoked.json();
    expect(body.status_code).toBe(200);
    const data = JSON.parse(Buffer.from(body.body, 'base64').toString('utf8'));
    expect(data.rows).toBe(1);
    expect(data.role).toBe('fn_' + functionID.slice(4));
    expect(data.message).toBe('monitoring');
    const after = await observe();
    expect(Date.parse(after.last_activity)).toBeGreaterThan(Date.parse(active.last_activity));
    expect(after.active_invocations).toBe(0);
    expect(after.definition_version_token).toBe(active.definition_version_token);
    await record('actual_read_invocation_updates_activity_without_rewriting_definition');
    await action('停止函数实例', '/suspend');
    const stopped = await observe();
    expect(stopped.observed).toBe(false);
    expect(stopped.observation_source).toBe('metadata_only');
    expect(stopped.active_invocations).toBeUndefined();
    await record('ui_stop_clears_old_runtime_health');
    await zero();
    } else {
      const original = await context.request.get(base + '/observe');
      expect(original.status()).toBe(200);
      const definition = await original.json();
      expect(definition.id).toBe(functionID);
      expect(definition.state).toBe('suspended');
      const originalEndpoint = await context.request.get(`/api/v1/projects/${project}/endpoints/${writer}`);
      expect(originalEndpoint.status()).toBe(200);
      expect((await originalEndpoint.json()).observed_state).toBe('suspended');
      await record('original_stopped_checkpoint_resumed_without_replaying_invocation');
    }
    await page.goto(`/#/projects/${project}/functions`);
    await page.getByRole('button', { name: '查看函数 observe', exact: true }).click();
    await observe();
    const endpoint = await context.request.get(`/api/v1/projects/${project}/endpoints/${writer}`);
    expect(endpoint.status()).toBe(200);
    expect((await endpoint.json()).observed_state).toBe('suspended');
    await record('suspended_runtime_observation_does_not_wake_sql');
    await action('删除函数', '', 'DELETE');
    await zero();
    await record('owned_monitoring_function_retired_and_compute_zero');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
