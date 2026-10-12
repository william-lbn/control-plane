import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('Functions explicit wake retains source, environment, SQL identity and idempotent intent', async ({
  page,
  context,
}, info) => {
  const input = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE;
  const admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const zip = process.env.NEON_E2E_FUNCTION_ZIP;
  if (!input || !admin || !zip)
    throw new Error('Protected Linux owner fixture and example required');
  const fixture = JSON.parse(await readFile(input, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
  };
  if (
    !/^prj_[a-f0-9]{16}$/.test(fixture.project_id) ||
    !/^br_[a-f0-9]{16}$/.test(fixture.branch_id) ||
    !/^ep_[a-f0-9]{16}$/.test(fixture.writer_id)
  )
    throw new Error('Exact retained native owner scope required');
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  const base = `/api/v1/projects/${project}/branches/${branch}/functions`;
  const path = base + '/wake';
  const checks: string[] = [],
    operations: string[] = [];
  let functionID = '',
    result = 'running';
  async function save(name?: string) {
    if (name) checks.push(name);
    await writeFile(
      info.outputPath('result.json'),
      JSON.stringify({
        result,
        project_id: project,
        branch_id: branch,
        writer_id: writer,
        function_id: functionID,
        checks,
        operations,
        automatic_zero_tested: false,
        production_qualified: false,
        credentials_in_evidence: false,
      }) + '\n',
    );
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
            throw new Error(`Original wake operation failed: ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
    return accepted;
  }
  async function action(button: string, suffix: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === path + suffix && r.request().method() === method,
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function observe() {
    const pending = page.waitForResponse((r) => new URL(r.url()).pathname === path + '/runtime');
    await page.getByRole('button', { name: '观察运行状态', exact: true }).click();
    const response = await pending;
    expect(response.status()).toBe(200);
    return response.json();
  }
  async function zero() {
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    const pending = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/suspend` &&
        r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
    await wait(await pending);
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    await page.goto(`/#/projects/${project}/functions`);
    await page.getByLabel('函数名称', { exact: true }).fill('wake');
    await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
    await page.getByLabel('授权 Schema', { exact: true }).fill('app');
    const create = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
    );
    await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
    functionID = (await wait(await create)).function_id;
    await expect(page.getByRole('button', { name: '查看函数 wake', exact: true })).toBeVisible();
    await page.getByRole('button', { name: '查看函数 wake', exact: true }).click();
    await expect(page.getByRole('button', { name: '唤醒保留版本', exact: true })).toBeDisabled();
    await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
    await page.getByLabel('环境变量 JSON', { exact: true }).fill('{"APP_MESSAGE":"wake-retained"}');
    await wait(await action('部署函数版本', '/deployments'));
    await expect(page.getByRole('button', { name: '调用函数', exact: true })).toBeEnabled({
      timeout: 30000,
    });
    const original = await observe();
    expect(original.node_ready).toBe(true);
    const history = await context.request.get(path + '/deployments');
    expect(history.status()).toBe(200);
    const first = (await history.json()).items[0];
    await save('original_deployed_source_and_authenticated_boot_recorded');
    await wait(await action('停止函数实例', '/suspend'));
    await zero();
    await page.goto(`/#/projects/${project}/functions`);
    await page.getByRole('button', { name: '查看函数 wake', exact: true }).click();
    await page
      .getByLabel('环境变量 JSON', { exact: true })
      .fill('{"APP_MESSAGE":"must-not-be-applied-by-wake"}');
    const response = await action('唤醒保留版本', '/resume');
    expect(response.request().postDataJSON()).toEqual({});
    const headers = await response.request().allHeaders();
    const accepted = await wait(response);
    await expect(page.getByRole('button', { name: '调用函数', exact: true })).toBeEnabled({
      timeout: 30000,
    });
    const awake = await observe();
    expect(awake.node_ready).toBe(true);
    expect(awake.instance_id).not.toBe(original.instance_id);
    expect(awake.deployment_id).not.toBe(original.deployment_id);
    expect(BigInt(awake.generation_token)).toBeGreaterThan(BigInt(original.generation_token));
    const nextHistory = await context.request.get(path + '/deployments');
    expect(nextHistory.status()).toBe(200);
    const second = (await nextHistory.json()).items[0];
    expect(second.bundle_digest).toBe(first.bundle_digest);
    expect(second.environment_names).toEqual(first.environment_names);
    await save('ui_wake_new_boot_retains_immutable_source_and_environment');
    const replayHeaders = {
      'X-CSRF-Token': headers['x-csrf-token'],
      'Idempotency-Key': headers['idempotency-key'],
      'If-Match': headers['if-match'],
    };
    const replay = await context.request.post(path + '/resume', {
      headers: replayHeaders,
      data: {},
    });
    expect(replay.status()).toBe(202);
    expect((await replay.json()).operation.id).toBe(accepted.operation.id);
    const conflict = await context.request.post(path + '/deployments', {
      headers: replayHeaders,
      multipart: { runtime: 'nodejs24' },
    });
    expect(conflict.status()).toBe(409);
    expect((await conflict.json()).code).toBe('idempotency_conflict');
    const stale = await context.request.post(path + '/resume', {
      headers: { ...replayHeaders, 'Idempotency-Key': 'wake-stale-' + functionID },
      data: {},
    });
    expect(stale.status()).toBe(412);
    const unchanged = await observe();
    expect(unchanged.instance_id).toBe(awake.instance_id);
    expect(unchanged.generation_token).toBe(awake.generation_token);
    await save('wake_replay_same_operation_conflicting_route_and_stale_version_rejected');
    const invoked = await action('调用函数', '/console-invoke');
    expect(invoked.status()).toBe(200);
    const envelope = await invoked.json();
    expect(envelope.status_code).toBe(200);
    const data = JSON.parse(Buffer.from(envelope.body, 'base64').toString('utf8'));
    expect(data.message).toBe('wake-retained');
    expect(data.role).toBe('fn_' + functionID.slice(4));
    expect(data.rows).toBe(1);
    await save('real_neon_sql_identity_data_and_environment_preserved_after_wake');
    await wait(await action('停止函数实例', '/suspend'));
    await wait(await action('删除函数', '', 'DELETE'));
    await zero();
    await save('owned_wake_function_retired_original_database_retained_compute_zero');
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
