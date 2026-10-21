import { expect, test, type APIResponse, type Response } from '@playwright/test';
import { readFile, writeFile } from 'node:fs/promises';

test('Object upload events use native SQL deduplication and retain explicit redrive receipts', async ({
  page,
  context,
}, info) => {
  const fixtureFile = process.env.NEON_E2E_FUNCTIONS_MONITOR_FIXTURE,
    admin = process.env.NEON_E2E_ADMIN_PASSWORD_FILE,
    zip = process.env.NEON_E2E_FUNCTION_ZIP;
  if (!fixtureFile || !admin || !zip)
    throw Error('Protected retained owner and sealed Linux bundle required');
  const fixture = JSON.parse(await readFile(fixtureFile, 'utf8'));
  const { project_id: project, branch_id: branch, writer_id: writer } = fixture;
  if (
    !/^prj_[a-f0-9]{16}$/.test(project) ||
    !/^br_[a-f0-9]{16}$/.test(branch) ||
    !/^ep_[a-f0-9]{16}$/.test(writer)
  )
    throw Error('Exact retained owner required');
  const base = `/api/v1/projects/${project}/branches/${branch}`,
    slug = 'eventprobe54',
    fn = base + '/functions/' + slug,
    triggers = base + '/functions-triggers';
  type Invocation = {
    id: string;
    state: string;
    attempts: number;
    application_status: number | null;
    object_event_id: string;
    parent_invocation_id: string | null;
    logical_invocation_id: string;
    object: { object_key: string };
    redrive_succeeded: boolean;
  };
  const resume = fixture.object_events_resume;
  const checks: string[] = [...(resume?.checks || [])],
    operations: string[] = [...(resume?.operations || [])];
  let result = 'running',
    functionID = resume?.function_id || '',
    triggerID = resume?.trigger_id || '',
    publicURL = resume?.public_url || '',
    bucket = resume?.bucket || 'ci-object-events-' + Date.now(),
    originalStorage = resume?.original_storage || null;
  const uploaded: string[] = [...(resume?.uploaded || [])];
  let failedReceipt: Invocation | null = resume?.failed_receipt || null;
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
        function_id: functionID,
        trigger_id: triggerID,
        slug,
        public_url: publicURL,
        bucket,
        uploaded,
        original_storage: originalStorage,
        failed_receipt: failedReceipt,
        credentials_in_evidence: false,
        performance_reliability_campaign: false,
      }) + '\n',
    );
  }
  async function operation(response: APIResponse | Response) {
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
          if (['failed', 'cancelled'].includes(op.state))
            throw Error(`Original operation ${op.id} ${op.error_code}`);
          return op.state;
        },
        { timeout: 490000, intervals: [2500, 5000] },
      )
      .toBe('succeeded');
  }
  async function get(path: string) {
    const r = await context.request.get(path);
    expect(r.status()).toBe(200);
    return r.json();
  }
  async function action(path: string, button: string, method = 'POST') {
    const pending = page.waitForResponse(
      (r) => new URL(r.url()).pathname === path && r.request().method() === method,
      { timeout: 210000 },
    );
    await page.getByRole('button', { name: button, exact: true }).click();
    return pending;
  }
  async function functionsPage() {
    await page.goto(`/#/projects/${project}/functions`);
    await action(base + '/functions', '刷新函数状态', 'GET');
    await page.getByRole('button', { name: '查看函数 ' + slug, exact: true }).click();
  }
  async function selectTrigger() {
    await functionsPage();
    await page.getByRole('button', { name: '管理对象触发器 object-events54', exact: true }).click();
  }
  async function invocations(): Promise<Invocation[]> {
    return (await get(`${triggers}/${triggerID}/invocations?limit=100`)).items;
  }
  async function waitObject(key: string, state: string) {
    await expect
      .poll(
        async () => {
          const rows = await invocations();
          const row = rows.find((v) => v.object?.object_key === key && !v.parent_invocation_id);
          return row?.state;
        },
        { timeout: 240000, intervals: [2000, 4000] },
      )
      .toBe(state);
    return (await invocations()).find(
      (v) => v.object?.object_key === key && !v.parent_invocation_id,
    )!;
  }
  async function upload(key: string) {
    await page.goto(`/#/projects/${project}/storage`);
    await page.getByRole('button', { name: '加载存储桶', exact: true }).click();
    await page
      .getByRole('row')
      .filter({ hasText: bucket })
      .getByRole('button', { name: '选择', exact: true })
      .click();
    await page.getByLabel('对象键', { exact: true }).fill(key);
    await page.getByLabel('上传文件', { exact: true }).setInputFiles({
      name: 'event.txt',
      mimeType: 'text/plain',
      buffer: Buffer.from('native object event bytes 中文\n'),
    });
    const r = await action(base + `/storage/buckets/${bucket}/objects`, '上传对象', 'PUT');
    expect(r.status()).toBe(200);
    uploaded.push(key);
    await save();
  }
  async function proof(id: string, attempts: number) {
    const r = await context.request.get(publicURL + '/receipts?trigger=' + triggerID, {
      timeout: 210000,
    });
    expect(r.status()).toBe(200);
    const rows: string[] = (await r.json()).receipts;
    const prefix = 'object54:' + triggerID + ':' + id + ':';
    expect(rows.filter((v) => v === prefix + 'effect')).toHaveLength(1);
    expect(rows.filter((v) => v.startsWith(prefix + 'attempt:'))).toHaveLength(attempts);
  }
  try {
    await save();
    await page.goto('/#/projects');
    await page.getByLabel('用户名').fill('admin');
    await page.getByLabel('密码', { exact: true }).fill((await readFile(admin, 'utf8')).trim());
    await page.getByRole('button', { name: '登录控制台 →', exact: true }).click();
    await expect(page.getByRole('button', { name: '＋ 创建项目', exact: true })).toBeVisible();
    page.on('dialog', (d) => void d.accept());
    if (!functionID) {
      await page.goto(`/#/projects/${project}/functions`);
      await page.getByLabel('函数名称', { exact: true }).fill(slug);
      await page.getByLabel('函数数据库', { exact: true }).fill('fn_native');
      await page.getByLabel('授权 Schema', { exact: true }).fill('app');
      const r = await action(base + '/functions', '创建函数定义');
      functionID = (await r.json()).function_id;
      await save();
      await operation(r);
      await functionsPage();
      await page.getByLabel('函数 ZIP', { exact: true }).setInputFiles(zip);
      await operation(await action(fn + '/deployments', '部署函数版本'));
      await save(
        'ui_original_function_deployed_with_existing_pinned_guest_and_restricted_sql_role',
      );
    } else expect((await get(fn)).id).toBe(functionID);
    if (!checks.includes('ui_public_policy_and_original_storage_enabled')) {
      await functionsPage();
      if (!(await get(fn)).public_enabled) {
        await page.getByLabel('公开 HTTP 入口', { exact: true }).check();
        await page.getByLabel('自动休眠唤醒', { exact: true }).check();
        await page.getByLabel('函数空闲秒数', { exact: true }).fill('30');
        await page.getByLabel('确认公开访问与后台回收', { exact: true }).check();
        expect((await action(fn + '/http-policy', '保存入口策略', 'PATCH')).status()).toBe(200);
      }
      publicURL = (await get(fn)).invocation_url;
      originalStorage = await get(base + '/storage');
      await save();
      expect(['disabled', 'active']).toContain(originalStorage.state);
      if (originalStorage.state === 'disabled') {
        await page.goto(`/#/projects/${project}/storage`);
        await operation(await action(base + '/storage', '启用对象存储'));
      }
      await save('ui_public_policy_and_original_storage_enabled');
    }
    if (!checks.includes('ui_bucket_and_paused_trigger_created_with_exact_prefix')) {
      await page.goto(`/#/projects/${project}/storage`);
      await page.getByLabel('存储桶名称', { exact: true }).fill(bucket);
      expect((await action(base + '/storage/buckets', '创建存储桶')).status()).toBe(201);
      await functionsPage();
      await page.getByLabel('函数触发类型', { exact: true }).selectOption('storage_object_created');
      await page.getByLabel('对象触发器名称', { exact: true }).fill('object-events54');
      await page.getByLabel('对象触发桶', { exact: true }).fill(bucket);
      await page.getByLabel('对象触发前缀', { exact: true }).fill('uploads/');
      await page.getByLabel('对象触发函数路径', { exact: true }).fill('/uploaded');
      const created = await action(triggers, '保存暂停对象触发器');
      expect(created.status()).toBe(201);
      triggerID = (await created.json()).trigger.trigger_id;
      await save('ui_bucket_and_paused_trigger_created_with_exact_prefix');
      await selectTrigger();
      await expect(
        page.getByRole('button', { name: '保存并启用对象触发器', exact: true }),
      ).toBeDisabled();
      await page.getByLabel('确认对象事件可能重复送达', { exact: true }).check();
      expect(
        (await action(triggers + '/' + triggerID, '保存并启用对象触发器', 'PATCH')).status(),
      ).toBe(200);
    }
    if (!checks.includes('actual_prefix_filter_and_automatic_retry_one_logical_sql_effect')) {
      if (!uploaded.includes('outside.txt')) await upload('outside.txt');
      if (!uploaded.includes('uploads/once.txt')) await upload('uploads/once.txt');
      const row = await waitObject('uploads/once.txt', 'succeeded');
      expect(row.attempts).toBe(2);
      expect(row.application_status).toBe(200);
      expect(row.logical_invocation_id).toBe(row.id);
      expect(
        (await invocations()).filter((v) => v.object?.object_key === 'outside.txt'),
      ).toHaveLength(0);
      await proof(row.id, 2);
      const forged = await context.request.post(publicURL + '/uploaded', {
        headers: { 'X-Neon-Trigger-Invocation-Id': row.id },
        data: {
          version: 1,
          invocation_id: row.id,
          trigger: { type: 'storage_object_created', id: triggerID },
          data: { object_key: 'uploads/once.txt' },
        },
        timeout: 210000,
      });
      expect(forged.status()).toBe(403);
      await proof(row.id, 2);
      await save('actual_prefix_filter_and_automatic_retry_one_logical_sql_effect');
    }
    if (!checks.includes('three_failed_deliveries_preserved_as_terminal_receipt')) {
      if (!uploaded.includes('uploads/redrive.txt')) await upload('uploads/redrive.txt');
      failedReceipt = await waitObject('uploads/redrive.txt', 'failed');
      expect(failedReceipt.attempts).toBe(3);
      expect(failedReceipt.application_status).toBe(503);
      await proof(failedReceipt.id, 3);
      await save('three_failed_deliveries_preserved_as_terminal_receipt');
    }
    if (
      !checks.includes(
        'explicit_ui_redrive_lost_reply_same_key_retains_original_and_deduplicates_sql',
      )
    ) {
      if (!failedReceipt) throw Error('Original terminal receipt required');
      await selectTrigger();
      const retryPath = `${triggers}/${triggerID}/invocations/${failedReceipt.id}/retry`;
      let saved: {
        key: string;
        version: string;
        body: string;
        receipt: { invocation_id: string; logical_invocation_id: string };
      } | null = null;
      await page.route(
        '**' + retryPath,
        async (route) => {
          const r = await route.fetch();
          expect(r.status()).toBe(202);
          saved = {
            key: route.request().headers()['idempotency-key'],
            version: route.request().headers()['if-match'],
            body: route.request().postData()!,
            receipt: await r.json(),
          };
          await route.abort('failed');
        },
        { times: 1 },
      );
      await page.getByRole('button', { name: '重投递对象事件', exact: true }).click();
      await expect(page.getByRole('button', { name: '重试原对象请求', exact: true })).toBeVisible();
      if (!saved) throw Error('Original accepted redrive missing');
      const original = saved as {
        key: string;
        version: string;
        body: string;
        receipt: { invocation_id: string; logical_invocation_id: string };
      };
      expect(original.receipt.logical_invocation_id).toBe(failedReceipt.id);
      const replay = page.waitForRequest((r) => new URL(r.url()).pathname === retryPath);
      const response = page.waitForResponse((r) => new URL(r.url()).pathname === retryPath);
      await page.getByRole('button', { name: '重试原对象请求', exact: true }).click();
      expect((await response).status()).toBe(202);
      const request = await replay;
      expect(request.headers()['idempotency-key']).toBe(original.key);
      expect(request.headers()['if-match']).toBe(original.version);
      expect(request.postData()).toBe(original.body);
      await expect
        .poll(
          async () => {
            const row = (await invocations()).find((v) => v.id === original.receipt.invocation_id);
            return row?.state;
          },
          { timeout: 240000, intervals: [2000, 4000] },
        )
        .toBe('succeeded');
      const rows = await invocations(),
        parent = rows.find((v) => v.id === failedReceipt!.id)!;
      expect(parent.state).toBe('failed');
      expect(parent.attempts).toBe(3);
      expect(parent.object_event_id).toBe(failedReceipt.object_event_id);
      expect(parent.redrive_succeeded).toBe(true);
      const child = rows.find((v) => v.id === original.receipt.invocation_id)!;
      expect(child.parent_invocation_id).toBe(parent.id);
      expect(child.logical_invocation_id).toBe(parent.id);
      expect(child.attempts).toBe(1);
      await proof(parent.id, 4);
      await save('explicit_ui_redrive_lost_reply_same_key_retains_original_and_deduplicates_sql');
    }
    await selectTrigger();
    await page.getByRole('button', { name: '刷新对象调用', exact: true }).click();
    await expect(page.getByRole('region', { name: '对象调用记录', exact: true })).toContainText(
      '已重投递完成',
    );
    await expect(page.getByRole('button', { name: '重投递对象事件', exact: true })).toHaveCount(0);
    expect((await action(triggers + '/' + triggerID, '保存暂停对象触发器', 'PATCH')).status()).toBe(
      200,
    );
    await selectTrigger();
    expect((await action(triggers + '/' + triggerID, '删除对象触发器', 'DELETE')).status()).toBe(
      204,
    );
    await functionsPage();
    if ((await get(fn)).state === 'active')
      await operation(await action(fn + '/suspend', '停止函数实例'));
    await functionsPage();
    await operation(await action(fn, '删除函数', 'DELETE'));
    if (originalStorage.state === 'disabled') {
      await page.goto(`/#/projects/${project}/storage`);
      await operation(await action(base + '/storage', '禁用对象存储', 'DELETE'));
      expect((await get(base + '/storage')).state).toBe('disabled');
    }
    await page.goto(`/#/projects/${project}/compute`);
    const endpoint = await get(`/api/v1/projects/${project}/endpoints/${writer}`);
    if (endpoint.state === 'active')
      await operation(
        await action(`/api/v1/projects/${project}/endpoints/${writer}/suspend`, '现在缩到 0'),
      );
    await save(
      'ui_trigger_deleted_function_retired_original_storage_state_and_writer_zero_restored_data_retained',
    );
    result = 'pass';
    await save();
  } catch (error) {
    result = 'fail';
    await save();
    throw error;
  }
});
