import { expect, test, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test.use({ actionTimeout: 30_000 });
test('Functions history restores actual original code and environment, preserves SQL writes and new boot lineage', async ({
  page,
  context,
}, info) => {
  const input = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE;
  const zip = process.env.NEON_E2E_FUNCTION_ZIP,
    replacement = process.env.NEON_E2E_FUNCTION_ROLLBACK_ZIP;
  if (!input || !admin || !zip || !replacement)
    throw new Error('Protected owner and two Linux example archives required');
  const fixture = JSON.parse(await readFile(input, 'utf8')) as {
    project_id: string;
    branch_id: string;
    writer_id: string;
    resume?: {
      phase: 'original_unready_no_write';
      function_id: string;
      first_deployment_id: string;
      operations: string[];
      original_failure_job: string;
    };
  };
  for (const [key, prefix] of [
    ['project_id', 'prj'],
    ['branch_id', 'br'],
    ['writer_id', 'ep'],
  ] as const)
    if (!new RegExp(`^${prefix}_[a-f0-9]{16}$`).test(fixture[key]))
      throw new Error('Exact accepted owner fixture required');
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  const base = `/api/v1/projects/${project}/branches/${branch}/functions`,
    path = base + '/roll';
  const checks: string[] = [],
    operations: string[] = [];
  let result = 'running',
    functionID = '',
    firstID = '',
    secondID = '';
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
        first_deployment_id: firstID,
        second_deployment_id: secondID,
        operations,
        checks,
        production_qualified: false,
        credentials_in_evidence: false,
      }) + '\n',
    );
  }
  async function wait(response: Response) {
    expect(response.status()).toBe(202);
    const accepted = await response.json();
    operations.push(accepted.operation.id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(
            `/api/v1/projects/${project}/operations/${accepted.operation.id}`,
          );
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (op.state === 'failed' || op.state === 'cancelled')
            throw new Error(`Original Operation failed ${op.id} ${op.error_code}`);
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
  async function invoke(method = 'GET', invocationPath = '/', body = '') {
    await page.getByLabel('调用方法', { exact: true }).selectOption(method);
    await page.getByLabel('调用路径', { exact: true }).fill(invocationPath);
    await page.getByLabel('调用请求体', { exact: true }).fill(body);
    const response = await action('调用函数', '/console-invoke');
    expect(response.status()).toBe(200);
    const envelope = await response.json();
    expect(envelope.status_code).toBe(200);
    expect(envelope.body_encoding).toBe('base64');
    return JSON.parse(Buffer.from(envelope.body, 'base64').toString('utf8'));
  }
  async function observe() {
    const pending = page.waitForResponse((r) => new URL(r.url()).pathname === path + '/runtime');
    await page.getByRole('button', { name: '观察运行状态', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(200);
    const data = await r.json();
    expect(data.node_ready).toBe(true);
    return data;
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
    if (fixture.resume) {
      const original = fixture.resume;
      if (
        original.phase !== 'original_unready_no_write' ||
        !/^fnc_[a-f0-9]{16}$/.test(original.function_id) ||
        !/^fdp_[a-f0-9]{16}$/.test(original.first_deployment_id) ||
        original.operations.length !== 2 ||
        !/^publication-ui-[0-9]{14}$/.test(original.original_failure_job)
      )
        throw new Error('Exact original no-write checkpoint required');
      functionID = original.function_id;
      const definition = await context.request.get(path);
      expect(definition.status()).toBe(200);
      const value = await definition.json();
      expect(value.id).toBe(functionID);
      expect(value.state).toBe('active');
      expect(value.active_deployment_id).toBe(original.first_deployment_id);
      for (const [index, id] of original.operations.entries()) {
        if (!/^op_[a-f0-9]{16,24}$/.test(id)) throw new Error('Original Operation ID required');
        const response = await context.request.get(`/api/v1/projects/${project}/operations/${id}`);
        expect(response.status()).toBe(200);
        const op = await response.json();
        expect(op.state).toBe('succeeded');
        expect(op.resource_id).toBe(functionID);
        expect(op.action).toBe(index === 0 ? 'create_function' : 'deploy_function');
      }
      operations.push(...original.operations);
      await page.getByRole('button', { name: '查看函数 roll', exact: true }).click();
      await save('original_completed_definition_and_deployment_verified_before_continuation');
      // Original pg idle-session error was classified from the retained guest
      // log before this continuation. Retire that exact guest normally; update
      // only the example bundle, preserving the definition, SQL identity/data.
      await wait(await action('停止函数实例', '/suspend'));
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await page
        .getByLabel('环境变量 JSON', { exact: true })
        .fill('{"APP_MESSAGE":"rollback-original","APP_IDLE_SESSION_PROBE":"1s"}');
      await wait(await action('部署函数版本', '/deployments'));
      await save('original_unready_guest_normally_retired_and_corrected_sdk_deployed');
    } else {
      await page.getByLabel('函数名称', { exact: true }).fill('roll');
      await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
      await page.getByLabel('授权 Schema', { exact: true }).fill('app');
      const create = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
      functionID = (await wait(await create)).function_id;
      await page.getByRole('button', { name: '查看函数 roll', exact: true }).click();
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await page
        .getByLabel('环境变量 JSON', { exact: true })
        .fill('{"APP_MESSAGE":"rollback-original","APP_IDLE_SESSION_PROBE":"1s"}');
      await wait(await action('部署函数版本', '/deployments'));
    }
    await expect(page.getByRole('button', { name: '调用函数', exact: true })).toBeEnabled();
    const firstBoot = await observe();
    firstID = firstBoot.deployment_id;
    if (fixture.resume) expect(firstID).not.toBe(fixture.resume.first_deployment_id);
    const before = await invoke();
    expect(before.message).toBe('rollback-original');
    expect(before.rows).toBe(1);
    const written = await invoke('POST', '/write', '{"marker":"rollback-kept"}');
    expect(written.rows).toBe(2);
    expect(written.role).toBe(before.role);
    await save('original_ui_version_real_sql_write_and_authenticated_boot');
    await page.waitForTimeout(15_000);
    const afterIdle = await invoke();
    expect(afterIdle.rows).toBe(2);
    expect(afterIdle.role).toBe(before.role);
    expect(afterIdle.idle_client_disconnects).toBeGreaterThan(0);
    await observe();
    await save(
      'actual_idle_session_disconnect_survives_and_explicit_next_read_reconnects_without_write_replay',
    );
    await wait(await action('停止函数实例', '/suspend'));
    await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(replacement);
    await page
      .getByLabel('环境变量 JSON', { exact: true })
      .fill('{"APP_MESSAGE":"rollback-replacement"}');
    await wait(await action('部署函数版本', '/deployments'));
    await expect(page.getByRole('button', { name: '调用函数', exact: true })).toBeEnabled();
    const secondBoot = await observe();
    secondID = secondBoot.deployment_id;
    const replaced = await invoke();
    expect(replaced.code_version).toBe('replacement-v2');
    expect(replaced.message).toBe('rollback-replacement');
    const history = await context.request.get(path + '/deployments');
    expect(history.status()).toBe(200);
    const items = (await history.json()).items;
    const first = items.find((d: { id: string }) => d.id === firstID),
      second = items.find((d: { id: string }) => d.id === secondID);
    expect(first.bundle_digest).not.toBe(second.bundle_digest);
    await save('second_ui_deployment_executes_distinct_code_and_environment');
    await wait(await action('停止函数实例', '/suspend'));
    await page
      .getByLabel('环境变量 JSON', { exact: true })
      .fill('{"APP_MESSAGE":"must-not-override-history"}');
    const response = await action(`恢复部署 ${firstID}`, '/rollback');
    expect(response.request().postDataJSON()).toEqual({ deployment_id: firstID });
    const headers = await response.request().allHeaders(),
      accepted = await wait(response);
    await expect(page.getByRole('button', { name: '调用函数', exact: true })).toBeEnabled();
    const restoredBoot = await observe();
    expect(restoredBoot.instance_id).not.toBe(firstBoot.instance_id);
    expect(restoredBoot.instance_id).not.toBe(secondBoot.instance_id);
    expect(BigInt(restoredBoot.generation_token)).toBeGreaterThan(
      BigInt(secondBoot.generation_token),
    );
    const restored = await invoke();
    expect(restored.message).toBe('rollback-original');
    expect(restored.code_version).toBeUndefined();
    expect(restored.role).toBe(before.role);
    expect(restored.rows).toBe(2);
    const restoredHistory = await context.request.get(path + '/deployments');
    expect(restoredHistory.status()).toBe(200);
    const lineage = (await restoredHistory.json()).items.find(
      (d: { id: string }) => d.id === restoredBoot.deployment_id,
    );
    expect(lineage.source_deployment_id).toBe(firstID);
    expect(lineage.bundle_digest).toBe(first.bundle_digest);
    await save(
      'ui_history_restore_new_boot_actual_original_code_env_sql_identity_and_retained_write',
    );
    const replayHeaders = {
      'X-CSRF-Token': headers['x-csrf-token'],
      'Idempotency-Key': headers['idempotency-key'],
      'If-Match': headers['if-match'],
    };
    const replay = await context.request.post(path + '/rollback', {
      headers: replayHeaders,
      data: { deployment_id: firstID },
    });
    expect(replay.status()).toBe(202);
    expect((await replay.json()).operation.id).toBe(accepted.operation.id);
    expect(
      (
        await context.request.post(path + '/rollback', {
          headers: replayHeaders,
          data: { deployment_id: secondID },
        })
      ).status(),
    ).toBe(409);
    expect(
      (await context.request.post(path + '/resume', { headers: replayHeaders, data: {} })).status(),
    ).toBe(409);
    expect(
      (
        await context.request.post(path + '/rollback', {
          headers: {
            ...replayHeaders,
            'Idempotency-Key': replayHeaders['Idempotency-Key'] + '-stale',
          },
          data: { deployment_id: firstID },
        })
      ).status(),
    ).toBe(412);
    await save(
      'original_rollback_operation_replay_source_conflict_route_conflict_and_stale_version',
    );
    await wait(await action('停止函数实例', '/suspend'));
    const definition = await context.request.get(path);
    expect(definition.status()).toBe(200);
    const current = await definition.json();
    expect(
      (
        await context.request.post(path + '/rollback', {
          headers: {
            ...replayHeaders,
            'Idempotency-Key': replayHeaders['Idempotency-Key'] + '-foreign',
            'If-Match': `"${current.version_token}"`,
          },
          data: { deployment_id: 'fdp_ffffffffffffffff' },
        })
      ).status(),
    ).toBe(404);
    await wait(await action('删除函数定义', '', 'DELETE'));
    await expect(page.getByRole('button', { name: '查看函数 roll', exact: true })).toHaveCount(0);
    await page.goto(`/#/projects/${project}/compute`);
    await page.locator('select').first().selectOption(writer);
    if (!(await page.getByRole('button', { name: '现在缩到 0', exact: true }).isDisabled())) {
      const pending = page.waitForResponse(
        (r) =>
          new URL(r.url()).pathname === `/api/v1/projects/${project}/endpoints/${writer}/suspend` &&
          r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
      await wait(await pending);
    }
    await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
      timeout: 150000,
    });
    await save(
      'unknown_history_rejected_original_guests_retired_records_and_sql_preserved_compute_zero',
    );
    result = 'pass';
    await save();
  } catch (e) {
    result = 'fail';
    await save();
    throw e;
  }
});
