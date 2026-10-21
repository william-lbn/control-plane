import { expect, test, type Response } from '@playwright/test';
import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { unzipSync } from 'fflate';

test.use({ actionTimeout: 30_000 });

test('Unhealthy Function: UI trusted observation, automatic original retirement and retained cold wake', async ({
  page,
  context,
}, info) => {
  const file = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    zip = process.env.NEON_E2E_FUNCTION_ZIP,
    privateDir = process.env.NEON_E2E_PRIVATE_DIR;
  if (!file || !admin || !zip || !privateDir)
    throw new Error('Protected Linux fixture and sealed unhealthy-idle example required');
  const privateRoot = privateDir,
    fixture = JSON.parse(await readFile(file, 'utf8')) as {
      project_id: string;
      branch_id: string;
      writer_id: string;
      unhealthy_resume?: {
        function_id: string;
        slug: string;
        public_url: string;
        instance_id: string;
        checks: string[];
        operations: string[];
        original_failure_job: string;
      };
    };
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(branch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw new Error('Original owned fixture scope required');
  const bundle = await readFile(zip),
    expectedEntry = await readFile(
      new URL('../../examples/functions/unhealthy-idle/index.mjs', import.meta.url),
    );
  expect(Buffer.from(unzipSync(bundle)['index.mjs'])).toEqual(expectedEntry);
  const bundleDigest = createHash('sha256').update(bundle).digest('hex'),
    resume = fixture.unhealthy_resume,
    slug = resume?.slug ?? 'idleprobev1',
    base = `/api/v1/projects/${project}/branches/${branch}/functions`,
    target = base + '/' + slug,
    checks = [...(resume?.checks ?? [])],
    operations = [...(resume?.operations ?? [])];
  let result = 'running',
    functionID = resume?.function_id ?? '',
    publicURL = resume?.public_url ?? '',
    instanceID = resume?.instance_id ?? '';
  if (
    resume &&
    (!/^fnc_[a-f0-9]{16}$/.test(resume.function_id) ||
      resume.slug !== 'idleprobev1' ||
      !/^publication-ui-[0-9]{14}$/.test(resume.original_failure_job))
  )
    throw new Error('Exact original unhealthy Function checkpoint required');
  async function save(check?: string) {
    if (check && !checks.includes(check)) checks.push(check);
    const receipt = {
      result,
      project_id: project,
      branch_id: branch,
      writer_id: writer,
      function_id: functionID,
      slug,
      public_url: publicURL,
      instance_id: instanceID,
      bundle_sha256: bundleDigest,
      checks,
      operations,
      production_qualified: false,
      credentials_in_evidence: false,
      ...(resume ? { original_failure_job: resume.original_failure_job } : {}),
    };
    await writeFile(info.outputPath('result.json'), JSON.stringify(receipt) + '\n');
    await writeFile(
      privateRoot + '/unhealthy-idle-checkpoint.json',
      JSON.stringify(receipt) + '\n',
      { mode: 0o600 },
    );
  }
  async function metadata() {
    const r = await context.request.get(target);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function verifyPublishedBundle() {
    const current = await metadata(),
      r = await context.request.get(target + '/deployments');
    expect(r.status()).toBe(200);
    const version = (await r.json()).items.find(
      (v: { id: string }) => v.id === current.active_deployment_id,
    );
    expect(version?.bundle_digest).toBe(bundleDigest);
  }
  async function observeOperation(id: string) {
    if (!operations.includes(id)) operations.push(id);
    await save();
    await expect
      .poll(
        async () => {
          const r = await context.request.get(`/api/v1/projects/${project}/operations/${id}`);
          expect(r.status()).toBe(200);
          const op = await r.json();
          if (['failed', 'cancelled'].includes(op.state))
            throw new Error(`Original operation ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function wait(r: Response) {
    expect(r.status()).toBe(202);
    const accepted = await r.json();
    await observeOperation(accepted.operation.id);
    return accepted;
  }
  async function action(button: string, suffix: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === target + suffix && r.request().method() === method,
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function refresh() {
    await page.goto(`/#/projects/${project}/functions`);
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === base && r.request().method() === 'GET',
    );
    await page.getByRole('button', { name: '刷新函数状态', exact: true }).click();
    expect((await pending).status()).toBe(200);
    await page.getByRole('button', { name: '查看函数 ' + slug, exact: true }).click();
  }
  async function uiRuntime() {
    const pending = page.waitForResponse((r) => new URL(r.url()).pathname === target + '/runtime');
    await page.getByRole('button', { name: '观察运行状态', exact: true }).click();
    const r = await pending;
    expect(r.status()).toBe(200);
    return r.json();
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (dialog) => void dialog.accept());
    if (!checks.includes('owned_function_created')) {
      await page.goto(`/#/projects/${project}/functions`);
      await page.getByLabel('函数名称', { exact: true }).fill(slug);
      await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
      await page.getByLabel('授权 Schema', { exact: true }).fill('app');
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === base && r.request().method() === 'POST',
      );
      await page.getByRole('button', { name: '创建函数定义', exact: true }).click();
      functionID = (await wait(await pending)).function_id;
      await save('owned_function_created');
    }
    if (!checks.includes('sealed_bundle_deployed_original_healthy_instance_recorded')) {
      await refresh();
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await page
        .getByLabel('环境变量 JSON', { exact: true })
        .fill('{"APP_MESSAGE":"retained-unhealthy-idle-v1"}');
      await wait(await action('部署函数版本', '/deployments'));
      await refresh();
      const runtime = await uiRuntime();
      expect(runtime.node_ready).toBe(true);
      instanceID = runtime.instance_id;
      expect(instanceID).toBeTruthy();
      await save('sealed_bundle_deployed_original_healthy_instance_recorded');
    }
    if (!checks.includes('ui_public_idle_policy_enabled')) {
      await refresh();
      await page.getByLabel('公开 HTTP 入口', { exact: true }).check();
      await page.getByLabel('自动休眠唤醒', { exact: true }).check();
      await page.getByLabel('函数空闲秒数', { exact: true }).fill('60');
      await page.getByLabel('确认公开访问与后台回收', { exact: true }).check();
      const pending = page.waitForResponse(
        (r) => new URL(r.url()).pathname === target + '/http-policy',
      );
      await page.getByRole('button', { name: '保存入口策略', exact: true }).click();
      expect((await pending).status()).toBe(200);
      publicURL = (await metadata()).invocation_url;
      expect(publicURL).toBeTruthy();
      await save('ui_public_idle_policy_enabled');
    }
    if (!checks.includes('single_customer_response_then_original_node_unready_no_active_call')) {
      await verifyPublishedBundle();
      const exit = await context.request.post(publicURL + '/exit-after-response', {
        data: {},
        timeout: 210000,
      });
      expect(exit.status()).toBe(200);
      expect(await exit.json()).toEqual({
        accepted: true,
        action: 'exit-owned-test-node',
        marker: 'retained-unhealthy-idle-v1',
      });
      await new Promise((resolve) => setTimeout(resolve, 2500));
      await refresh();
      const runtime = await uiRuntime();
      expect(runtime.instance_id).toBe(instanceID);
      expect(runtime.node_ready).toBe(false);
      expect(runtime.active_invocations).toBe(0);
      expect(runtime.observation_source).toBe('authenticated_supervisor');
      await expect(page.getByTestId('function-runtime-ready')).toHaveText('已认证 · 未就绪');
      await save('single_customer_response_then_original_node_unready_no_active_call');
    }
    if (!checks.includes('automatic_stop_original_operation_completed_and_ui_metadata_only')) {
      await expect
        .poll(
          async () => {
            const state = await metadata();
            expect(state.id).toBe(functionID);
            return state.state;
          },
          { timeout: 210000, intervals: [2500, 5000] },
        )
        .toBe('suspended');
      expect((await metadata()).idle_suspended).toBe(true);
      const r = await context.request.get(`/api/v1/projects/${project}/operations?limit=100`);
      expect(r.status()).toBe(200);
      const original = (await r.json()).items.filter(
        (op: { resource_id: string; action: string }) =>
          op.resource_id === functionID && op.action === 'suspend_function',
      );
      expect(original).toHaveLength(1);
      await observeOperation(original[0].id);
      await refresh();
      const stopped = await uiRuntime();
      expect(stopped.observed).toBe(false);
      expect(stopped.observation_source).toBe('metadata_only');
      await save('automatic_stop_original_operation_completed_and_ui_metadata_only');
    }
    if (!checks.includes('one_public_request_cold_wakes_same_definition_bundle_and_environment')) {
      const r = await context.request.get(publicURL + '/health', { timeout: 210000 });
      expect(r.status()).toBe(200);
      expect(await r.json()).toEqual({ ready: true, marker: 'retained-unhealthy-idle-v1' });
      await refresh();
      const runtime = await uiRuntime();
      expect(runtime.node_ready).toBe(true);
      expect(runtime.instance_id).not.toBe(instanceID);
      expect((await metadata()).id).toBe(functionID);
      await verifyPublishedBundle();
      await save('one_public_request_cold_wakes_same_definition_bundle_and_environment');
    }
    if (!checks.includes('original_function_normally_retired_backings_preserved_writer_zero')) {
      await refresh();
      await wait(await action('停止函数实例', '/suspend'));
      await refresh();
      await wait(await action('删除函数', '', 'DELETE'));
      await page.goto(`/#/projects/${project}/compute`);
      await page.locator('select').first().selectOption(writer);
      const r = await context.request.get(`/api/v1/projects/${project}/endpoints/${writer}`);
      expect(r.status()).toBe(200);
      if ((await r.json()).observed_state !== 'suspended') {
        const pending = page.waitForResponse(
          (r) =>
            new URL(r.url()).pathname ===
              `/api/v1/projects/${project}/endpoints/${writer}/suspend` &&
            r.request().method() === 'POST',
        );
        await page.getByRole('button', { name: '现在缩到 0', exact: true }).click();
        await wait(await pending);
      }
      await expect(page.locator('.stats-grid .stat-card strong').first()).toHaveText('已休眠', {
        timeout: 150000,
      });
      await save('original_function_normally_retired_backings_preserved_writer_zero');
    }
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
